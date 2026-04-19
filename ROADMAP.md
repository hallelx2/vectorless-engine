# vectorless-engine — roadmap

Living document. Tick boxes as things land. Edit freely; this is the source of truth for "what's done, what's next."

**Legend:** `[x]` done · `[~]` in progress · `[ ]` not started · `[?]` idea, not committed · `(opt)` optional polish

---

## Phase 0 — scaffold *(shipped)*

One-line: get a single Go binary that builds, boots, and serves an HTTP surface.

- [x] Go module (`github.com/hallelx2/vectorless-engine`, Go 1.25+)
- [x] `cmd/engine` entry point with graceful shutdown (`signal.NotifyContext`, 15s drain)
- [x] Structured logging (`slog`, JSON + console handlers)
- [x] `config` package — YAML + `VLE_*` env overrides + `Validate()`
- [x] HTTP layer (chi router, RequestID / RealIP / Recoverer middleware)
- [x] Pluggable interfaces: `storage.Storage`, `queue.Queue`, `llm.Client`, `retrieval.Strategy`
- [x] Driver stubs for: local / S3 storage · QStash / River / Asynq queue · Anthropic / OpenAI / Gemini LLM
- [x] `tree` package — core `Tree` / `Section` / `View` model
- [x] Dockerfile (multi-stage, distroless) + `docker-compose.yml` (Postgres / Redis / MinIO)
- [x] Apache 2.0 license, README with badges and SVG diagrams
- [x] GitHub repo created, `main` pushed, topics added

---

## Phase 1 — ingest *(shipped)*

One-line: raw bytes → queryable, persisted tree.

- [x] **Database layer**
  - [x] `pgxpool` wrapper in `internal/db` with `Open()` + ping
  - [x] Embedded SQL migrations + auto-apply at boot (`schema_migrations` tracked)
  - [x] Schema: `documents` (lifecycle: pending → parsing → summarizing → ready | failed) + `sections` (self-referential tree)
  - [x] CRUD helpers: `NewDocument`, `GetDocument`, `SetDocumentStatus`, `SetDocumentTitle`, `DeleteDocument`, `UpsertSection`, `UpdateSectionSummary`, `GetSection`, `ListSections`, `LoadTree`, `ListDocuments`
  - [ ] (opt) sqlc migration — queries are hand-written right now; revisit once schema stabilizes

- [x] **Parser subsystem**
  - [x] `Parser` interface + `Registry` that routes by content-type / extension
  - [x] **Markdown** (goldmark, ATX+Setext headings → level-stack hierarchy)
  - [x] **HTML** (`golang.org/x/net/html`, prefers `<main>`/`<article>`, strips chrome)
  - [x] **DOCX** (stdlib `archive/zip` + `encoding/xml`, detects `Heading 1…9` + `Title` styles — both `Heading2` and `Heading 2` spellings)
  - [x] **PDF** (`ledongthuc/pdf`, pure Go no cgo)
    - [x] Font-size heuristic for unstructured PDFs
    - [x] `/Outlines` ground truth when bookmarks exist, with text-matching fallback (< 50% match ⇒ fall back)
    - [ ] (opt) OCR for scanned PDFs (Tesseract via shell-out, or LLM vision call)
    - [ ] (opt) Encrypted PDF support via `NewReaderEncrypted`
  - [x] **Plain Text** single-section fallback
  - [x] Table-driven smoke tests for all five (DOCX test assembles `.docx` in-memory — no binary fixtures)

- [x] **Ingest pipeline**
  - [x] `Pipeline` orchestrates parse → persist tree → summarize (all stages idempotent)
  - [x] Registered against `queue.KindIngestDocument`
  - [x] Section content lands in object storage (`storage.Storage`); DB holds only outline + summaries
  - [x] Summarizer calls the `llm.Client` with a terse one-sentence prompt
  - [x] Graceful degradation when LLM is stubbed — falls back to truncated excerpt so ingest completes end-to-end in dev
  - [ ] (opt) Parallel summarization via errgroup + semaphore (today it's sequential)
  - [ ] (opt) Retry budget per section; surface summary errors on the document row

- [x] **HTTP API (ingest side)**
  - [x] `POST /v1/documents` — multipart *or* JSON body, stores bytes, enqueues job, returns 202
  - [x] `GET /v1/documents` — keyset pagination (`?limit`, `?cursor`, `?status`)
  - [x] `GET /v1/documents/{id}` — metadata + status
  - [x] `GET /v1/documents/{id}/tree` — compact `View`
  - [x] `GET /v1/sections/{id}` — metadata + full content
  - [x] `DELETE /v1/documents/{id}` — cascades to sections
  - [ ] (opt) `GET /v1/documents/{id}/source` — stream the original bytes back
  - [ ] (opt) Presigned URL passthrough when `storage.SignedURL` is supported

- [x] **TLS**
  - [x] Plaintext by default (behind reverse proxy — recommended production setup)
  - [x] Opt-in direct TLS via `server.tls.{cert_file,key_file,min_version}` + `VLE_TLS_*` env overrides
  - [ ] (opt) Autocert / Let's Encrypt integration for single-node deployments

- [x] **Dev ergonomics**
  - [x] `docker-compose` with Postgres, Redis, MinIO
  - [x] `engine` service gated behind `profiles: ["engine"]` for one-command containerised stack
  - [x] `.gitignore` tightened so `cmd/engine/main.go` stops being ignored

---

## Phase 2 — retrieval *(in progress)*

One-line: turn `POST /v1/query` from a 501 into the feature the engine exists for.

- [~] **Live LLM clients** (swap out the stubs)
  - [x] Anthropic (messages API via direct HTTP — no SDK dep, prompt caching via `cache_control` when `EnablePromptCache` is on, exp-backoff retries on 429/5xx, `/v1/messages/count_tokens` for real counts with a `len/4` fallback)
  - [ ] OpenAI (responses or chat completions, structured outputs for section-ID selection)
  - [ ] Gemini (generateContent, long-context mode for whole-tree single-pass)
  - [x] Real token counting for Anthropic (count_tokens endpoint)
  - [x] Retry with exponential backoff + jitter on 429 / 5xx (Anthropic)
  - [ ] Streaming responses (SSE) — deferred to Phase 4

- [x] **Retrieval strategies**
  - [x] `SinglePass` — real implementation
    - [x] Build prompt from `tree.View` (titles + summaries + IDs, depth-aware indentation)
    - [x] Request structured output (JSON list of section IDs + reasoning) — JSON-mode via prompt nudge + schema
    - [x] Validate returned IDs against the tree; drop unknown ones (`FilterKnownIDs`)
    - [x] Tolerate code fences / leading prose in model output (`ParseSelection`)
  - [x] `ChunkedTree` — real implementation of the parallel map-reduce design
    - [x] `Splitter` that slices the tree view into budget-sized chunks with breadcrumb + sibling summaries (structure-aware bin-packing, recurses into oversized subtrees)
    - [x] `errgroup` + semaphore bounded by `MaxParallelCalls` (already in scaffold)
    - [x] `Merge` policies: `Union` default (dedupe + sorted)
    - [ ] (opt) `TopN(ranked)`, `Vote(k-of-n)` merges
    - [x] Fall back to single slice when the tree fits the budget
    - [x] Filter IDs per-slice so the model can't fabricate IDs from other slices
  - [x] Unit tests with a mock `llm.Client` that returns canned IDs
    - [x] Happy-path selection, unknown-ID filtering, code-fence tolerance, multi-slice split, ID-fabrication guard, splitter fast path

- [x] **`POST /v1/query` handler**
  - [x] Parse body `{ document_id, query, model?, max_tokens?, reserved_for_prompt?, max_parallel_calls?, max_sections? }`
  - [x] Load tree via `db.LoadTree`
  - [x] Run the configured `retrieval.Strategy`
  - [x] Fetch picked sections' content from storage
  - [x] Return `{ sections: [...], strategy, model, elapsed_ms }`
  - [ ] (opt) Include `tokens_in` / `tokens_out` in response (Response struct already tracks them — just needs plumbing)
  - [ ] (opt) SSE streaming variant for progressively revealing sections as they're picked

- [ ] **Benchmarks vs. traditional RAG**
  - [ ] Pick a corpus (e.g. 50 technical docs + hand-written QA pairs)
  - [ ] Baseline: pgvector + OpenAI embeddings + top-K=5
  - [ ] Metrics: precision@k, recall, citation correctness, $ per query, p50/p95 latency
  - [ ] Publish in `benchmarks/README.md`

---

## Phase 3 — ecosystem *(soon)*

One-line: the engine becomes useful beyond a single `go run` on a laptop.

- [ ] **Queue drivers — flesh out the stubs**
  - [ ] River live (Postgres-backed, uses the same DB as the data plane)
  - [ ] Asynq live (Redis-backed, higher throughput path)
  - [ ] QStash webhook signature verification in `handleQueueWebhook`
  - [ ] Dead-letter surface (document row records last error + retry count)

- [ ] **Storage drivers**
  - [ ] S3-compatible live (AWS S3, Cloudflare R2, MinIO, Backblaze B2, DigitalOcean Spaces)
  - [ ] `SignedURL` for providers that support it
  - [ ] (opt) GCS driver
  - [ ] (opt) Azure Blob driver

- [ ] **SDKs** (separate repos)
  - [ ] `@vectorless/sdk-ts` — TypeScript, targets node + edge runtimes
  - [ ] `vectorless` Python package — targets 3.10+
  - [ ] `github.com/hallelx2/vectorless-go` — Go client
  - [ ] OpenAPI 3 spec generated from route handlers, SDKs generated from it

- [ ] **Packaging / deploy**
  - [ ] GitHub Actions: build + test + lint matrix
  - [ ] GHCR image publish on tag (`:latest`, `:vX.Y.Z`, `:sha-<short>`)
  - [ ] Release binaries via `goreleaser` (linux/darwin/windows × amd64/arm64)
  - [ ] Helm chart (`charts/vectorless-engine`)
  - [ ] Terraform module (`terraform/`) for one-click cloud deploys
  - [ ] systemd unit file for bare-metal installs

---

## Phase 4 — scale *(later)*

One-line: push the engine past the "one doc, one query" comfort zone.

- [ ] **Multi-document queries** — reason across N trees in one call, merge across docs
- [ ] **Streaming answers** — SSE on `/v1/query`, tokens as they come
- [ ] **Tree caching** — cache the `View` prompt per document+model so repeated queries skip rebuilding
- [ ] **Tree compaction** — merge adjacent leaf sections with tiny token counts for more efficient reasoning
- [ ] **Incremental re-ingest** — detect changed sections in a re-uploaded doc, keep stable section IDs for unchanged ones
- [ ] **Access control** — per-document ACLs + API key scoping (the control-plane's job, but engine needs hooks)

---

## Cross-cutting — always on

### Observability

- [ ] OpenTelemetry tracing (HTTP + queue jobs + LLM calls)
- [ ] Prometheus metrics endpoint (`/metrics`): request counters, queue depth, ingest latency, LLM token usage, error rates
- [ ] Structured error wrapping everywhere (sentinel errors + `errors.Is`)

### Security

- [ ] API key auth middleware (pluggable; the control-plane supplies keys)
- [ ] Rate limiting per key
- [ ] Request size limits (already 32MB on multipart; review)
- [ ] SBOM generation + supply-chain signing (cosign on images)

### Developer docs

- [ ] `docs/API.md` — full OpenAPI-driven reference
- [ ] `docs/CONTRIBUTING.md` — conventions, commit style, local dev loop
- [ ] `docs/ADR/` — architecture decision records as we go
- [ ] `docs/BENCHMARKS.md` — live numbers, updated per release

### Testing

- [ ] Unit test coverage ≥ 70% on `internal/retrieval`, `internal/ingest`, `internal/db`, `internal/parser`
- [ ] Integration test suite that spins `docker-compose` and runs end-to-end ingest → query
- [ ] Fuzz tests on parsers (malformed markdown, malformed HTML, truncated PDFs)
- [ ] Load test harness with `k6` or `vegeta` scripts

---

## Known issues / deferred

- [ ] Windows CRLF handling in git — benign warnings on every `git add`
- [ ] PDF parser doesn't handle scanned (image-only) PDFs — needs OCR
- [ ] DOCX parser loses inline formatting (bold/italic/links) — plain text only for now
- [ ] Summarizer is sequential; large trees (> 100 sections) serialize too long
- [ ] `handleQueueWebhook` is a no-op stub; needed when `queue.driver=qstash`

---

## How to use this doc

- **Before starting a task**: flip its box to `[~]` in a tiny commit so collaborators see it's claimed.
- **On merge**: flip to `[x]` in the same PR that delivers the work.
- **New ideas**: drop them under the right phase with `[?]` — it means "plausible, not committed yet."
- **Removals**: if a task turns out not to make sense, delete it rather than leaving a zombie checkbox.
