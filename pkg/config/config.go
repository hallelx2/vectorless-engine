// Package config loads engine configuration from a YAML file and/or
// environment variables (prefix VLE_).
//
// Precedence (highest wins):
//  1. Environment variables
//  2. YAML file supplied via --config flag
//  3. Built-in defaults
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the root configuration.
type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Database  DatabaseConfig  `yaml:"database"`
	Storage   StorageConfig   `yaml:"storage"`
	Queue     QueueConfig     `yaml:"queue"`
	LLM       LLMConfig       `yaml:"llm"`
	Retrieval RetrievalConfig `yaml:"retrieval"`
	Ingest    IngestConfig    `yaml:"ingest"`
	Log       LogConfig       `yaml:"log"`
}

// IngestConfig configures retrieval-quality boosters that run during
// the ingest pipeline (between summarize and StatusReady).
type IngestConfig struct {
	// Mode selects how much work the ingest pipeline does before a
	// document is marked ready.
	//
	//   "full"    (default) — parse → build tree → persist → summarize
	//                          → HyDE → multi-axis summaries → TOC build.
	//                          Maximises retrieval quality at the cost of
	//                          ~1,000-3,000 LLM calls + a table-extraction
	//                          pass on a large filing (minutes of wall time).
	//
	//   "minimal"           — parse → build tree → persist → ready.
	//                          Skips ALL per-section LLM enrichment
	//                          (summarize, HyDE, multi-axis, TOC build)
	//                          AND the pdftable table-finding pass, so a
	//                          document becomes queryable in ~parse-speed
	//                          (seconds). The page-based retrieval strategy
	//                          (/v1/answer/pageindex) needs none of the
	//                          skipped enrichment: it navigates a TOC tree
	//                          (synthesised from the section tree when
	//                          documents.toc_tree is NULL) and reads raw
	//                          section/page text at query time — and the raw
	//                          page text still contains the tables' text, so
	//                          dropping table *sections* loses nothing for
	//                          it. The summary-dependent strategies
	//                          (chunked-tree, agentic) degrade to using
	//                          titles + raw content with no summaries.
	//
	// Empty defaults to "full". Engine env override: VLE_INGEST_MODE.
	Mode string `yaml:"mode"`

	HyDE HyDEConfig `yaml:"hyde"`

	// Tables configures pdftable's table-finding pass over PDF inputs.
	// Enabled by default — tables are the single biggest retrieval-quality
	// boost on FinanceBench-style documents because every numeric question
	// hides in a balance sheet that text-only extraction collapses.
	Tables TablesConfig `yaml:"tables"`

	// SummaryAxes configures the Phase 2.5 multi-axis summarizer.
	// Enabled by default — the structured shape unlocks
	// entity / numeric matching at retrieval time without changing the
	// existing `summary` field's contract (axes.one_line continues to
	// populate it).
	SummaryAxes SummaryAxesBlock `yaml:"summary_axes"`

	// TOC configures the PageIndex-style LLM-built table-of-contents
	// tree stage. Enabled by default for PDF inputs; the resulting
	// tree is persisted on documents.toc_tree (JSONB). Failures are
	// non-fatal — they leave the column NULL and the document fully
	// retrievable via the existing sections tree.
	TOC TOCBlock `yaml:"toc"`

	// GlobalLLMConcurrency caps the total number of LLM calls in flight
	// across the summarize and HyDE stages combined, which now run
	// concurrently. Each stage still respects its own per-stage cap
	// (summary_concurrency / hyde.concurrency), but neither can push the
	// shared counter above this ceiling.
	//
	// 0 (or omitted) defaults to 12 — enough headroom for the default
	// 4 + 4 per-stage caps while staying well below typical provider
	// per-tenant concurrency limits.
	GlobalLLMConcurrency int `yaml:"global_llm_concurrency"`

	// LLMCallTimeoutSeconds bounds each INDIVIDUAL LLM call issued by the
	// ingest pipeline (one section's summary, one leaf's HyDE questions,
	// one TOC detect/extract/verify turn). Without it, a single provider
	// call that hangs — no response, no error — blocks its bounded-
	// concurrency errgroup forever, and the document never leaves
	// `summarizing`. (Observed: a doc stuck for 13+ hours.)
	//
	// When a call exceeds this deadline it is treated exactly like any
	// other per-section failure: logged and skipped, leaving that
	// section with its existing/empty summary. One bad section can no
	// longer freeze the whole document — it still reaches `ready`.
	//
	// 0 (or omitted) defaults to 90. Set explicitly to tune for slow
	// reasoning models; a negative value is rejected by Validate.
	LLMCallTimeoutSeconds int `yaml:"llm_call_timeout_seconds"`

	// MaxSections caps the number of leaf sections a single document may
	// produce. A pathological PDF (e.g. a 92-page 10-Q whose every bold
	// table cell looks like a heading) can shatter into ~1500 leaves,
	// each of which then costs a summarize + HyDE LLM call — thousands of
	// calls that throttle or stall ingest.
	//
	// When the parsed leaf count exceeds this cap, the parser merges
	// adjacent small leaf sections under a shared parent (smallest
	// siblings first) until the document is back under the cap.
	//
	// 0 (or omitted) defaults to 400 — comfortably above a real filing's
	// section count (~170-510 with tables) while still catching the
	// runaway case. A negative value is rejected by Validate.
	MaxSections int `yaml:"max_sections"`

	// ParseTimeoutSeconds bounds the ENTIRE parse of a single document —
	// row extraction, table extraction, section building, and the leaf
	// cap, end to end. It is the outermost robustness valve: every
	// per-stage timeout inside the parser (per-page / doc-wide table
	// budgets) is bounded by something pre-LLM, but pure-Go row extraction
	// (ledongthuc's reader.Page(n).Content()) had no bound, so a
	// pathological PDF (observed: a 10-K stuck 600s+ in `parsing` even in
	// minimal mode) could hang the parse forever.
	//
	// When the whole parse exceeds this deadline the parser abandons the
	// work and returns a clear error; the ingest pipeline treats it like
	// any other parse failure (the document goes to `failed`), so a doc
	// that can't parse in time fails fast and is visible to ops/bench
	// rather than wedging the pipeline. NOTHING is disabled — the full
	// feature set (LLM TOC, tables, summarize, HyDE, multi-axis) still
	// runs; parse is merely bounded.
	//
	// 0 (or omitted) defaults to 120. A negative value is rejected by
	// Validate. Engine env override: VLE_INGEST_PARSE_TIMEOUT_SECONDS;
	// the server binary also forwards VLS_/VLE_INGEST_PARSE_TIMEOUT_SECONDS.
	ParseTimeoutSeconds int `yaml:"parse_timeout_seconds"`
}

// TOCBlock configures the LLM-driven table-of-contents tree
// builder. The builder reads page-by-page text from a freshly-
// ingested PDF and emits a hierarchical TOC (PageIndex-style),
// persisted on documents.toc_tree (JSONB).
//
// Enabled by default for PDF inputs; non-PDF documents skip the
// stage unconditionally. Builder failures never break ingest —
// the document remains fully retrievable via the existing
// sections tree.
type TOCBlock struct {
	// Enabled toggles the stage. Default: true. Flip to false to
	// skip the extra LLM round-trip when ingest budget matters
	// more than having a TOC tree for retrieval to reason over.
	Enabled bool `yaml:"enabled"`

	// Model overrides the LLM model used by the builder. Empty
	// inherits the engine's configured default. Point this at a
	// reasoning-capable model — the no-TOC generator has to find
	// hierarchy in raw body text, which a small/fast model often
	// botches.
	Model string `yaml:"model"`

	// Concurrency caps parallel LLM calls during the verification
	// phase (one call per leaf node). Default: 4.
	Concurrency int `yaml:"concurrency"`

	// TOCCheckPages bounds the leading prefix the detector scans
	// for a table of contents. Default: 20 — financial filings
	// put their TOC inside the first dozen pages and a document
	// without one by page 20 almost never has one further in.
	TOCCheckPages int `yaml:"toc_check_pages"`
}

// SummaryAxesBlock configures the Phase 2.5 structured summarizer.
//
// When enabled, the summarize stage runs in JSON mode and produces
// {topics, entities, numbers, one_line} per section instead of a
// single sentence. The structured blob is persisted in
// sections.summary_axes (JSONB); the one_line continues to land in
// sections.summary so older API consumers keep working.
//
// Disable to roll back to the pre-2.5 single-sentence behaviour
// without un-deploying the binary — useful for A/B comparisons or as
// a fast off-switch if a real-world document triggers a regression.
type SummaryAxesBlock struct {
	// Enabled toggles the structured path. Default: true.
	Enabled bool `yaml:"enabled"`

	// MaxTopics caps the topics axis the summarizer returns per
	// section. Default: 4. A misbehaving model that returns 50 topics
	// can't push past this cap; the prompt-budget impact stays bounded.
	MaxTopics int `yaml:"max_topics"`

	// MaxEntities caps the entities axis. Default: 8.
	MaxEntities int `yaml:"max_entities"`

	// MaxNumbers caps the numbers axis. Default: 6.
	MaxNumbers int `yaml:"max_numbers"`
}

// TablesConfig configures the table-extraction stage of the PDF parser.
// The stage runs pdftable's geometry-based finder over every page and
// emits each detected table as its own Section with
// Metadata["table"]="true", so downstream retrieval and the agentic
// navigator can branch on whether a candidate is a numeric table or
// prose.
//
// All knobs are forwarded to pdftable's TableSettings; defaults match
// pdfplumber. See pdftable's docs for the full strategy surface.
type TablesConfig struct {
	// Enabled toggles the stage. Default: true. Flip to false to
	// restore pre-integration text-only output; one config change is
	// enough to roll back if a real-world PDF triggers a regression.
	Enabled bool `yaml:"enabled"`

	// VerticalStrategy picks the source of vertical column boundaries.
	// Allowed values:
	//   - "lines"        (default) edges from drawn lines/rects/curves
	//   - "lines_strict" edges from drawn lines only
	//   - "text"         edges inferred from word alignment (borderless
	//                    tables — bank statements, narrative 10-Ks)
	//   - "explicit"     caller-supplied coordinates (not yet wired
	//                    through the engine config; reserved)
	VerticalStrategy string `yaml:"vertical_strategy"`

	// HorizontalStrategy picks the source of horizontal row boundaries.
	// Same value set as VerticalStrategy; the two axes can mix
	// independently (e.g. "lines" vertical + "text" horizontal).
	HorizontalStrategy string `yaml:"horizontal_strategy"`

	// MinTableRows drops candidate tables with fewer than this many
	// rows. Default: 2. Trivial single-row matches are almost always
	// false positives from layout artefacts (form-field grids, ruling
	// hairlines on a single line of text).
	MinTableRows int `yaml:"min_table_rows"`

	// MinTableCols drops candidate tables with fewer than this many
	// columns. Default: 2. Same rationale as MinTableRows — a single
	// column is a vertical list, not a table.
	MinTableCols int `yaml:"min_table_cols"`
}

// HyDEConfig configures the HyDE candidate-question stage. For each
// leaf section the pipeline asks the LLM to enumerate questions the
// section's content can answer; those are later folded into the
// retrieval prompt to widen lexical/semantic overlap with user queries.
type HyDEConfig struct {
	// Enabled toggles the stage. Default: true. Disable to skip an LLM
	// call per leaf when ingest budget matters more than recall.
	Enabled bool `yaml:"enabled"`

	// Model, when non-empty, overrides the LLM model used for HyDE
	// generation. Defaults to the same model used for summarization.
	Model string `yaml:"model"`

	// NumQuestions caps the questions generated per leaf section.
	// Default: 5.
	NumQuestions int `yaml:"num_questions"`

	// Concurrency bounds parallel LLM calls during the HyDE stage.
	// Default: 4.
	Concurrency int `yaml:"concurrency"`
}

// ServerConfig configures the HTTP server.
//
// TLS is opt-in. If TLS.CertFile and TLS.KeyFile are both set the engine
// listens with TLS directly; otherwise it listens plaintext and expects
// a reverse proxy (Caddy, nginx, an ALB, ingress) to terminate TLS.
type ServerConfig struct {
	Addr         string        `yaml:"addr"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	TLS          TLSConfig     `yaml:"tls"`
}

// TLSConfig enables direct TLS termination inside the engine.
type TLSConfig struct {
	// CertFile is a PEM-encoded certificate chain.
	CertFile string `yaml:"cert_file"`
	// KeyFile is a PEM-encoded private key matching CertFile.
	KeyFile string `yaml:"key_file"`
	// MinVersion is the minimum TLS version (e.g. "1.2", "1.3"). Empty
	// defaults to Go's default (currently TLS 1.2).
	MinVersion string `yaml:"min_version"`
}

// Enabled reports whether direct-TLS serving is configured.
func (t TLSConfig) Enabled() bool {
	return t.CertFile != "" && t.KeyFile != ""
}

// DatabaseConfig configures Postgres.
type DatabaseConfig struct {
	URL      string `yaml:"url"`
	MaxConns int    `yaml:"max_conns"`
}

// StorageConfig configures the document storage backend.
type StorageConfig struct {
	Driver string          `yaml:"driver"`
	Local  LocalStorage    `yaml:"local"`
	S3     S3StorageBlock  `yaml:"s3"`
	GCS    GCSStorageBlock `yaml:"gcs"`
}

// LocalStorage configures filesystem-backed storage.
type LocalStorage struct {
	Root string `yaml:"root"`
}

// S3StorageBlock configures S3-compatible storage.
type S3StorageBlock struct {
	Endpoint     string `yaml:"endpoint"`
	Region       string `yaml:"region"`
	Bucket       string `yaml:"bucket"`
	AccessKey    string `yaml:"access_key"`
	SecretKey    string `yaml:"secret_key"`
	UsePathStyle bool   `yaml:"use_path_style"`
}

// GCSStorageBlock configures native Google Cloud Storage. Auths via
// Application Default Credentials, so no key fields needed.
type GCSStorageBlock struct {
	Bucket string `yaml:"bucket"`
}

// QueueConfig configures the background job queue.
type QueueConfig struct {
	Driver string      `yaml:"driver"`
	QStash QStashBlock `yaml:"qstash"`
	River  RiverBlock  `yaml:"river"`
	Asynq  AsynqBlock  `yaml:"asynq"`
}

// QStashBlock configures QStash.
//
// Token is the publish token (used to enqueue). CurrentSigningKey and
// NextSigningKey are used to verify inbound webhooks; both are surfaced
// on the Upstash console under "Signing Keys". NextSigningKey is only
// populated while rotating.
type QStashBlock struct {
	Token             string `yaml:"token"`
	WebhookBaseURL    string `yaml:"webhook_base_url"`
	CurrentSigningKey string `yaml:"current_signing_key"`
	NextSigningKey    string `yaml:"next_signing_key"`
}

// RiverBlock configures River.
type RiverBlock struct {
	NumWorkers int `yaml:"num_workers"`
}

// AsynqBlock configures Asynq.
type AsynqBlock struct {
	Addr        string `yaml:"addr"`
	Password    string `yaml:"password"`
	DB          int    `yaml:"db"`
	Concurrency int    `yaml:"concurrency"`
}

// LLMConfig configures the LLM provider.
type LLMConfig struct {
	Driver    string         `yaml:"driver"`
	Anthropic AnthropicBlock `yaml:"anthropic"`
	OpenAI    OpenAIBlock    `yaml:"openai"`
	Gemini    GeminiBlock    `yaml:"gemini"`
}

// AnthropicBlock configures the Anthropic provider.
type AnthropicBlock struct {
	APIKey         string `yaml:"api_key"`
	Model          string `yaml:"model"`
	ReasoningModel string `yaml:"reasoning_model"`
	// BaseURL overrides the Anthropic API endpoint. Empty = official
	// api.anthropic.com. Set this to point the Anthropic driver at any
	// Anthropic-compatible gateway — e.g. GLM/Zhipu's
	// https://api.z.ai/api/anthropic — so the same driver can drive a
	// non-Anthropic model that speaks the Messages API.
	BaseURL string `yaml:"base_url"`
}

// OpenAIBlock configures the OpenAI provider.
type OpenAIBlock struct {
	APIKey         string `yaml:"api_key"`
	Model          string `yaml:"model"`
	ReasoningModel string `yaml:"reasoning_model"`
}

// GeminiBlock configures the Gemini provider.
type GeminiBlock struct {
	APIKey         string `yaml:"api_key"`
	Model          string `yaml:"model"`
	ReasoningModel string `yaml:"reasoning_model"`
}

// RetrievalConfig configures the retrieval strategy.
type RetrievalConfig struct {
	Strategy    string           `yaml:"strategy"`
	ChunkedTree ChunkedTreeBlock `yaml:"chunked_tree"`
	Agentic     AgenticBlock     `yaml:"agentic"`
	Cache       CacheBlock       `yaml:"cache"`
	AnswerSpan  AnswerSpanBlock  `yaml:"answer_span"`
	Answer      AnswerBlock      `yaml:"answer"`
	Planning    PlanningBlock    `yaml:"planning"`
	ReRank      ReRankBlock      `yaml:"rerank"`
	Replay      ReplayBlock      `yaml:"replay"`
	Abstain     AbstainBlock     `yaml:"abstain"`
	PageIndex   PageIndexBlock   `yaml:"pageindex"`
}

// PageIndexBlock configures the PageIndex page-based agentic
// strategy and its dedicated /v1/answer/pageindex endpoint.
//
// The strategy is registered as a Strategy choice
// (retrieval.strategy: pageindex) AND is wired into the
// /v1/answer/pageindex endpoint regardless of which selection
// strategy the server runs by default. The Enabled flag controls
// the endpoint only — flipping it off does not unregister the
// strategy, so a deployment that wants the strategy available
// to /v1/query but not the dedicated answer endpoint can still
// disable the endpoint here.
//
// Defaults are tuned to match the reference PageIndex demo: 8
// hops covers structure → 3 navigation calls → done + buffer,
// and 16,000 chars of get_pages content fits a 5-7 page excerpt
// comfortably under any flagship model's context window.
//
// Per-request overrides on /v1/answer/pageindex (max_hops,
// max_pages_per_fetch) win over this block; this block is the
// server-side default.
type PageIndexBlock struct {
	// Enabled toggles the /v1/answer/pageindex endpoint. Default:
	// true. When false, the endpoint returns 501. The
	// PageIndexStrategy itself stays registered as a selection
	// strategy regardless — disabling here only unwires the
	// dedicated answer surface.
	Enabled bool `yaml:"enabled"`

	// MaxHops caps the number of LLM turns one /v1/answer/pageindex
	// request consumes, including the terminal done turn. Default:
	// 8. Set to 0 to use the strategy's built-in default.
	MaxHops int `yaml:"max_hops"`

	// PageContentLimit caps how many chars a single get_pages tool
	// call returns. Default: 16000. Keeps one stray full-document
	// fetch from torching the model's context window.
	PageContentLimit int `yaml:"page_content_limit"`

	// Model overrides the LLM model used for the navigation loop.
	// Empty means use the request's model (which itself falls back
	// to the engine default). Useful when navigation should run on
	// a fast/cheap model while answering benefits from a stronger
	// one — though the PageIndex protocol does both in the same
	// loop, so most deployments leave this blank.
	Model string `yaml:"model"`
}

// AbstainBlock configures the Phase 2.4 abstention behaviour.
//
// When the selection LLM returns per-pick confidence scores and every
// confidence is below Below, the API layer (handleQuery /
// handleAnswer) replaces the normal response with an abstention:
// sections is empty and abstained=true. This refuses to ground an
// answer in evidence the model itself isn't confident is relevant,
// converting a likely hallucination into an honest "I don't know".
//
// Abstention fires only when explicit confidence signal is present.
// Legacy-shape responses (no confidences) always fall through to the
// normal path — the engine never abstains on the absence of signal.
//
// Per-request override: callers may set `enable_abstain` on the
// /v1/query or /v1/answer body to opt out of abstention for one
// request without restarting the server. When this block has
// Enabled=false, no request abstains regardless of the per-request
// flag.
type AbstainBlock struct {
	// Enabled toggles abstention at the server level. Default: true
	// (opt-out).
	Enabled bool `yaml:"enabled"`

	// Below is the confidence threshold. Picks with confidence
	// strictly less than Below are "not confident"; when ALL picks
	// fall below this threshold the response is an abstention.
	// Default: 0.4.
	//
	// The "all" semantics (vs "any") is deliberate: if even one
	// section scored above the threshold, the engine has enough
	// signal to surface it as evidence. Abstention is reserved for
	// the case where every candidate is weak.
	Below float64 `yaml:"below"`
}

// ReplayBlock configures the Phase 3.1 replay-trace store.
//
// When enabled, every /v1/query and /v1/answer response is stamped
// with a deterministic trace_token and the response body is stored
// in an in-memory LRU. Callers can POST /v1/replay with the token
// (plus the original query + document_id) to retrieve the byte-
// identical response.
//
// The store is opt-out — replay is a moat versus stateless vector
// RAG and should ship on by default. Disable to free the memory
// budget when audit/replay isn't part of the operator's flow.
type ReplayBlock struct {
	// Enabled turns the replay store on. Default: true.
	Enabled bool `yaml:"enabled"`

	// MaxEntries bounds the in-memory LRU. Default: 1024.
	MaxEntries int `yaml:"max_entries"`

	// TTLSeconds is how long a replay entry remains valid. Default:
	// 86400 (24h). Long-running audit flows may want to bump this;
	// short-TTL deployments save memory.
	TTLSeconds int `yaml:"ttl_seconds"`
}

// ReRankBlock configures the Phase 2.3 content-aware re-rank pass.
//
// When enabled, every /v1/query and /v1/answer request that returns
// candidate sections runs one extra LLM call: the candidates' first
// MaxContentChars chars of content are fed to the model with the
// query, and the model returns a per-section relevance score
// (0-100). Sections are reordered by score; if TopK > 0 the response
// is truncated to the top K.
//
// The pass is opt-in. Per-request `enable_rerank` body field
// overrides this block.
//
// Re-rank failures never drop sections — at worst the original
// strategy order is preserved and the request returns unchanged from
// the no-rerank path. See pkg/retrieval/rerank.go for the exact
// degradation contract.
type ReRankBlock struct {
	// Enabled toggles re-rank at the server level. Default: false.
	Enabled bool `yaml:"enabled"`

	// Model overrides the re-rank LLM model. Empty means use the
	// request's model (which itself falls back to the engine default).
	// Point this at a small/fast model — the re-rank prompt is short
	// and shouldn't burn the flagship model's budget.
	Model string `yaml:"model"`

	// MaxContentChars caps how many characters of each candidate's
	// content are sent to the model. Default: 2000.
	MaxContentChars int `yaml:"max_content_chars"`

	// TopK caps the number of sections kept after re-ranking. 0 means
	// keep all candidates (re-rank only reorders). Useful when the
	// strategy is configured to return a wide candidate list and the
	// re-rank pass picks the focused top-k for synthesis.
	TopK int `yaml:"top_k"`
}

// PlanningBlock configures Phase 2.1 query planning + Phase 2.2 multi-hop
// decomposition.
//
// When enabled, every /v1/query and /v1/answer request issues one short
// LLM call before retrieval to build a Plan (intent + entities + expected
// document areas + multi-hop flag + sub-questions). On multi-hop plans,
// retrieval fans out one selection call per sub-question and unions the
// results.
//
// Planning is opt-in. The per-request `enable_planning` body field
// overrides this config block; the body field winning lets callers
// experiment without a server restart.
type PlanningBlock struct {
	// Enabled toggles planning at the server level. Default: false.
	Enabled bool `yaml:"enabled"`

	// Model overrides the planner's LLM model. Empty means use the
	// request's model (which itself falls back to the engine default).
	// Point this at a small/fast model — planning is a short prompt
	// that should not run on the same flagship used for synthesis.
	Model string `yaml:"model"`

	// CacheSize bounds the per-process LRU of (query, model) → Plan
	// entries. 0 means use the planner's default (128).
	CacheSize int `yaml:"cache_size"`

	// Decompose toggles Phase 2.2 multi-hop decomposition. When false,
	// planning still runs (and the plan is surfaced in the response)
	// but retrieval always sees the original query — useful for
	// validating the planner in isolation before turning decomposition
	// on. Default: true (when Enabled).
	Decompose bool `yaml:"decompose"`
}

// AnswerSpanBlock configures the answer-span extractor.
//
// When enabled, every section returned by /v1/query gets an extra
// `answer_span` field carrying the verbatim quote the model judged
// most relevant to the query, plus byte offsets back into the
// section's content. Costs one LLM call per returned section.
type AnswerSpanBlock struct {
	// Enabled toggles per-section span extraction on /v1/query. Default: false.
	Enabled bool `yaml:"enabled"`
	// Model overrides the budget's model for the span extraction call.
	// Empty means use the request's model. Keep this on a cheap/fast
	// model (the call is short and runs once per returned section).
	Model string `yaml:"model"`
	// MaxConcurrency caps parallel span-extraction calls per request.
	// Default: 4.
	MaxConcurrency int `yaml:"max_concurrency"`
	// MaxQuoteLen caps the per-section quote length (characters).
	// Default: 400.
	MaxQuoteLen int `yaml:"max_quote_len"`
}

// AnswerBlock configures the /v1/answer endpoint, which runs retrieval
// + span extraction + a synthesis LLM call to return a quote-grounded
// answer in a single round-trip.
type AnswerBlock struct {
	// Model overrides the budget's model for the synthesis call.
	// Empty means use the request's model.
	Model string `yaml:"model"`
	// MaxSections caps how many sections are fed into synthesis.
	// Default: 5.
	MaxSections int `yaml:"max_sections"`
	// MaxAnswerTokens bounds the synthesised answer length.
	// Default: 1024.
	MaxAnswerTokens int `yaml:"max_answer_tokens"`
}

// CacheBlock configures the retrieval-result cache.
type CacheBlock struct {
	// Enabled turns the retrieval cache on. Default: true.
	Enabled bool `yaml:"enabled"`

	// MaxEntries is the maximum number of cached retrieval results.
	// Default: 1024.
	MaxEntries int `yaml:"max_entries"`

	// TTLSeconds is how long (in seconds) a cached result remains valid.
	// Default: 600 (10 minutes).
	TTLSeconds int `yaml:"ttl_seconds"`
}

// ChunkedTreeBlock configures the chunked-tree strategy.
type ChunkedTreeBlock struct {
	MaxTokensPerCall         int  `yaml:"max_tokens_per_call"`
	MaxParallelCalls         int  `yaml:"max_parallel_calls"`
	IncludeSiblingBreadcrumb bool `yaml:"include_sibling_breadcrumbs"`
}

// AgenticBlock configures the agentic-navigation strategy.
//
// The agentic loop trades sequential latency for the ability to handle
// arbitrarily large trees: the model issues outline/expand/read actions
// until it picks a final set of section IDs or hits MaxHops.
type AgenticBlock struct {
	// MaxHops caps the number of LLM turns one query consumes, counting
	// the terminal "done" turn. Default: 6.
	MaxHops int `yaml:"max_hops"`

	// Model optionally overrides the budget's model for navigation
	// turns. Empty means use the budget's model. Useful when the
	// retrieval engine wants the navigation loop on a fast/cheap
	// model while answering is on a stronger one.
	Model string `yaml:"model"`
}

// LogConfig configures logging.
type LogConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Default returns a Config with built-in defaults pre-applied.
func Default() Config {
	return Config{
		Server: ServerConfig{
			Addr:         ":8080",
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 120 * time.Second,
		},
		Database: DatabaseConfig{MaxConns: 10},
		Storage: StorageConfig{
			Driver: "local",
			Local:  LocalStorage{Root: "./data/documents"},
		},
		Queue: QueueConfig{
			Driver: "river",
			River:  RiverBlock{NumWorkers: 10},
			Asynq:  AsynqBlock{Concurrency: 20},
		},
		LLM: LLMConfig{Driver: "anthropic"},
		Retrieval: RetrievalConfig{
			Strategy: "chunked-tree",
			ChunkedTree: ChunkedTreeBlock{
				MaxTokensPerCall:         60000,
				MaxParallelCalls:         8,
				IncludeSiblingBreadcrumb: true,
			},
			Agentic: AgenticBlock{
				MaxHops: 6,
			},
			Cache: CacheBlock{
				Enabled:    true,
				MaxEntries: 1024,
				TTLSeconds: 600,
			},
			AnswerSpan: AnswerSpanBlock{
				Enabled:        false,
				MaxConcurrency: 4,
				MaxQuoteLen:    400,
			},
			Answer: AnswerBlock{
				MaxSections:     5,
				MaxAnswerTokens: 1024,
			},
			Planning: PlanningBlock{
				Enabled:   false,
				CacheSize: 128,
				Decompose: true,
			},
			ReRank: ReRankBlock{
				Enabled:         false,
				MaxContentChars: 2000,
				TopK:            0,
			},
			Replay: ReplayBlock{
				Enabled:    true,
				MaxEntries: 1024,
				TTLSeconds: 86400,
			},
			Abstain: AbstainBlock{
				Enabled: true,
				Below:   0.4,
			},
			PageIndex: PageIndexBlock{
				Enabled:          true,
				MaxHops:          8,
				PageContentLimit: 16000,
			},
		},
		Ingest: IngestConfig{
			Mode:                  "full",
			GlobalLLMConcurrency:  12,
			LLMCallTimeoutSeconds: 90,
			MaxSections:           400,
			ParseTimeoutSeconds:   120,
			HyDE: HyDEConfig{
				Enabled:      true,
				NumQuestions: 5,
				Concurrency:  4,
			},
			Tables: TablesConfig{
				Enabled:            true,
				VerticalStrategy:   "lines",
				HorizontalStrategy: "lines",
				MinTableRows:       2,
				MinTableCols:       2,
			},
			SummaryAxes: SummaryAxesBlock{
				Enabled:     true,
				MaxTopics:   4,
				MaxEntities: 8,
				MaxNumbers:  6,
			},
			TOC: TOCBlock{
				Enabled:       true,
				Concurrency:   4,
				TOCCheckPages: 20,
			},
		},
		Log: LogConfig{Level: "info", Format: "json"},
	}
}

// Load reads configuration from a YAML file (optional) and applies
// environment overrides on top. Pass an empty path to skip the file.
func Load(path string) (Config, error) {
	cfg := Default()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, fmt.Errorf("read config: %w", err)
		}
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config: %w", err)
		}
	}
	applyEnvOverrides(&cfg)
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// firstEnv returns the first non-empty environment variable value from
// the supplied names, checked in order.
func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

func applyEnvOverrides(c *Config) {
	// Minimal, deliberately shallow env handling — production-heavy values
	// that are typically rotated live here. Extend as needed.
	if v := os.Getenv("VLE_DATABASE_URL"); v != "" {
		c.Database.URL = v
	}
	if v := os.Getenv("VLE_SERVER_ADDR"); v != "" {
		c.Server.Addr = v
	}
	if v := os.Getenv("VLE_LOG_LEVEL"); v != "" {
		c.Log.Level = v
	}
	if v := os.Getenv("VLE_ANTHROPIC_API_KEY"); v != "" {
		c.LLM.Anthropic.APIKey = v
	}
	if v := os.Getenv("VLE_OPENAI_API_KEY"); v != "" {
		c.LLM.OpenAI.APIKey = v
	}
	if v := os.Getenv("VLE_GEMINI_API_KEY"); v != "" {
		c.LLM.Gemini.APIKey = v
	}
	// Accept both VLE_-prefixed and bare QSTASH_* env vars. The bare
	// names match what the Upstash console documents and what the
	// dashboard already uses, so ops can set them once for both
	// services. VLE_-prefixed wins if both are set.
	if v := firstEnv("VLE_QSTASH_TOKEN", "QSTASH_TOKEN"); v != "" {
		c.Queue.QStash.Token = v
	}
	if v := firstEnv("VLE_QSTASH_WEBHOOK_BASE_URL", "QSTASH_WEBHOOK_BASE_URL"); v != "" {
		c.Queue.QStash.WebhookBaseURL = v
	}
	if v := firstEnv("VLE_QSTASH_CURRENT_SIGNING_KEY", "QSTASH_CURRENT_SIGNING_KEY"); v != "" {
		c.Queue.QStash.CurrentSigningKey = v
	}
	if v := firstEnv("VLE_QSTASH_NEXT_SIGNING_KEY", "QSTASH_NEXT_SIGNING_KEY"); v != "" {
		c.Queue.QStash.NextSigningKey = v
	}
	// Asynq / Redis env overrides. Accept both VLE_-prefixed and bare
	// REDIS_* names so ops can set them once for multiple services.
	if v := firstEnv("VLE_ASYNQ_ADDR", "REDIS_ADDR"); v != "" {
		c.Queue.Asynq.Addr = v
	}
	if v := firstEnv("VLE_ASYNQ_PASSWORD", "REDIS_PASSWORD"); v != "" {
		c.Queue.Asynq.Password = v
	}
	if v := os.Getenv("VLE_STORAGE_DRIVER"); v != "" {
		c.Storage.Driver = v
	}
	if v := os.Getenv("VLE_QUEUE_DRIVER"); v != "" {
		c.Queue.Driver = v
	}
	if v := os.Getenv("VLE_LLM_DRIVER"); v != "" {
		c.LLM.Driver = v
	}
	// Anthropic-driver overrides. These let an operator point the
	// anthropic driver at an Anthropic-compatible gateway (e.g. GLM via
	// https://api.z.ai/api/anthropic) without baking the values into the
	// config file or secret.
	if v := os.Getenv("VLE_LLM_ANTHROPIC_API_KEY"); v != "" {
		c.LLM.Anthropic.APIKey = v
	}
	if v := os.Getenv("VLE_LLM_ANTHROPIC_BASE_URL"); v != "" {
		c.LLM.Anthropic.BaseURL = v
	}
	if v := os.Getenv("VLE_LLM_ANTHROPIC_MODEL"); v != "" {
		c.LLM.Anthropic.Model = v
	}
	if v := os.Getenv("VLE_TLS_CERT_FILE"); v != "" {
		c.Server.TLS.CertFile = v
	}
	if v := os.Getenv("VLE_TLS_KEY_FILE"); v != "" {
		c.Server.TLS.KeyFile = v
	}
	if v := os.Getenv("VLE_RETRIEVAL_AGENTIC_MAX_HOPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Retrieval.Agentic.MaxHops = n
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_AGENTIC_MODEL"); v != "" {
		c.Retrieval.Agentic.Model = v
	}
	// Ingest mode switch (full | minimal). A single env var flips the
	// engine into fast/minimal ingest with no secret edit.
	if v := os.Getenv("VLE_INGEST_MODE"); v != "" {
		c.Ingest.Mode = v
	}
	// Ingest / HyDE knobs. Booleans accept the usual truthy strings —
	// kept narrow so a typo doesn't silently flip the flag.
	if v := os.Getenv("VLE_INGEST_HYDE_ENABLED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			c.Ingest.HyDE.Enabled = true
		case "0", "false", "no", "off":
			c.Ingest.HyDE.Enabled = false
		}
	}
	if v := os.Getenv("VLE_INGEST_HYDE_MODEL"); v != "" {
		c.Ingest.HyDE.Model = v
	}
	if v := os.Getenv("VLE_INGEST_HYDE_NUM_QUESTIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Ingest.HyDE.NumQuestions = n
		}
	}
	if v := os.Getenv("VLE_INGEST_HYDE_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Ingest.HyDE.Concurrency = n
		}
	}
	if v := os.Getenv("VLE_INGEST_GLOBAL_LLM_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Ingest.GlobalLLMConcurrency = n
		}
	}
	if v := os.Getenv("VLE_INGEST_LLM_CALL_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Ingest.LLMCallTimeoutSeconds = n
		}
	}
	if v := os.Getenv("VLE_INGEST_MAX_SECTIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Ingest.MaxSections = n
		}
	}
	if v := os.Getenv("VLE_INGEST_PARSE_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Ingest.ParseTimeoutSeconds = n
		}
	}
	// pdftable-driven table extraction.
	if v := os.Getenv("VLE_INGEST_TABLES_ENABLED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			c.Ingest.Tables.Enabled = true
		case "0", "false", "no", "off":
			c.Ingest.Tables.Enabled = false
		}
	}
	if v := os.Getenv("VLE_INGEST_TABLES_VERTICAL_STRATEGY"); v != "" {
		c.Ingest.Tables.VerticalStrategy = v
	}
	if v := os.Getenv("VLE_INGEST_TABLES_HORIZONTAL_STRATEGY"); v != "" {
		c.Ingest.Tables.HorizontalStrategy = v
	}
	if v := os.Getenv("VLE_INGEST_TABLES_MIN_ROWS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Ingest.Tables.MinTableRows = n
		}
	}
	if v := os.Getenv("VLE_INGEST_TABLES_MIN_COLS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Ingest.Tables.MinTableCols = n
		}
	}
	// Phase 2.5 structured-summary knobs. Booleans accept the same
	// truthy strings the other ingest toggles use; numeric overrides
	// require a positive int (a typo silently preserves the default).
	if v := os.Getenv("VLE_INGEST_SUMMARY_AXES_ENABLED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			c.Ingest.SummaryAxes.Enabled = true
		case "0", "false", "no", "off":
			c.Ingest.SummaryAxes.Enabled = false
		}
	}
	if v := os.Getenv("VLE_INGEST_SUMMARY_AXES_MAX_TOPICS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Ingest.SummaryAxes.MaxTopics = n
		}
	}
	if v := os.Getenv("VLE_INGEST_SUMMARY_AXES_MAX_ENTITIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Ingest.SummaryAxes.MaxEntities = n
		}
	}
	if v := os.Getenv("VLE_INGEST_SUMMARY_AXES_MAX_NUMBERS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Ingest.SummaryAxes.MaxNumbers = n
		}
	}
	// LLM-built TOC tree (PageIndex-style). Same truthy-string set
	// as the other ingest toggles; numeric overrides require a
	// positive int so a typo doesn't silently flip the default.
	if v := os.Getenv("VLE_INGEST_TOC_ENABLED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			c.Ingest.TOC.Enabled = true
		case "0", "false", "no", "off":
			c.Ingest.TOC.Enabled = false
		}
	}
	if v := os.Getenv("VLE_INGEST_TOC_MODEL"); v != "" {
		c.Ingest.TOC.Model = v
	}
	if v := os.Getenv("VLE_INGEST_TOC_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Ingest.TOC.Concurrency = n
		}
	}
	if v := os.Getenv("VLE_INGEST_TOC_TOC_CHECK_PAGES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Ingest.TOC.TOCCheckPages = n
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_ANSWER_SPAN_ENABLED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			c.Retrieval.AnswerSpan.Enabled = true
		case "0", "false", "no", "off":
			c.Retrieval.AnswerSpan.Enabled = false
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_ANSWER_SPAN_MODEL"); v != "" {
		c.Retrieval.AnswerSpan.Model = v
	}
	if v := os.Getenv("VLE_RETRIEVAL_ANSWER_SPAN_MAX_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Retrieval.AnswerSpan.MaxConcurrency = n
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_ANSWER_MODEL"); v != "" {
		c.Retrieval.Answer.Model = v
	}
	if v := os.Getenv("VLE_RETRIEVAL_ANSWER_MAX_SECTIONS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Retrieval.Answer.MaxSections = n
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_PLANNING_ENABLED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			c.Retrieval.Planning.Enabled = true
		case "0", "false", "no", "off":
			c.Retrieval.Planning.Enabled = false
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_PLANNING_MODEL"); v != "" {
		c.Retrieval.Planning.Model = v
	}
	if v := os.Getenv("VLE_RETRIEVAL_PLANNING_CACHE_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Retrieval.Planning.CacheSize = n
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_PLANNING_DECOMPOSE"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			c.Retrieval.Planning.Decompose = true
		case "0", "false", "no", "off":
			c.Retrieval.Planning.Decompose = false
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_RERANK_ENABLED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			c.Retrieval.ReRank.Enabled = true
		case "0", "false", "no", "off":
			c.Retrieval.ReRank.Enabled = false
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_RERANK_MODEL"); v != "" {
		c.Retrieval.ReRank.Model = v
	}
	if v := os.Getenv("VLE_RETRIEVAL_RERANK_MAX_CONTENT_CHARS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Retrieval.ReRank.MaxContentChars = n
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_RERANK_TOP_K"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Retrieval.ReRank.TopK = n
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_REPLAY_ENABLED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			c.Retrieval.Replay.Enabled = true
		case "0", "false", "no", "off":
			c.Retrieval.Replay.Enabled = false
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_REPLAY_MAX_ENTRIES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Retrieval.Replay.MaxEntries = n
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_REPLAY_TTL_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.Retrieval.Replay.TTLSeconds = n
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_ABSTAIN_ENABLED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			c.Retrieval.Abstain.Enabled = true
		case "0", "false", "no", "off":
			c.Retrieval.Abstain.Enabled = false
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_ABSTAIN_BELOW"); v != "" {
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f >= 0 && f <= 1 {
			c.Retrieval.Abstain.Below = f
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_PAGEINDEX_ENABLED"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			c.Retrieval.PageIndex.Enabled = true
		case "0", "false", "no", "off":
			c.Retrieval.PageIndex.Enabled = false
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_PAGEINDEX_MAX_HOPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Retrieval.PageIndex.MaxHops = n
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_PAGEINDEX_PAGE_CONTENT_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			c.Retrieval.PageIndex.PageContentLimit = n
		}
	}
	if v := os.Getenv("VLE_RETRIEVAL_PAGEINDEX_MODEL"); v != "" {
		c.Retrieval.PageIndex.Model = v
	}
}

// Validate checks that required fields for the selected drivers are set.
func (c Config) Validate() error {
	switch c.Storage.Driver {
	case "local":
		if c.Storage.Local.Root == "" {
			return errors.New("storage.local.root is required when driver=local")
		}
	case "s3":
		if c.Storage.S3.Bucket == "" || c.Storage.S3.Endpoint == "" {
			return errors.New("storage.s3 requires bucket and endpoint")
		}
	case "gcs":
		if c.Storage.GCS.Bucket == "" {
			return errors.New("storage.gcs.bucket is required when driver=gcs")
		}
	default:
		return fmt.Errorf("unknown storage.driver: %q", c.Storage.Driver)
	}

	switch c.Queue.Driver {
	case "qstash":
		if c.Queue.QStash.Token == "" {
			return errors.New("queue.qstash.token is required when driver=qstash")
		}
		if c.Queue.QStash.WebhookBaseURL == "" {
			return errors.New("queue.qstash.webhook_base_url is required when driver=qstash")
		}
		if c.Queue.QStash.CurrentSigningKey == "" {
			return errors.New("queue.qstash.current_signing_key is required when driver=qstash")
		}
	case "river":
		if c.Database.URL == "" {
			return errors.New("database.url is required when queue.driver=river")
		}
	case "asynq":
		if c.Queue.Asynq.Addr == "" {
			return errors.New("queue.asynq.addr is required when driver=asynq")
		}
	default:
		return fmt.Errorf("unknown queue.driver: %q", c.Queue.Driver)
	}

	switch c.LLM.Driver {
	case "anthropic", "openai", "gemini":
		// API keys are checked lazily at first call so the engine can boot
		// in dev without all providers configured.
	default:
		return fmt.Errorf("unknown llm.driver: %q", c.LLM.Driver)
	}

	switch c.Retrieval.Strategy {
	case "single-pass", "chunked-tree", "agentic", "pageindex":
	default:
		return fmt.Errorf("unknown retrieval.strategy: %q", c.Retrieval.Strategy)
	}

	if c.Retrieval.Agentic.MaxHops < 0 {
		return fmt.Errorf("retrieval.agentic.max_hops must be >= 0, got %d", c.Retrieval.Agentic.MaxHops)
	}

	if c.Server.TLS.CertFile != "" && c.Server.TLS.KeyFile == "" {
		return errors.New("server.tls.key_file is required when cert_file is set")
	}
	if c.Server.TLS.KeyFile != "" && c.Server.TLS.CertFile == "" {
		return errors.New("server.tls.cert_file is required when key_file is set")
	}
	if v := c.Server.TLS.MinVersion; v != "" && v != "1.2" && v != "1.3" {
		return fmt.Errorf("server.tls.min_version must be 1.2 or 1.3, got %q", v)
	}

	switch c.Ingest.Mode {
	case "", "full", "minimal":
	default:
		return fmt.Errorf("ingest.mode must be one of full|minimal, got %q", c.Ingest.Mode)
	}

	if c.Ingest.HyDE.NumQuestions < 0 {
		return fmt.Errorf("ingest.hyde.num_questions must be >= 0, got %d", c.Ingest.HyDE.NumQuestions)
	}
	if c.Ingest.HyDE.Concurrency < 0 {
		return fmt.Errorf("ingest.hyde.concurrency must be >= 0, got %d", c.Ingest.HyDE.Concurrency)
	}
	if c.Ingest.GlobalLLMConcurrency < 0 {
		return fmt.Errorf("ingest.global_llm_concurrency must be >= 0, got %d", c.Ingest.GlobalLLMConcurrency)
	}
	if c.Ingest.LLMCallTimeoutSeconds < 0 {
		return fmt.Errorf("ingest.llm_call_timeout_seconds must be >= 0, got %d", c.Ingest.LLMCallTimeoutSeconds)
	}
	if c.Ingest.MaxSections < 0 {
		return fmt.Errorf("ingest.max_sections must be >= 0, got %d", c.Ingest.MaxSections)
	}
	if c.Ingest.ParseTimeoutSeconds < 0 {
		return fmt.Errorf("ingest.parse_timeout_seconds must be >= 0, got %d", c.Ingest.ParseTimeoutSeconds)
	}

	switch c.Ingest.Tables.VerticalStrategy {
	case "", "lines", "lines_strict", "text", "explicit":
	default:
		return fmt.Errorf("ingest.tables.vertical_strategy must be one of lines|lines_strict|text|explicit, got %q",
			c.Ingest.Tables.VerticalStrategy)
	}
	switch c.Ingest.Tables.HorizontalStrategy {
	case "", "lines", "lines_strict", "text", "explicit":
	default:
		return fmt.Errorf("ingest.tables.horizontal_strategy must be one of lines|lines_strict|text|explicit, got %q",
			c.Ingest.Tables.HorizontalStrategy)
	}
	if c.Ingest.Tables.MinTableRows < 0 {
		return fmt.Errorf("ingest.tables.min_table_rows must be >= 0, got %d", c.Ingest.Tables.MinTableRows)
	}
	if c.Ingest.Tables.MinTableCols < 0 {
		return fmt.Errorf("ingest.tables.min_table_cols must be >= 0, got %d", c.Ingest.Tables.MinTableCols)
	}

	if c.Ingest.SummaryAxes.MaxTopics < 0 {
		return fmt.Errorf("ingest.summary_axes.max_topics must be >= 0, got %d", c.Ingest.SummaryAxes.MaxTopics)
	}
	if c.Ingest.SummaryAxes.MaxEntities < 0 {
		return fmt.Errorf("ingest.summary_axes.max_entities must be >= 0, got %d", c.Ingest.SummaryAxes.MaxEntities)
	}
	if c.Ingest.SummaryAxes.MaxNumbers < 0 {
		return fmt.Errorf("ingest.summary_axes.max_numbers must be >= 0, got %d", c.Ingest.SummaryAxes.MaxNumbers)
	}

	if c.Ingest.TOC.Concurrency < 0 {
		return fmt.Errorf("ingest.toc.concurrency must be >= 0, got %d", c.Ingest.TOC.Concurrency)
	}
	if c.Ingest.TOC.TOCCheckPages < 0 {
		return fmt.Errorf("ingest.toc.toc_check_pages must be >= 0, got %d", c.Ingest.TOC.TOCCheckPages)
	}

	if c.Retrieval.Planning.CacheSize < 0 {
		return fmt.Errorf("retrieval.planning.cache_size must be >= 0, got %d", c.Retrieval.Planning.CacheSize)
	}

	if c.Retrieval.ReRank.MaxContentChars < 0 {
		return fmt.Errorf("retrieval.rerank.max_content_chars must be >= 0, got %d", c.Retrieval.ReRank.MaxContentChars)
	}
	if c.Retrieval.ReRank.TopK < 0 {
		return fmt.Errorf("retrieval.rerank.top_k must be >= 0, got %d", c.Retrieval.ReRank.TopK)
	}

	if c.Retrieval.Replay.MaxEntries < 0 {
		return fmt.Errorf("retrieval.replay.max_entries must be >= 0, got %d", c.Retrieval.Replay.MaxEntries)
	}
	if c.Retrieval.Replay.TTLSeconds < 0 {
		return fmt.Errorf("retrieval.replay.ttl_seconds must be >= 0, got %d", c.Retrieval.Replay.TTLSeconds)
	}

	if c.Retrieval.Abstain.Below < 0 || c.Retrieval.Abstain.Below > 1 {
		return fmt.Errorf("retrieval.abstain.below must be in [0.0, 1.0], got %v", c.Retrieval.Abstain.Below)
	}

	if c.Retrieval.PageIndex.MaxHops < 0 {
		return fmt.Errorf("retrieval.pageindex.max_hops must be >= 0, got %d", c.Retrieval.PageIndex.MaxHops)
	}
	if c.Retrieval.PageIndex.PageContentLimit < 0 {
		return fmt.Errorf("retrieval.pageindex.page_content_limit must be >= 0, got %d", c.Retrieval.PageIndex.PageContentLimit)
	}

	return nil
}
