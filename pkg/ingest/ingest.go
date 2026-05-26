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
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/hallelx2/llmgate"

	"github.com/hallelx2/vectorless-engine/pkg/db"
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
	// Profile selects domain-aware structuring/summarization prompts
	// ("generic", "research", "medical"). Empty = generic. Sourced from
	// the document's store (the control plane injects X-Vectorless-Profile).
	Profile string `json:"profile,omitempty"`
}

// Pipeline runs the ingest stages.
type Pipeline struct {
	DB      *db.Pool
	Storage storage.Storage
	LLM     llmgate.Client
	Parsers *parser.Registry
	Logger  *slog.Logger

	// SummaryMaxChars caps the content window sent to the LLM per section.
	// Sections longer than this are truncated — we're generating a short
	// summary, not reproducing the text.
	SummaryMaxChars int

	// SummaryModel, when non-empty, overrides the LLM client's default
	// model for summarization calls.
	SummaryModel string

	// SummaryConcurrency bounds the number of concurrent LLM calls during
	// the summarization stage. Higher values speed up ingest for large
	// documents at the cost of higher LLM throughput. Default: 4.
	SummaryConcurrency int
}

// NewPipeline returns a Pipeline with sensible defaults filled in.
func NewPipeline(p Pipeline) *Pipeline {
	if p.SummaryMaxChars == 0 {
		p.SummaryMaxChars = 4000
	}
	if p.SummaryConcurrency <= 0 {
		p.SummaryConcurrency = 4
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
	if err := p.summarize(ctx, pl.DocumentID, pl.Profile); err != nil {
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
	// Only overwrite the row's title (which was seeded with the
	// filename at upload time) when the parsed title looks usable.
	// Watermarked PDFs whose overlay text shares a Y coordinate with
	// the real title produce mojibake like "GGlloobbaall SSttrraatteeggyy"
	// — we'd rather keep the original filename than show that to a user.
	if doc.Title != "" && !isLikelyMojibakeTitle(doc.Title) {
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

			// Strip invalid UTF-8 / disallowed control chars at storage
			// time so we never persist bytes the LLM SDKs would reject
			// later. PDFs with CID-mapped fonts and no ToUnicode CMap
			// leak raw glyph IDs into extracted text.
			cleanedContent := cleanForLLM(s.Content)
			if strings.TrimSpace(cleanedContent) != "" {
				if err := p.Storage.Put(ctx, contentKey,
					bytes.NewReader([]byte(cleanedContent)),
					storage.Metadata{
						ContentType: "text/plain; charset=utf-8",
						Size:        int64(len(cleanedContent)),
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
				Title:      cleanForLLM(s.Title),
				ContentRef: contentKey,
				TokenCount: approxTokens(cleanedContent),
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
// Summarization is parallelized across sections, bounded by
// Pipeline.SummaryConcurrency. This speeds up ingest for large
// documents (100+ sections) from minutes to seconds.
//
// Processing order: leaf sections first (depth DESC, ordinal ASC) so
// that by the time internal sections are summarized, their children's
// titles are already populated.
//
// Non-fatal per-section errors are collected and returned joined — the
// caller decides whether to fail the whole document.
func (p *Pipeline) summarize(ctx context.Context, docID tree.DocumentID, profile string) error {
	sections, err := p.DB.ListSectionsForWorker(ctx, docID)
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

	// Separate sections into depth layers so we can process leaves in
	// parallel first, then move up the tree. Within each layer, sections
	// are independent and safe to parallelize.
	maxDepth := 0
	for _, s := range sections {
		if s.Depth > maxDepth {
			maxDepth = s.Depth
		}
	}

	byDepth := make(map[int][]db.Section)
	for _, s := range sections {
		if s.Summary != "" {
			continue
		}
		byDepth[s.Depth] = append(byDepth[s.Depth], s)
	}

	var (
		mu       sync.Mutex
		errs     []error
		computed = map[tree.SectionID]string{} // section ID → freshly-written summary
	)

	// Process from deepest to shallowest so children are summarized
	// before their parents.
	for depth := maxDepth; depth >= 0; depth-- {
		layer := byDepth[depth]
		if len(layer) == 0 {
			continue
		}

		sem := make(chan struct{}, p.SummaryConcurrency)
		g, gctx := errgroup.WithContext(ctx)

		for _, s := range layer {
			s := s
			g.Go(func() error {
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-gctx.Done():
					return nil
				}

				// Build child context from already-computed summaries.
				// Children live in deeper layers that completed before this
				// one (g.Wait between layers), so their summaries are ready.
				// Fall back to a child's stored summary, then its title.
				var childLines []string
				if kids := children[s.ID]; len(kids) > 0 {
					mu.Lock()
					for _, c := range kids {
						cs := computed[c.ID]
						if cs == "" {
							cs = c.Summary
						}
						if cs == "" {
							childLines = append(childLines, fmt.Sprintf("- %s", c.Title))
						} else {
							childLines = append(childLines, fmt.Sprintf("- %s: %s", c.Title, cs))
						}
					}
					mu.Unlock()
				}

				summary, err := p.summaryFor(gctx, s, childLines, profile)
				if err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("section %s: %w", s.ID, err))
					mu.Unlock()
					return nil // non-fatal — don't abort siblings
				}

				mu.Lock()
				computed[s.ID] = summary
				mu.Unlock()

				if err := p.DB.UpdateSectionSummary(gctx, s.ID, summary, s.TokenCount); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
				return nil
			})
		}

		_ = g.Wait() // errors collected in errs, not propagated
	}

	return errors.Join(errs...)
}

func (p *Pipeline) summaryFor(ctx context.Context, s db.Section, childLines []string, profile string) (string, error) {
	var body string
	if len(childLines) == 0 {
		// Leaf: fetch the stored text.
		if s.ContentRef == "" {
			return cleanForLLM(s.Title), nil
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
		body = cleanForLLM(string(raw))
	} else {
		// Internal: compose from children's *summaries* (richer than bare
		// titles) so a parent's summary reflects what's actually inside it.
		var b strings.Builder
		b.WriteString("This section's subsections, each with a short summary:\n")
		for _, line := range childLines {
			b.WriteString(cleanForLLM(line))
			b.WriteByte('\n')
		}
		body = b.String()
	}

	resp, err := p.LLM.Complete(ctx, llmgate.Request{
		Model:       p.SummaryModel,
		Temperature: 0.0,
		MaxTokens:   260,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: summarySystemPrompt(profile)},
			{Role: llmgate.RoleUser, Content: fmt.Sprintf(
				"Section titled %q.\n\n%s\n\nReturn a single sentence (≤ 60 words) that names this section's concrete topics, entities, identifiers, and key items so a retrieval engine can match it to user questions.",
				cleanForLLM(s.Title), body)},
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
	// Use a FRESH context for the failure write — the inbound one is
	// almost certainly the reason we're failing (timeout/cancel) and
	// reusing it would leave the doc stuck on "parsing" forever.
	failCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.DB.SetDocumentStatus(failCtx, id, db.StatusFailed, msg); err != nil {
		p.Logger.Error("ingest: failed to mark document failed", "err", err, "cause", cause)
	}
}

// cleanForLLM strips invalid-UTF-8 bytes and a couple of control
// characters that some LLM SDKs reject at the proto layer (the
// gemini-go SDK is strict about this — it errors with
// "google.ai.generativelanguage.v1beta.Part.text contains invalid UTF-8"
// the moment any byte sequence isn't a complete UTF-8 codepoint).
//
// PDFs with custom CID-mapped fonts and no ToUnicode CMap leak raw
// glyph IDs into our extracted text, which look like garbage bytes.
// We drop them rather than fail the whole summarization.
func cleanForLLM(s string) string {
	if utf8.ValidString(s) && !hasBadControlChars(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if r == utf8.RuneError && size == 1 {
			b.WriteRune('�')
			continue
		}
		// Drop NUL + most C0 control chars; keep tab/newline/CR.
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func hasBadControlChars(s string) bool {
	for _, r := range s {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' {
			return true
		}
	}
	return false
}

// isLikelyMojibakeTitle returns true when s shows the doubled-glyph
// signature of two-layer PDFs (an overlay watermark drawn over real
// text at the same Y coordinate, so chars from both layers interleave
// into runs like "GGlloobbaall"). Also flags suspiciously short titles
// that are pure punctuation/whitespace.
//
// Conservative on purpose: we'd rather show a slightly weird real
// title than silently fall back to the filename for a normal doc.
func isLikelyMojibakeTitle(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	// Count alphabetic chars + adjacent same-letter pairs (case-insensitive).
	letters := 0
	doubled := 0
	var prev rune
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			letters++
			if r == prev {
				doubled++
			}
			prev = r
		} else {
			prev = 0
		}
	}
	if letters < 4 {
		return true // too few letters to be a real title
	}
	// >30% adjacent-doubled letters is the signature of the two-layer
	// glyph interleaving — normal English titles sit well under 5%.
	return doubled*100/letters > 30
}

// summarySystemPrompt returns a domain-aware system prompt for the
// summarization LLM based on the document's store profile. Summaries are
// optimized for RETRIEVAL: a downstream retrieval engine, given only the
// summary, should be able to tell whether the section answers a specific
// question. So we ask the model to name the concrete topics, entities,
// identifiers, and key items the section covers — not just describe it
// generically.
func summarySystemPrompt(profile string) string {
	const retrievalRule = "Write so a downstream retrieval engine, reading only your summary, can tell whether this section answers a specific user question. Name the section's concrete topics — entities, identifiers, table contents, named items, key numbers — not just a generic description. One factual sentence, ≤ 60 words, no preamble, no quotes."
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "research":
		return "You summarize sections of academic research papers. Capture the key claim, method, dataset, or result. " + retrievalRule
	case "medical":
		return "You summarize sections of clinical and medical documents. Capture the key finding, recommendation, dosage, drug name, definition, or guideline. " + retrievalRule
	default:
		return "You summarize sections of business, legal, and financial documents (filings, reports, contracts). " + retrievalRule
	}
}

// approxTokens estimates the token count of s without a provider
// round-trip. We use a word-based estimate (~1.3 tokens/word for English,
// which matches GPT/Gemini BPE behaviour closely) with a character-based
// floor so non-space-delimited text (CJK, code) isn't under-counted.
//
// Exact counts would need the provider's own tokenizer (e.g. Gemini's
// countTokens API) — a per-section round-trip the ingest path
// deliberately avoids. Retrieval reconciles real counts at query time.
func approxTokens(s string) int {
	if s == "" {
		return 0
	}
	byWords := len(strings.Fields(s)) * 13 / 10 // ~1.3 tokens per word
	byChars := len(s) / 4
	n := byWords
	if byChars > n {
		n = byChars
	}
	if n < 1 {
		n = 1
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
