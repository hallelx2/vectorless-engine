# Engine

> The core vectorless retrieval engine — a Go library that doubles as a
> long-running daemon.

## Purpose

Turn documents into hierarchical trees of titles + summaries, then
answer queries by reasoning over those trees with an LLM. No vectors,
no embeddings, no similarity index. The tree *is* the index.

## What the engine does

- Accept a document (Markdown, HTML, DOCX, PDF, plain text).
- Parse it into a hierarchy of sections, preserving structure.
- Store the raw bytes in object storage (S3-compatible).
- Store the tree metadata (titles, summaries, token counts) in
  Postgres.
- Summarise each section with an LLM call.
- Answer a natural-language query by running a **retrieval strategy**
  against the tree and returning the relevant section IDs + content.

## What the engine does not do

- **Authentication.** Zero concept of users, orgs, keys. The engine
  trusts whoever calls it. Auth is the server's and control plane's
  problem.
- **Billing or quota enforcement.** That lives in the control plane.
- **HTTP serving.** The engine is a library. A separate server package
  wraps it for network transport. See [SERVER.md](./SERVER.md).
- **Embedding-based retrieval.** By design. If the tree doesn't work
  for a use case, vectorless is not the right tool.
- **Content rendering.** The engine returns section text, not formatted
  HTML or Markdown for display. The calling app decides how to show it.

## The two retrieval strategies

### `single-pass`

Feed the whole tree view to the model in one call. Model returns the
IDs it thinks are relevant. Budget-friendly, fast, works great when
the document tree fits in the model's context window.

### `chunked-tree`

When the tree is too big for one call, slice it into budget-sized
chunks, reason over each in parallel, merge the picks.

Pipeline:

```
Split(tree, budget)        -> []Slice
for each slice in parallel -> LLM.Select -> []SectionID
Merge(results, policy)     -> []SectionID
```

Each slice carries a breadcrumb (`Document X -> Part II -> slice 3 of
12`) so the model knows where it is relative to siblings it cannot see.
Each slice also filters the model's picks against its own section IDs
so the model cannot fabricate IDs from other slices.

Merge policies:

- **Union** (default) — any ID picked by any slice is included, sorted.
- `TopN(ranked)` and `Vote(k-of-n)` are deferred.

## Package layout

```
pkg/
  tree/         - the core Tree / Section / View data model
  parser/       - parser interface + Markdown / HTML / DOCX / PDF / Text
  ingest/       - parse -> persist -> summarise pipeline
  retrieval/    - Strategy interface + SinglePass + ChunkedTree + Splitter
  storage/      - Storage interface + Local + S3 drivers
  queue/        - Queue interface + River + Asynq + QStash drivers
  db/           - Postgres pool + embedded migrations + CRUD
  llm/          - Client interface, delegated to llmgate (external)
  api/          - HTTP + gRPC handlers (will move to vectorless-server repo)
cmd/
  engine/       - the binary entry point with subcommands
```

`pkg/` is the promise: these packages are importable by external Go
code. `internal/` may still exist for things we explicitly don't want
anyone depending on yet.

## CLI subcommands

The engine ships as a single binary with cobra-style subcommands.

```
vectorless-engine server       # boot HTTP + gRPC + embedded workers (dev / small deploys)
vectorless-engine worker       # queue workers only (scale horizontally)
vectorless-engine migrate      # run DB migrations explicitly
vectorless-engine ingest FILE  # one-shot: ingest a local file for testing
vectorless-engine query ID Q   # one-shot: query a doc from the CLI
vectorless-engine version      # print version + git SHA + build time
vectorless-engine config print # print the effective config (with secrets redacted)
vectorless-engine config check # validate config and exit 0/1
```

Production deployment pattern: the same image runs twice under
different commands — `server` behind a load balancer, `worker` on an
autoscaler driven by queue depth. No separate images to keep in sync.

## Configuration

The engine is configured through **three layers** that compose
cleanly. Every knob is reachable from every layer; later layers
override earlier ones.

### Precedence (low -> high)

1. **Built-in defaults** — compiled into the binary. Safe for
   `vectorless-engine server` to boot with zero config and a local
   Postgres + local storage.
2. **YAML config file** — loaded from `--config` (default
   `./config.yaml` if present, else skipped).
3. **Environment variables** — `VLE_*`, dot path flattened with
   underscores (e.g. `server.tls.cert_file` -> `VLE_SERVER_TLS_CERT_FILE`).
4. **Command-line flags** — highest priority. Great for ad-hoc
   overrides (`--log.level=debug`) and for container orchestrators
   that prefer args over env.

A later layer overrides only the specific keys it sets; it does not
replace whole sub-trees. So you can ship a YAML file in the image,
set secrets via env, and tweak `--log.level=debug` per run.

### Flag naming

Flags mirror the YAML tree, dot-separated:

```
--server.addr=:8080
--server.tls.cert_file=/etc/tls/cert.pem
--database.url=postgres://...
--storage.driver=s3
--storage.s3.bucket=my-bucket
--queue.driver=asynq
--queue.asynq.addr=redis:6379
--llm.driver=anthropic
--llm.anthropic.model=claude-sonnet-4-5
--retrieval.strategy=chunked-tree
--retrieval.chunked_tree.max_parallel_calls=16
--log.level=debug
--log.format=console
```

Boolean flags accept `--flag` / `--flag=false`. Durations accept
Go's `time.ParseDuration` form (`30s`, `2m`, `1h`). Secrets should
normally come from env or a YAML file mounted as a secret — flags
end up in process listings.

Meta flags:

```
--config=/etc/vectorless/config.yaml   # override config file path
--config.print                         # print effective config and exit
--config.check                         # validate and exit 0/1
```

### The one-liner rule

Any single deployment should be reproducible with either:

- A YAML file + secrets in env, *or*
- A single `vectorless-engine server --...` invocation with flags.

Both must produce an identical effective config. `config print`
makes this easy to verify in CI.

### Implementation

`cobra` for the command tree, `pflag` for flag definitions, and a
thin merger that walks the struct tree. We deliberately do **not**
pull in `viper` — its magic reload, config-watching, and remote
backends are features we don't need and complicate testing. A ~200
LOC merger over `mapstructure` decode is plenty.

### Config validation

`config.Validate()` runs after the merge and before anything else
boots. It checks:

- Required fields present (`database.url`, an LLM api key if the
  driver needs one).
- Driver + subsection match (e.g. `storage.driver=s3` requires
  `storage.s3.bucket`).
- Mutually-exclusive fields (TLS cert/key are both set or neither).
- Resource sanity (`max_conns > 0`, timeouts > 0).

Validation failures print which layer provided the bad value so the
user knows where to fix it (`server.addr=:abc (from --server.addr)`).

## Public interfaces

These are the contracts the engine exposes to anyone embedding it.
They are small on purpose. Keep them small.

```go
// Ingest a document.
ingest.Pipeline{ DB, Storage, LLM, Parsers, Logger }.Run(ctx, Payload) error

// Run a retrieval strategy.
retrieval.Strategy interface {
    Name() string
    Select(ctx, *tree.Tree, query string, ContextBudget) ([]tree.SectionID, error)
}

// LLM access (delegated to llmgate).
llm.Client interface {
    Complete(ctx, Request) (*Response, error)
    CountTokens(ctx, text string) (int, error)
}

// Pluggable storage / queue / DB — all driver-based.
storage.Storage
queue.Queue
db.Pool
```

Everything else is either `pkg/tree` types (plain data) or subsystem
internals.

## Tech choices and why

- **Go 1.25+** — modern stdlib (`slog`, `signal.NotifyContext`,
  `errors.Is/As`), single binary, cross-compile, goroutine concurrency.
- **chi** for HTTP routing — idiomatic, zero-dep, plays well with
  stdlib `http.Handler`.
- **pgx/v5 + pgxpool** — the one Postgres driver worth using in Go.
  Binary protocol, typed params, proper JSONB support, connection
  pooling.
- **Embedded SQL migrations** via `//go:embed`. No Atlas, no goose, no
  Flyway. Migration is ten lines of Go; external tools are overkill.
- **ledongthuc/pdf** for PDF — pure Go, no cgo, cross-compiles cleanly.
  Trade-off: no OCR, no encrypted PDFs. Deferred to Phase 2+.
- **goldmark** for Markdown — the Go community's standard, actively
  maintained.
- **`golang.org/x/net/html`** for HTML — stdlib-ish, no third-party dep.
- **`archive/zip + encoding/xml`** for DOCX — pure stdlib, no
  unidoc/gooxml dependency.
- **`errgroup` + semaphore** for parallel work — stdlib-first, no
  workerpool library needed.

## Subsystem boundaries

Each `pkg/*` subsystem owns one concern and speaks to the rest only
through its public interface. No package imports another's internals.

### `pkg/tree`

The data model. `Tree`, `Section`, `SectionID`, `View`. Pure types
and traversal helpers — no DB, no LLM, no IO. Everything else in the
engine passes these around.

### `pkg/parser`

`Parser` interface + `Registry` that routes by content-type /
extension. One parser per format (markdown, html, docx, pdf, text).
Parsers return a `*tree.Tree`; they never touch storage or the DB.
Adding a format = implementing `Parser` + registering it.

### `pkg/ingest`

Orchestrates **parse -> persist tree -> summarise**. Every stage is
idempotent so a queue retry is safe. The pipeline owns the
lifecycle transitions on the `documents` row
(`pending -> parsing -> summarizing -> ready | failed`).

Degradation rules:
- Parser fails -> `failed`, `error_message` set, pipeline exits.
- Summariser fails on a section -> use a truncated excerpt, mark
  the summary as a fallback, continue.
- Storage write fails -> retry via queue; document stays in its
  current status.

### `pkg/retrieval`

`Strategy` interface + `SinglePass` + `ChunkedTree` + `Splitter` +
merge policies. Takes a `*tree.Tree` and a query, returns section
IDs. Calls `llm.Client` through the interface; never imports a
concrete provider.

### `pkg/storage`

`Storage` interface + drivers (`local`, `s3`). Content-addressed
keys. Optional `SignedURL` for drivers that support it. Stores raw
document bytes + per-section content blobs; never stores structure.

### `pkg/queue`

`Queue` interface + drivers (`river`, `asynq`, `qstash`). Jobs are
registered by `Kind`. The engine ships two jobs today:
`ingest_document` and (reserved) `reprocess_document`. The queue
driver decides retries, backoff, dead-lettering.

### `pkg/db`

`pgxpool` wrapper + embedded migrations + hand-written CRUD. Owns
schema. No other package issues SQL. Migrations run at boot (or via
`vectorless-engine migrate`). Failing to reach Postgres is a
fatal boot error — there's no point starting without state.

### `pkg/llm`

Thin facade that delegates to `llmgate`. `Client` interface +
`Request` / `Response` / `Message` / `Usage` types. The engine
depends on this interface, never on a vendor SDK.

### `pkg/api` (moves to `vectorless-server`)

HTTP/gRPC wrappers around the subsystems above. Included today for
convenience; leaves the engine repo when the server extracts.

## Request lifecycle

### Ingest

```
POST /v1/documents
  |
  v
api: multipart/JSON decode, size check
  |
  v
db: INSERT documents row (status=pending)
storage: write raw bytes (content-addressed key)
queue: enqueue ingest_document{document_id}
  |
  v
202 Accepted  {id, status}

(async, in a worker)
  |
  v
ingest: load row, status=parsing
parser: Registry.ParseFor(content_type).Parse(bytes) -> *tree.Tree
db: UpsertSection * N (outline only)
  |
  v
ingest: status=summarizing
llm: summarise each section (sequential today, parallel Phase 3+)
db: UpdateSectionSummary per section
storage: write per-section content blob if large
  |
  v
ingest: status=ready
```

### Query

```
POST /v1/query  {document_id, query, model?, ...}
  |
  v
api: decode, validate
db: LoadTree(document_id) -> *tree.Tree
  |
  v
retrieval.Strategy.Select(ctx, tree, query, budget) -> []SectionID
  (may fan out to N LLM calls via errgroup + semaphore)
  |
  v
storage: fetch section content for each selected ID
  |
  v
api: assemble response {sections, strategy, model, elapsed_ms}
```

## Deployment topologies

The same binary supports multiple shapes. Pick the one that matches
your scale.

### All-in-one (`server` subcommand)

One process runs HTTP + embedded workers. Good for dev,
self-hosters, and small single-node deploys. Trade-off: a long
summariser call can starve request handling if workers are on the
same goroutines. (They aren't, but tune `queue.*.concurrency` so
ingest doesn't hog the machine.)

### Split (`server` + `worker` subcommands)

Two deployments of the same image:
- `vectorless-engine server` behind a load balancer, N replicas.
- `vectorless-engine worker` on an autoscaler driven by queue
  depth, M replicas.

Both point at the same Postgres + storage + queue + LLM gateway.
This is the production default. Workers can scale independently
during heavy ingest; request-serving replicas stay lean.

### Library mode (embedded)

Another Go program imports `pkg/ingest` and `pkg/retrieval`
directly and skips the HTTP layer entirely. Useful for batch jobs,
custom CLIs, and internal tools that already have their own
transport. Auth is the host program's responsibility.

## Observability hooks

The engine ships observability primitives; the server exposes them.

- **Structured logs** via `slog`. Every request / job / LLM call
  emits one line with: `request_id`, `document_id` (where
  relevant), `duration_ms`, `status`.
- **Metrics** via a `metrics.Recorder` interface (default no-op;
  Prometheus impl in the server). Counters: requests, errors,
  ingest jobs by status, LLM calls by provider, tokens in/out.
  Histograms: request duration, ingest duration, LLM call duration.
- **Tracing** via OpenTelemetry. Root span per request / job.
  Child spans: `parser`, `db`, `storage`, `llm`. Trace IDs land in
  log lines via context propagation.

All three are **opt-in** via config. Zero overhead when off.

## Error taxonomy

Errors are sentinel-based and wrappable. Callers use `errors.Is`
to branch.

- `ingest.ErrUnsupportedContent` — no parser registered.
- `parser.ErrMalformed` — bytes aren't valid for the claimed type.
- `storage.ErrNotFound` / `storage.ErrPermission`.
- `db.ErrNotFound` — row missing where one was expected.
- `llm.ErrRateLimited` / `llm.ErrProviderDown` / `llm.ErrBadRequest`.
- `retrieval.ErrNoSelection` — strategy ran but found nothing.

Server maps these to HTTP status codes; the engine doesn't know or
care about status codes.

## Testing strategy

- **Unit**: every `pkg/*` has its own tests. Parsers use
  table-driven cases and assemble their own fixtures in memory
  (no committed binaries). Retrieval uses a mock `llm.Client`.
- **Integration**: a `docker-compose` harness starts Postgres +
  MinIO + the engine and runs end-to-end ingest -> query. Gated
  behind `ENGINE_INTEGRATION_TESTS=1` so `go test ./...` stays
  fast by default.
- **Live LLM**: a separate integration target that hits real
  Anthropic/OpenAI with tiny prompts. Run in CI on a nightly cron,
  not on every PR.
- **Fuzz**: parsers get `go test -fuzz` runs in CI weekly —
  malformed markdown, truncated PDFs, XML bombs in DOCX.
- **Coverage target**: 70% on `pkg/retrieval`, `pkg/ingest`,
  `pkg/db`, `pkg/parser`. Lower is acceptable on glue code.

## Parallelism model

- One goroutine per HTTP request (chi default).
- One goroutine per queue job (queue driver manages the pool).
- Within a strategy: `errgroup.WithContext` + buffered semaphore
  channel to cap parallel LLM calls at `MaxParallelCalls`.
- The engine is pure-IO bound (DB, S3, LLM API). Parallelism is always
  network-bound, never CPU-bound.

## Failure modes and recovery

- **Parser fails** -> document row moves to `failed` status,
  `error_message` is populated, pipeline exits cleanly.
- **LLM fails during summarisation** -> fall back to a truncated
  excerpt so ingest still completes. Section summary marked with a
  hint that it's a fallback.
- **LLM fails during query** -> propagate to caller. No fallback here;
  the caller needs to know the query didn't actually run.
- **Queue worker crashes** -> job is redelivered by the queue driver
  up to its retry budget. Every pipeline stage is idempotent.
- **Postgres unavailable at startup** -> engine fails to boot. No
  point starting without state.

## What lives in the engine vs the server

The split matters. Keep it clean.

| Concern | Engine | Server |
|---|---|---|
| Parse / ingest / retrieve | Yes | No |
| HTTP routing | No | Yes |
| gRPC handlers | No | Yes |
| API key middleware | No | Yes |
| Request / response JSON serialisation | No | Yes |
| Queue registration + dispatch | Yes | No |
| Worker execution | Yes | No |
| Graceful shutdown for workers | Yes | No |
| Graceful shutdown for HTTP | No | Yes |

Rule of thumb: if it can be called from another Go program without
HTTP, it belongs in the engine. If it wraps HTTP or gRPC, it belongs
in the server.

## Open questions

- **Incremental re-ingest.** When a document is re-uploaded, today we
  reprocess it fully. Detecting changed sections and preserving stable
  IDs for unchanged ones is Phase 4 work but worth designing for.
- **Tree compaction.** Merging adjacent tiny leaves into a single
  section for more efficient reasoning. Heuristic + token-threshold
  driven.
- **Streaming queries.** Today `/v1/query` blocks until the strategy
  finishes. SSE would let us emit section picks as they land.

## Related docs

- [SERVER.md](./SERVER.md) — the HTTP/gRPC layer on top.
- [LLMGATE.md](./LLMGATE.md) — how the engine talks to LLMs.
- [DATA.md](./DATA.md) — what lives in Postgres vs S3.
- The root `ROADMAP.md` — phase-by-phase task tracking.
