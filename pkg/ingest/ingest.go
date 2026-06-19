// Package ingest owns the asynchronous pipeline that turns raw document
// bytes into a queryable tree:
//
//	parse      — bytes → hierarchical outline (parser.Registry)
//	build tree — outline → sections persisted in Postgres + object store
//	summarize  — every section gets an LLM-written summary
//	hyde       — every leaf gets a list of HyDE candidate questions
//
// After parse + persist, the summarize and hyde stages run CONCURRENTLY:
// HyDE operates from a section's title + content (the summary, when
// available, is a nice-to-have), so it has no hard ordering dependency
// on summarize. Running them in parallel roughly halves wall time on
// long documents. Total LLM-in-flight is capped by an optional shared
// semaphore (Pipeline.GlobalLLMConcurrency) so we don't oversubscribe
// the provider.
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

// ModeMinimal is the Pipeline.Mode value that collapses ingest to
// parse → build tree → persist → ready, skipping all LLM enrichment
// and table extraction. Any other value runs the full pipeline.
const ModeMinimal = "minimal"

// docPersister is the narrow slice of *db.Pool the parse → persist →
// ready path depends on. Declaring it here (rather than threading the
// concrete *db.Pool) lets the minimal-mode runner be exercised with a
// fake store, so the "zero LLM calls, still reaches ready" guarantee is
// provable without a live Postgres. *db.Pool satisfies it.
type docPersister interface {
	SetDocumentStatus(ctx context.Context, id tree.DocumentID, s db.DocumentStatus, errMsg string) error
	SetDocumentTitle(ctx context.Context, id tree.DocumentID, title string) error
	UpsertSection(ctx context.Context, s db.Section) error
}

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

	// Mode selects how much work Run does before marking a document
	// ready. "minimal" collapses ingest to parse → build tree → persist
	// → ready, skipping every per-section LLM stage (summarize, HyDE,
	// multi-axis summaries, TOC build) AND the pdftable table-finding
	// pass. Anything else (including the empty Go zero value used by
	// Pipeline literals in tests) runs the full enrichment pipeline.
	//
	// The page-based retrieval strategy (/v1/answer/treewalk) needs none
	// of the skipped enrichment — it navigates a synthesised-from-sections
	// TOC and reads raw section/page text at query time — so a
	// minimal-ingested document is immediately queryable through it.
	Mode string

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

	// SummaryAxesEnabled toggles the Phase 2.5 multi-axis structured
	// summary path. When true (the default), the summarizer asks the LLM
	// for a JSON object {topics, entities, numbers, one_line} and
	// persists both the axes blob and the one_line into the existing
	// summary column. When false, the pipeline falls back to the
	// pre-2.5 single-sentence prompt — no axes are written and the
	// retrieval prompt sees only summary + HyDE questions.
	//
	// Defaulted to true by config wiring. Left as the Go zero value
	// (false) when Pipeline is constructed directly, so old test paths
	// that build a Pipeline literal without the field keep the legacy
	// behaviour.
	SummaryAxesEnabled bool

	// SummaryAxesMaxTopics caps the topic axis returned by the
	// structured summarizer. Default: 4.
	SummaryAxesMaxTopics int

	// SummaryAxesMaxEntities caps the entities axis. Default: 8.
	SummaryAxesMaxEntities int

	// SummaryAxesMaxNumbers caps the numbers axis. Default: 6.
	SummaryAxesMaxNumbers int

	// HyDEEnabled toggles the candidate-question generation stage.
	// Defaulted to true by config wiring; left as the Go zero value
	// (false) when Pipeline is constructed directly, so unit tests with
	// no LLM can opt out by simply not setting it.
	HyDEEnabled bool

	// HyDEModel, when non-empty, overrides the model used for HyDE
	// candidate-question generation. Defaults to SummaryModel.
	HyDEModel string

	// HyDENumQuestions is the target number of candidate questions
	// generated per leaf section. Default: 5.
	HyDENumQuestions int

	// HyDEConcurrency bounds parallel LLM calls during the HyDE stage.
	// Default: 4.
	HyDEConcurrency int

	// GlobalLLMConcurrency, when > 0, caps the total number of LLM calls
	// in flight across BOTH the summarize and HyDE stages combined.
	// Each stage still respects its own per-stage cap
	// (SummaryConcurrency / HyDEConcurrency), but neither can push the
	// shared counter above this ceiling. Useful because summarize and
	// HyDE now run concurrently — without this, total in-flight load is
	// SummaryConcurrency + HyDEConcurrency, which may exceed the
	// provider's per-tenant rate limit.
	//
	// 0 disables the global cap (each stage is bounded only by its own
	// per-stage semaphore). Default applied by NewPipeline: 12.
	GlobalLLMConcurrency int

	// TOCEnabled toggles the LLM-built table-of-contents stage. The
	// stage runs after summarize+HyDE on PDF inputs and persists the
	// resulting tree on documents.toc_tree (JSONB). Failures are
	// non-fatal — they leave the column NULL.
	//
	// Defaulted to true by config wiring; left as the Go zero value
	// (false) when Pipeline is constructed directly, so unit tests
	// with no LLM can opt out by simply not setting it.
	TOCEnabled bool

	// TOCModel overrides the LLM model used by the TOC builder.
	// Empty inherits SummaryModel (which itself falls back to the
	// client default).
	TOCModel string

	// TOCConcurrency caps parallel LLM calls during the TOC
	// verification phase. Default: 4.
	TOCConcurrency int

	// TOCCheckPages bounds the leading prefix the detector scans
	// for a table of contents. Default: 20.
	TOCCheckPages int

	// LLMCallTimeout bounds each INDIVIDUAL LLM call the pipeline issues
	// (one section's summary, one leaf's HyDE questions, one TOC
	// detect/extract/verify turn). It is the safety valve against a
	// provider call that hangs with neither a response nor an error:
	// without it, that call's bounded-concurrency errgroup blocks on
	// Wait() forever and the document never leaves `summarizing` (observed
	// stuck for 13+ hours).
	//
	// A call that exceeds this deadline is handled exactly like any other
	// per-section failure: the surrounding stage logs it and skips the
	// section, leaving its existing/empty summary. One hung call can no
	// longer freeze the whole document.
	//
	// Zero (the Go zero value, used by Pipeline literals in tests) means
	// "no per-call timeout" so existing test paths that don't set it keep
	// their unbounded behaviour. NewPipeline defaults it to 90s.
	LLMCallTimeout time.Duration

	// globalLLMSem is the lazily-initialized shared semaphore enforcing
	// GlobalLLMConcurrency. nil means "no global cap" — callers fall back
	// to per-stage limits only.
	globalLLMSem chan struct{}
}

// defaultLLMCallTimeout is the per-call deadline NewPipeline applies when
// LLMCallTimeout is left unset. 90s is generous for a single summarize /
// HyDE / TOC turn even on a slow reasoning model, while still being short
// enough that a hung call is reaped in seconds-to-low-minutes rather than
// blocking the document forever.
const defaultLLMCallTimeout = 90 * time.Second

// NewPipeline returns a Pipeline with sensible defaults filled in.
func NewPipeline(p Pipeline) *Pipeline {
	if p.SummaryMaxChars == 0 {
		p.SummaryMaxChars = 4000
	}
	if p.SummaryConcurrency <= 0 {
		p.SummaryConcurrency = 4
	}
	// Multi-axis structured summaries are on by default — they unlock
	// Phase 2.5 retrieval signal without changing the existing summary
	// field's contract. Callers that construct a Pipeline literal
	// directly and don't set this still get the legacy single-line
	// path (Go zero value), matching the historical test contract.
	if p.SummaryAxesMaxTopics <= 0 {
		p.SummaryAxesMaxTopics = 4
	}
	if p.SummaryAxesMaxEntities <= 0 {
		p.SummaryAxesMaxEntities = 8
	}
	if p.SummaryAxesMaxNumbers <= 0 {
		p.SummaryAxesMaxNumbers = 6
	}
	if p.HyDENumQuestions <= 0 {
		p.HyDENumQuestions = 5
	}
	if p.HyDEConcurrency <= 0 {
		p.HyDEConcurrency = 4
	}
	if p.TOCConcurrency <= 0 {
		p.TOCConcurrency = 4
	}
	if p.TOCCheckPages <= 0 {
		p.TOCCheckPages = 20
	}
	// Default the global cap to a value that comfortably exceeds the
	// sum of the two default per-stage caps (4 + 4 = 8) while leaving
	// some headroom — but stays well below typical provider per-tenant
	// concurrency limits.
	if p.GlobalLLMConcurrency < 0 {
		p.GlobalLLMConcurrency = 0
	}
	if p.GlobalLLMConcurrency == 0 {
		p.GlobalLLMConcurrency = 12
	}
	if p.GlobalLLMConcurrency > 0 {
		p.globalLLMSem = make(chan struct{}, p.GlobalLLMConcurrency)
	}
	// A per-call timeout is the difference between "one bad section" and
	// "the whole document wedged for hours", so NewPipeline always fills
	// one in. Pipeline literals (test paths) that leave it zero keep the
	// historical unbounded behaviour.
	if p.LLMCallTimeout <= 0 {
		p.LLMCallTimeout = defaultLLMCallTimeout
	}
	if p.Logger == nil {
		p.Logger = slog.Default()
	}
	return &p
}

// completeWithTimeout issues a single LLM call bounded by timeout. A
// non-positive timeout disables the bound (calls ctx directly), preserving
// the legacy behaviour for Pipeline literals that never set one.
//
// The returned error on deadline expiry is context.DeadlineExceeded
// wrapped by the client — callers in the ingest stages already treat any
// Complete error as a non-fatal per-section skip, so a timeout slots into
// the existing degrade-and-continue path with no special-casing.
func completeWithTimeout(ctx context.Context, client llmgate.Client, req llmgate.Request, timeout time.Duration) (*llmgate.Response, error) {
	if timeout <= 0 {
		return client.Complete(ctx, req)
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return client.Complete(callCtx, req)
}

// isTimeout reports whether err is a context deadline/cancellation — the
// signature of a per-call LLM timeout. The ingest retry loops use it to
// stop retrying immediately on a timeout: re-issuing a call that just hung
// would only multiply the wall-time cost (N retries × the timeout) without
// changing the outcome, so a timeout is terminal, not retryable.
//

// acquireGlobalLLM blocks until a global-LLM-concurrency slot is free,
// or returns false if ctx is canceled first. Returns a release func the
// caller must invoke (typically deferred). Safe to call when the global
// semaphore is disabled — the returned release is a no-op.
func (p *Pipeline) acquireGlobalLLM(ctx context.Context) (release func(), ok bool) {
	if p.globalLLMSem == nil {
		return func() {}, true
	}
	select {
	case p.globalLLMSem <- struct{}{}:
		return func() { <-p.globalLLMSem }, true
	case <-ctx.Done():
		return func() {}, false
	}
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

// Run executes the pipeline for one document. Safe to retry.
//
// When Mode == ModeMinimal it dispatches to runMinimal — parse → build
// tree → persist → ready, with no LLM enrichment and no table
// extraction. Otherwise it runs the full enrichment pipeline below.
func (p *Pipeline) Run(ctx context.Context, pl Payload) error {
	if p.Mode == ModeMinimal {
		return p.runMinimal(ctx, p.DB, pl)
	}

	log := p.Logger.With("document_id", string(pl.DocumentID))
	log.Info("ingest: start", "source_ref", pl.SourceRef)

	if err := p.DB.SetDocumentStatus(ctx, pl.DocumentID, db.StatusParsing, ""); err != nil {
		return err
	}

	parsed, err := p.parse(ctx, p.Parsers, pl)
	if err != nil {
		p.fail(ctx, p.DB, pl.DocumentID, "parse", err)
		return err
	}
	log.Info("ingest: parsed", "sections", len(parsed.Flatten()), "title", parsed.Title)

	if err := p.persistTree(ctx, p.DB, pl.DocumentID, parsed); err != nil {
		p.fail(ctx, p.DB, pl.DocumentID, "persist tree", err)
		return err
	}

	if err := p.DB.SetDocumentStatus(ctx, pl.DocumentID, db.StatusSummarizing, ""); err != nil {
		return err
	}

	stageStart := time.Now()
	summarizeFn := func(ctx context.Context) error {
		return p.summarize(ctx, pl.DocumentID, pl.Profile)
	}
	var hydeFn func(ctx context.Context) error
	if p.HyDEEnabled {
		hydeFn = func(ctx context.Context) error {
			return p.generateCandidateQuestions(ctx, pl.DocumentID, pl.Profile)
		}
	}
	sumErr, hydeErr := runParallelStages(ctx, summarizeFn, hydeFn)
	if sumErr != nil {
		// Summarization failures are recoverable — a section without a
		// summary is still query-able, just less efficient. We log and
		// proceed rather than dead-letter the document.
		log.Warn("ingest: summarize had errors", "err", sumErr)
	}
	if hydeErr != nil {
		// HyDE is a retrieval-quality booster, not a correctness
		// requirement. Failures here leave the document fully usable
		// (just with less recall on lexically-distant queries), so we
		// log and proceed.
		log.Warn("ingest: hyde had errors", "err", hydeErr)
	}
	log.Info("ingest: summarize+hyde complete", "elapsed", time.Since(stageStart))

	// LLM-built TOC tree (TreeWalk-style). PDF-only because it
	// relies on the parser's PageStart/PageEnd attribution to
	// reconstruct per-page text. Non-fatal: a builder failure
	// leaves documents.toc_tree NULL and the document remains
	// fully retrievable via the sections tree above.
	if p.TOCEnabled && pl.ContentType == "application/pdf" {
		if err := p.runTOCBuilder(ctx, pl.DocumentID, parsed, log); err != nil {
			log.Warn("ingest: toc-builder failed; falling back to NULL toc_tree", "err", err)
		}
	}

	if err := p.DB.SetDocumentStatus(ctx, pl.DocumentID, db.StatusReady, ""); err != nil {
		return err
	}
	log.Info("ingest: ready")
	return nil
}

// runTOCBuilder assembles per-page text from the parsed PDF, runs
// the LLM-driven TOC builder over it, and persists the result.
// Returns an error only on a transport-level builder failure or a
// JSON-marshal blip; the caller logs and continues either way.
//
// A nil-result (no usable nodes) is treated as success and writes
// SQL NULL to documents.toc_tree (which is the column's default,
// so this is also the no-op).
func (p *Pipeline) runTOCBuilder(ctx context.Context, docID tree.DocumentID, parsed *parser.ParsedDoc, log *slog.Logger) error {
	pages := assemblePagesFromSections(parsed.Sections)
	if len(pages) == 0 {
		log.Info("ingest: toc-builder skipped; no per-page text available")
		return nil
	}
	model := p.TOCModel
	if model == "" {
		model = p.SummaryModel
	}
	builder := &TOCBuilder{
		LLM:            p.LLM,
		Model:          model,
		Concurrency:    p.TOCConcurrency,
		TOCCheckPages:  p.TOCCheckPages,
		LLMCallTimeout: p.LLMCallTimeout,
	}
	nodes, usage, err := builder.Build(ctx, pages)
	if err != nil {
		return err
	}
	log.Info("ingest: toc-builder done",
		"top_level_nodes", len(nodes),
		"llm_calls", usage.LLMCalls,
		"input_tokens", usage.InputTokens,
		"output_tokens", usage.OutputTokens,
	)
	if len(nodes) == 0 {
		return nil
	}
	treeJSON, err := json.Marshal(nodes)
	if err != nil {
		return fmt.Errorf("marshal toc tree: %w", err)
	}
	if err := p.DB.UpdateDocumentTOCTree(ctx, docID, treeJSON); err != nil {
		return fmt.Errorf("persist toc tree: %w", err)
	}
	return nil
}

// assemblePagesFromSections groups the parsed sections' text by
// their PageStart, producing PageText entries the TOC builder can
// reason over. Sections that span multiple pages collapse onto
// their starting page — perfect page reconstruction would need
// raw glyph-level coordinates the parser doesn't currently
// surface, but the title-on-claimed-page heuristic still works
// because section starts (where the LLM looks for titles) live
// on PageStart.
//
// Sections with PageStart == 0 are skipped (the parser couldn't
// place them) so the builder never sees ambiguous page numbers.
func assemblePagesFromSections(secs []parser.Section) []PageText {
	pageText := map[int]*strings.Builder{}
	pages := []int{}
	var walk func([]parser.Section)
	walk = func(ss []parser.Section) {
		for _, s := range ss {
			if s.PageStart > 0 {
				b, ok := pageText[s.PageStart]
				if !ok {
					b = &strings.Builder{}
					pageText[s.PageStart] = b
					pages = append(pages, s.PageStart)
				}
				if title := strings.TrimSpace(s.Title); title != "" {
					if b.Len() > 0 {
						b.WriteByte('\n')
					}
					b.WriteString(title)
					b.WriteByte('\n')
				}
				if body := strings.TrimSpace(s.Content); body != "" {
					b.WriteString(body)
					b.WriteByte('\n')
				}
			}
			walk(s.Children)
		}
	}
	walk(secs)
	// Sort the page-number index in place.
	sortIntsAscending(pages)
	out := make([]PageText, 0, len(pages))
	for _, p := range pages {
		out = append(out, PageText{PageNumber: p, Text: pageText[p].String()})
	}
	return out
}

// sortIntsAscending sorts a slice of ints in place. Insertion sort
// is fine here — pages slice is typically a few hundred items
// at most.
func sortIntsAscending(xs []int) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

// runParallelStages runs summarize and HyDE concurrently, returning each
// stage's error independently so callers can log them separately. A nil
// hydeFn skips the HyDE stage (returns nil for hydeErr).
//
// Extracted so the interleave behaviour is testable without touching the
// real DB-backed summarize/HyDE entry points.
func runParallelStages(ctx context.Context, summarizeFn, hydeFn func(context.Context) error) (summarizeErr, hydeErr error) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if summarizeFn != nil {
			summarizeErr = summarizeFn(ctx)
		}
	}()
	if hydeFn != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hydeErr = hydeFn(ctx)
		}()
	}
	wg.Wait()
	return summarizeErr, hydeErr
}

func (p *Pipeline) parse(ctx context.Context, parsers *parser.Registry, pl Payload) (*parser.ParsedDoc, error) {
	rc, _, err := getSourceWithRetry(ctx, p.Storage, pl.SourceRef)
	if err != nil {
		return nil, fmt.Errorf("fetch source: %w", err)
	}
	defer func() { _ = rc.Close() }() // best-effort close
	return parsers.Parse(ctx, pl.ContentType, pl.Filename, rc)
}

// getSourceWithRetry fetches a freshly-uploaded object, tolerating the
// brief window where the background ingest job (enqueued right after the
// upload handler's Storage.Put) outraces the source bytes becoming
// visible. Storage.Put now fsyncs, so this is belt-and-suspenders for
// slower or eventually-consistent backends: a transient ErrNotFound is
// retried with short backoff rather than failing the whole document.
// Any non-ErrNotFound error returns immediately.
func getSourceWithRetry(ctx context.Context, s storage.Storage, key string) (io.ReadCloser, storage.Metadata, error) {
	// Up to ~16s of incremental backoff. A large source (multi-MB) written
	// under heavy concurrent ingestion on a busy/low-disk filesystem can take
	// several seconds to become visible to this worker; a too-short window
	// turns that transient into a hard "object not found" failure.
	const attempts = 16
	var lastErr error
	for i := 0; i < attempts; i++ {
		rc, meta, err := s.Get(ctx, key)
		if err == nil {
			return rc, meta, nil
		}
		if !errors.Is(err, storage.ErrNotFound) {
			return nil, storage.Metadata{}, err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return nil, storage.Metadata{}, ctx.Err()
		case <-time.After(time.Duration(i+1) * 125 * time.Millisecond):
		}
	}
	return nil, storage.Metadata{}, lastErr
}

// runMinimal is the fast/minimal ingest path: parse → build tree →
// persist → ready. It does ZERO LLM work — no summarize, no HyDE, no
// multi-axis summaries, no TOC build — and parses with table extraction
// DISABLED (the pdftable table-finding pass is the slow/hang-prone part
// of parse, and the page-based strategy reads raw page text which still
// contains the table's text, so dropping table *sections* loses nothing
// for it).
//
// The doc reaches StatusReady the moment the section tree is persisted,
// which is what "ready" means for the page-based strategy: it
// synthesises its TOC from the section tree (titles + page ranges) when
// documents.toc_tree is NULL — and minimal mode leaves it NULL — and
// reads section bodies from storage at query time.
//
// store is the persistence target; production passes p.DB. The DB seam
// is an interface so this path is testable without a live Postgres.
func (p *Pipeline) runMinimal(ctx context.Context, store docPersister, pl Payload) error {
	log := p.Logger.With("document_id", string(pl.DocumentID))
	log.Info("ingest: start (minimal mode)", "source_ref", pl.SourceRef)

	if err := store.SetDocumentStatus(ctx, pl.DocumentID, db.StatusParsing, ""); err != nil {
		return err
	}

	// Table extraction is disabled unconditionally in minimal mode,
	// regardless of ingest.tables.enabled: a nil-opts registry makes the
	// PDF parser skip the table-finding pass entirely. All other parsers
	// are unaffected.
	parsers := RegistryFromTableOpts(nil)
	parsed, err := p.parse(ctx, parsers, pl)
	if err != nil {
		p.fail(ctx, store, pl.DocumentID, "parse", err)
		return err
	}
	log.Info("ingest: parsed", "sections", len(parsed.Flatten()), "title", parsed.Title)

	if err := p.persistTree(ctx, store, pl.DocumentID, parsed); err != nil {
		p.fail(ctx, store, pl.DocumentID, "persist tree", err)
		return err
	}

	// Skip summarize / HyDE / multi-axis / TOC entirely — flip straight
	// to ready. The document is now queryable via the page-based
	// strategy (synthesised TOC + raw page reads).
	if err := store.SetDocumentStatus(ctx, pl.DocumentID, db.StatusReady, ""); err != nil {
		return err
	}
	log.Info("ingest: ready (minimal mode)")
	return nil
}

// persistTree writes sections + full content in document order. Parents
// are written before children so the FK on sections.parent_id holds.
//
// The DB operations go through the narrow docPersister interface so the
// persist path can be exercised (e.g. by the minimal-mode test) without
// a live Postgres; production callers pass p.DB, which satisfies it.
func (p *Pipeline) persistTree(ctx context.Context, store docPersister, docID tree.DocumentID, doc *parser.ParsedDoc) error {
	// Only overwrite the row's title (which was seeded with the
	// filename at upload time) when the parsed title looks usable.
	// Watermarked PDFs whose overlay text shares a Y coordinate with
	// the real title produce mojibake like "GGlloobbaall SSttrraatteeggyy"
	// — we'd rather keep the original filename than show that to a user.
	if doc.Title != "" && !isLikelyMojibakeTitle(doc.Title) {
		if err := store.SetDocumentTitle(ctx, docID, doc.Title); err != nil {
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
			// Only assign a ContentRef when we actually wrote content. A
			// leaf whose text is empty after cleanForLLM (heading-only
			// sections, or CID-font garbage stripped to nothing) gets NO
			// object stored, so it must get NO ref — otherwise every later
			// read (summarize, HyDE, treewalk get_pages) chases a key that
			// was never written and fails with "storage: object not found".
			// Empty ContentRef is already the canonical "no stored content"
			// state every reader guards on (summaryFor falls back to the
			// title; the treewalk loader returns the summary or empty).
			cleanedContent := cleanForLLM(s.Content)
			contentRef := ""
			if strings.TrimSpace(cleanedContent) != "" {
				if err := p.Storage.Put(ctx, contentKey,
					bytes.NewReader([]byte(cleanedContent)),
					storage.Metadata{
						ContentType: "text/plain; charset=utf-8",
						Size:        int64(len(cleanedContent)),
					}); err != nil {
					return fmt.Errorf("store section %s: %w", id, err)
				}
				contentRef = contentKey
			}

			if err := store.UpsertSection(ctx, db.Section{
				ID:         id,
				DocumentID: docID,
				ParentID:   parent,
				Ordinal:    i,
				Depth:      depth,
				Title:      cleanForLLM(s.Title),
				ContentRef: contentRef,
				TokenCount: approxTokens(cleanedContent),
				PageStart:  s.PageStart,
				PageEnd:    s.PageEnd,
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
		computed = map[tree.SectionID]string{} // section ID → freshly-written one-line summary
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

				// Global cap on total LLM-in-flight across summarize+HyDE.
				// Released the moment the LLM call returns.
				release, ok := p.acquireGlobalLLM(gctx)
				if !ok {
					return nil
				}
				axes, err := p.summaryFor(gctx, s, childLines, profile)
				release()
				if err != nil {
					mu.Lock()
					errs = append(errs, fmt.Errorf("section %s: %w", s.ID, err))
					mu.Unlock()
					return nil // non-fatal — don't abort siblings
				}

				oneLine := ""
				if axes != nil {
					oneLine = axes.OneLine
				}
				mu.Lock()
				computed[s.ID] = oneLine
				mu.Unlock()

				// Always patch the flat `summary` column with the
				// one-line sentence so older API consumers continue to
				// see a populated summary. The axes blob is patched
				// separately and only when the multi-axis path is on,
				// so a future opt-out (or a parse failure) cleanly
				// leaves summary_axes NULL.
				if err := p.DB.UpdateSectionSummary(gctx, s.ID, oneLine, s.TokenCount); err != nil {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
				if p.SummaryAxesEnabled && axes != nil {
					if err := p.DB.UpdateSectionSummaryAxes(gctx, s.ID, axes); err != nil {
						mu.Lock()
						errs = append(errs, err)
						mu.Unlock()
					}
				}
				return nil
			})
		}

		_ = g.Wait() // errors collected in errs, not propagated
	}

	return errors.Join(errs...)
}

// summaryFor produces the multi-axis structured summary for a single
// section. Returns a non-nil *tree.SummaryAxes even on parse failure:
// in the failure case OneLine carries the model's raw text (or a
// fallback excerpt) and the slice axes are empty, so the section is
// still retrieval-able via the flat summary column.
//
// When SummaryAxesEnabled is false the function falls back to the
// pre-2.5 single-sentence prompt and returns axes with only OneLine
// populated. Callers (summarize) skip the axes JSONB write in that
// case, leaving summary_axes NULL.
func (p *Pipeline) summaryFor(ctx context.Context, s db.Section, childLines []string, profile string) (*tree.SummaryAxes, error) {
	var body string
	if len(childLines) == 0 {
		// Leaf: fetch the stored text.
		if s.ContentRef == "" {
			return &tree.SummaryAxes{OneLine: cleanForLLM(s.Title)}, nil
		}
		rc, _, err := p.Storage.Get(ctx, s.ContentRef)
		if err != nil {
			return nil, err
		}
		defer func() { _ = rc.Close() }() // best-effort close
		raw, err := io.ReadAll(io.LimitReader(rc, int64(p.SummaryMaxChars)))
		if err != nil {
			return nil, err
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

	if !p.SummaryAxesEnabled {
		oneLine, err := p.legacyOneLineSummary(ctx, s, body, profile)
		if err != nil {
			return nil, err
		}
		return &tree.SummaryAxes{OneLine: oneLine}, nil
	}
	return p.structuredSummaryFor(ctx, s, body, profile), nil
}

// legacyOneLineSummary is the pre-Phase-2.5 path: a single-sentence
// request. Kept for the SummaryAxesEnabled=false opt-out branch and
// for unit tests that build a Pipeline literal without the new flag.
func (p *Pipeline) legacyOneLineSummary(ctx context.Context, s db.Section, body, profile string) (string, error) {
	resp, err := completeWithTimeout(ctx, p.LLM, llmgate.Request{
		Model:       p.SummaryModel,
		Temperature: 0.0,
		MaxTokens:   260,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: summarySystemPrompt(profile)},
			{Role: llmgate.RoleUser, Content: fmt.Sprintf(
				"Section titled %q.\n\n%s\n\nReturn a single sentence (≤ 60 words) that names this section's concrete topics, entities, identifiers, and key items so a retrieval engine can match it to user questions.",
				cleanForLLM(s.Title), body)},
		},
	}, p.LLMCallTimeout)
	if err != nil {
		// Stub LLMs return ErrNotImplemented. Degrade gracefully: use a
		// truncated excerpt as the "summary" so downstream retrieval
		// still has something to reason over.
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

// structuredSummaryFor is the Phase 2.5 path: a JSON-mode request that
// returns {topics, entities, numbers, one_line}. Mirrors the HyDE
// retry-on-parse-failure shape — if the model produces non-JSON we
// retry with a stricter nudge; final failure degrades to an axes
// object whose OneLine carries the model's raw text and whose axis
// lists are empty, so ingest never calls p.fail for a summarization
// parse blip.
//
// ErrNotImplemented (stub LLM) collapses to the legacy text fallback
// so unit tests with no LLM keep producing a non-empty summary.
func (p *Pipeline) structuredSummaryFor(ctx context.Context, s db.Section, body, profile string) *tree.SummaryAxes {
	req := llmgate.Request{
		Model:       p.SummaryModel,
		Temperature: 0.0,
		MaxTokens:   600,
		Messages: []llmgate.Message{
			{Role: llmgate.RoleSystem, Content: summaryAxesSystemPrompt(profile)},
			{Role: llmgate.RoleUser, Content: fmt.Sprintf(
				"Section titled %q.\n\nContent:\n%s\n\n%s",
				cleanForLLM(s.Title), body,
				summaryAxesUserSuffix(p.SummaryAxesMaxTopics, p.SummaryAxesMaxEntities, p.SummaryAxesMaxNumbers),
			)},
		},
		JSONMode:   true,
		JSONSchema: []byte(summaryAxesJSONSchema),
	}

	axes, rawText, err := runSummaryAxesWithRetry(ctx, p.LLM, req, defaultSummaryAxesRetries, p.LLMCallTimeout)
	if err != nil {
		// Transport / ErrNotImplemented / unrecoverable: fall back to a
		// text excerpt as OneLine with empty axes. Never fail ingest.
		return &tree.SummaryAxes{OneLine: fallbackSummary(s.Title, body)}
	}
	if axes != nil {
		// Successful parse — enforce the configured per-axis caps and
		// normalise the one-line so older API consumers always see a
		// populated summary.
		capStrings(&axes.Topics, p.SummaryAxesMaxTopics)
		capStrings(&axes.Entities, p.SummaryAxesMaxEntities)
		capStrings(&axes.Numbers, p.SummaryAxesMaxNumbers)
		if strings.TrimSpace(axes.OneLine) == "" {
			axes.OneLine = fallbackSummary(s.Title, body)
		}
		return axes
	}
	// Parse failed but the LLM returned something — use the raw text as
	// OneLine so a downstream retrieval engine still has signal. Axis
	// lists stay empty.
	one := strings.TrimSpace(rawText)
	if one == "" {
		one = fallbackSummary(s.Title, body)
	}
	return &tree.SummaryAxes{OneLine: one}
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

func (p *Pipeline) fail(ctx context.Context, store docPersister, id tree.DocumentID, stage string, cause error) {
	msg := fmt.Sprintf("%s: %s", stage, cause.Error())
	// Use a FRESH context for the failure write — the inbound one is
	// almost certainly the reason we're failing (timeout/cancel) and
	// reusing it would leave the doc stuck on "parsing" forever.
	failCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := store.SetDocumentStatus(failCtx, id, db.StatusFailed, msg); err != nil {
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
// the engine ships with, using the production defaults for each format
// (including table-aware PDF extraction). Callers that need to override
// PDF table behaviour from config should use RegistryFromTableOpts.
func DefaultRegistry() *parser.Registry {
	return parser.NewRegistry(
		parser.NewMarkdown(),
		parser.NewHTML(),
		parser.NewDOCX(),
		parser.NewXLSX(),
		parser.NewCSV(),
		parser.NewPDF(),
		parser.NewText(),
	)
}

// RegistryFromTableOpts returns a parser.Registry where the PDF parser
// is configured from the supplied TableOpts, with the leaf-section cap
// and total-parse timeout left at their parser defaults (400 sections,
// 120s). Pass nil to disable table extraction entirely; pass
// parser.DefaultTableOpts() (or a custom set) to enable. All non-PDF
// parsers are constructed at their defaults.
//
// Use RegistryFromIngestParams to thread an operator-tuned cap / parse
// timeout from config.
func RegistryFromTableOpts(opts *parser.TableOpts) *parser.Registry {
	return RegistryFromIngestParams(opts, 0, 0)
}

// RegistryFromIngestParams returns a parser.Registry where the PDF parser
// is configured from the supplied TableOpts AND the operator-tuned
// leaf-section cap and total-parse timeout (ingest.max_sections,
// ingest.parse_timeout_seconds). maxSections == 0 / parseTimeout == 0
// each select the parser's built-in default; a negative value disables
// that bound. All non-PDF parsers are constructed at their defaults.
//
// This is the constructor the engine/server wiring uses so the parse
// deadline and section cap from config actually reach the parser — the
// outermost robustness valves for full-feature ingest. Table extraction,
// the section tree, and the cap all still run; they are merely bounded.
func RegistryFromIngestParams(opts *parser.TableOpts, maxSections int, parseTimeout time.Duration) *parser.Registry {
	return parser.NewRegistry(
		parser.NewMarkdown(),
		parser.NewHTML(),
		parser.NewDOCX(),
		parser.NewXLSX(),
		parser.NewCSV(),
		parser.NewPDFWithConfig(opts, maxSections, parseTimeout),
		parser.NewText(),
	)
}

// helper kept for tests — not used by the pipeline itself.
var _ = time.Now
