// Package ingest owns the asynchronous pipeline that turns raw document
// bytes into a queryable tree:
//
//	parse      — bytes → hierarchical outline (parser.Registry)
//	build tree — outline → sections persisted in Postgres + object store
//	summarize  — every section gets an LLM-written summary
//
// The pipeline is driven by a queue job of kind queue.KindIngestDocument.
// Each stage is idempotent so a retry from any point leaves the document
// in a consistent state.
package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/hallelx2/vectorless-engine/pkg/db"
	"github.com/hallelx2/llmgate"
	"github.com/hallelx2/vectorless-engine/pkg/parser"
	"github.com/hallelx2/vectorless-engine/pkg/queue"
	"github.com/hallelx2/vectorless-engine/pkg/storage"
	"github.com/hallelx2/vectorless-engine/pkg/tree"
)

// Payload is the JSON body attached to an ingest job.
type Payload struct {
	DocumentID  tree.DocumentID `json:"document_id"`
	ContentType string          `json:"content_type"`
	Filename    string          `json:"filename"`
	SourceRef   string          `json:"source_ref"` // storage key of the original bytes
}

// Pipeline runs the ingest stages.
type Pipeline struct {
	DB       *db.Pool
	Storage  storage.Storage
	LLM      llmgate.Client
	Parsers  *parser.Registry
	Logger   *slog.Logger

	// SummaryMaxChars caps the content window sent to the LLM per section.
	// Sections longer than this are truncated — we're generating a short
	// summary, not reproducing the text.
	SummaryMaxChars int

	// SummaryModel, when non-empty, overrides the LLM client's default
	// model for summarization calls.
	SummaryModel string
}

// NewPipeline returns a Pipeline with sensible defaults filled in.
func NewPipeline(p Pipeline) *Pipeline {
	if p.SummaryMaxChars == 0 {
		p.SummaryMaxChars = 4000
	}
	if p.Logger == nil {
		p.Logger = slog.Default()
	}
	return &p
}

// Handler returns a queue.Handler suitable for queue.KindIngestDocument.
func (p *Pipeline) Handler() queue.Handler {
	return func(ctx context.Context, j queue.Job) error {
		var payload Payload
		if err := json.Unmarshal(j.Payload, &payload); err != nil {
			return fmt.Errorf("decode payload: %w", err)
		}
		return p.Run(ctx, payload)
	}
}

// Run executes the full pipeline for one document. Safe to retry.
func (p *Pipeline) Run(ctx context.Context, pl Payload) error {
	log := p.Logger.With("document_id", string(pl.DocumentID))
	log.Info("ingest: start", "source_ref", pl.SourceRef)

	if err := p.DB.SetDocumentStatus(ctx, pl.DocumentID, db.StatusParsing, ""); err != nil {
		return err
	}

	parsed, err := p.parse(ctx, pl)
	if err != nil {
		p.fail(ctx, pl.DocumentID, "parse", err)
		return err
	}
	log.Info("ingest: parsed", "sections", len(parsed.Flatten()), "title", parsed.Title)

	if err := p.persistTree(ctx, pl.DocumentID, parsed); err != nil {
		p.fail(ctx, pl.DocumentID, "persist tree", err)
		return err
	}

	if err := p.DB.SetDocumentStatus(ctx, pl.DocumentID, db.StatusSummarizing, ""); err != nil {
		return err
	}
	if err := p.summarize(ctx, pl.DocumentID); err != nil {
		// Summarization failures are recoverable — a section without a
		// summary is still query-able, just less efficient. We log and
		// proceed rather than dead-letter the document.
		log.Warn("ingest: summarize had errors", "err", err)
	}

	if err := p.DB.SetDocumentStatus(ctx, pl.DocumentID, db.StatusReady, ""); err != nil {
		return err
	}
	log.Info("ingest: ready")
	return nil
}

func (p *Pipeline) parse(ctx context.Context, pl Payload) (*parser.ParsedDoc, error) {
	rc, _, err := p.Storage.Get(ctx, pl.SourceRef)
	if err != nil {
		return nil, fmt.Errorf("fetch source: %w", err)
	}
	defer rc.Close()
	return p.Parsers.Parse(ctx, pl.ContentType, pl.Filename, rc)
}

// persistTree writes sections + full content in document order. Parents
// are written before children so the FK on sections.parent_id holds.
func (p *Pipeline) persistTree(ctx context.Context, docID tree.DocumentID, doc *parser.ParsedDoc) error {
	if doc.Title != "" {
		if err := p.DB.SetDocumentTitle(ctx, docID, doc.Title); err != nil {
			return err
		}
	}

	ordinal := 0
	var walk func(secs []parser.Section, parent tree.SectionID, depth int) error
	walk = func(secs []parser.Section, parent tree.SectionID, depth int) error {
		for i, s := range secs {
			id := tree.SectionID("sec_" + uuid.New().String())
			contentKey := path.Join("documents", string(docID), "sections", string(id)+".txt")

			if strings.TrimSpace(s.Content) != "" {
				if err := p.Storage.Put(ctx, contentKey,
					bytes.NewReader([]byte(s.Content)),
					storage.Metadata{
						ContentType: "text/plain; charset=utf-8",
						Size:        int64(len(s.Content)),
					}); err != nil {
					return fmt.Errorf("store section %s: %w", id, err)
				}
			}

			if err := p.DB.UpsertSection(ctx, db.Section{
				ID:         id,
				DocumentID: docID,
				ParentID:   parent,
				Ordinal:    i,
				Depth:      depth,
				Title:      s.Title,
				ContentRef: contentKey,
				TokenCount: approxTokens(s.Content),
				Metadata:   s.Metadata,
			}); err != nil {
				return err
			}
			ordinal++

			if err := walk(s.Children, id, depth+1); err != nil {
				return err
			}
		}
		return nil
	}

	return walk(doc.Sections, "", 0)
}

// summarize walks every section that lacks a summary and asks the LLM
// for a short description of its content. Leaf sections pull content
// from storage; internal sections synthesize a summary from their
// children's titles.
//
// Non-fatal per-section errors are collected and returned joined — the
// caller decides whether to fail the whole document.
func (p *Pipeline) summarize(ctx context.Context, docID tree.DocumentID) error {
	sections, err := p.DB.ListSections(ctx, docID)
	if err != nil {
		return err
	}

	// Build a children map so internal-node summaries can lean on titles.
	children := map[tree.SectionID][]db.Section{}
	for _, s := range sections {
		if s.ParentID != "" {
			children[s.ParentID] = append(children[s.ParentID], s)
		}
	}

	var errs []error
	for _, s := range sections {
		if s.Summary != "" {
			continue
		}
		summary, err := p.summaryFor(ctx, s, children[s.ID])
		if err != nil {
			errs = append(errs, fmt.Errorf("section %s: %w", s.ID, err))
			continue
		}
		if err := p.DB.UpdateSectionSummary(ctx, s.ID, summary, s.TokenCount); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (p *Pipeline) summaryFor(ctx context.Context, s db.Section, kids []db.Section) (string, error) {
	var body string
	if len(kids) == 0 {
		// Leaf: fetch the stored text.
		if s.ContentRef == "" {
			return s.Title, nil
		}
		rc, _, err := p.Storage.Get(ctx, s.ContentRef)
		if err != nil {
			return "", err
		}
		defer rc.Close()
		raw, err := io.ReadAll(io.LimitReader(rc, int64(p.SummaryMaxChars)))
		if err != nil {
			return "", err
		}
		body = string(raw)
	} else {
		// Internal: compose from children's titles so we have SOMETHING to
		// summarize even without bringing all children content into memory.
		var b strings.Builder
		b.WriteString("This section contains:\n")
		for _, c := range kids {
			fmt.Fprintf(&b, "- %s\n", c.Title)
		}
		body = b.String()
	}

	resp, err := p.LLM.Complete(ctx, llmgate.Request{
		Model:       p.SummaryModel,
		Temperature: 0.0,
		MaxTokens:   200,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: "You write short, factual section summaries. One sentence, no preamble, no quotes."},
			{Role: llmgate.RoleUser, Content: fmt.Sprintf(
				"Summarize this section titled %q in a single sentence (max 40 words):\n\n%s",
				s.Title, body)},
		},
	})
	if err != nil {
		// Stub LLMs return ErrNotImplemented. Degrade gracefully: use a
		// truncated excerpt as the "summary" so downstream retrieval still
		// has something to reason over.
		if errors.Is(err, llmgate.ErrNotImplemented) {
			return fallbackSummary(s.Title, body), nil
		}
		return "", err
	}
	if out := strings.TrimSpace(resp.Content); out != "" {
		return out, nil
	}
	return fallbackSummary(s.Title, body), nil
}

func fallbackSummary(title, body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return title
	}
	const max = 240
	if len(body) > max {
		body = body[:max] + "…"
	}
	// Collapse internal whitespace for readability.
	return strings.Join(strings.Fields(body), " ")
}

func (p *Pipeline) fail(ctx context.Context, id tree.DocumentID, stage string, cause error) {
	msg := fmt.Sprintf("%s: %s", stage, cause.Error())
	if err := p.DB.SetDocumentStatus(ctx, id, db.StatusFailed, msg); err != nil {
		p.Logger.Error("ingest: failed to mark document failed", "err", err, "cause", cause)
	}
}

// approxTokens is a cheap 4-chars-per-token heuristic used at ingest
// time so we don't spend a provider round-trip per section just to
// populate metadata. Real token counts are reconciled when a retrieval
// strategy runs.
func approxTokens(s string) int {
	if s == "" {
		return 0
	}
	n := len(s) / 4
	if n < 1 {
		return 1
	}
	return n
}

// NewDocumentID mints a fresh document ID ("doc_<uuid>"). Exported so
// the API layer can mint one before enqueuing the ingest job.
func NewDocumentID() tree.DocumentID {
	return tree.DocumentID("doc_" + uuid.New().String())
}

// SourceKey returns the canonical storage key where an ingest payload's
// original bytes live.
func SourceKey(id tree.DocumentID, filename string) string {
	// Keep the original extension so future content-type sniffing works.
	ext := path.Ext(filename)
	return path.Join("documents", string(id), "source"+ext)
}

// DefaultRegistry returns a parser.Registry preloaded with the parsers
// the engine ships with. Callers may add more via Registry.Register.
func DefaultRegistry() *parser.Registry {
	return parser.NewRegistry(
		parser.NewMarkdown(),
		parser.NewHTML(),
		parser.NewDOCX(),
		parser.NewPDF(),
		parser.NewText(),
	)
}

// helper kept for tests — not used by the pipeline itself.
var _ = time.Now
