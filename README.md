<p align="center">
  <img src="docs/images/hero.svg" alt="vectorless-engine — reason over document structure, not embeddings" width="100%"/>
</p>

<h1 align="center">vectorless-engine</h1>

<p align="center">
  <strong>A retrieval engine that reasons over document structure — not embeddings.</strong><br/>
  No chunking. No top-K. No vector database. Just a tree, an LLM, and full sections.
</p>

<p align="center">
  <a href="LICENSE"><img alt="License: Apache 2.0" src="https://img.shields.io/badge/license-Apache%202.0-blue.svg"/></a>
  <a href="go.mod"><img alt="Go version" src="https://img.shields.io/badge/go-%3E%3D1.25-00ADD8?logo=go&logoColor=white"/></a>
  <a href="https://goreportcard.com/report/github.com/hallelx2/vectorless-engine"><img alt="Go Report Card" src="https://goreportcard.com/badge/github.com/hallelx2/vectorless-engine"/></a>
  <a href="https://github.com/hallelx2/vectorless-engine/actions"><img alt="CI" src="https://img.shields.io/badge/ci-pending-lightgrey"/></a>
  <a href="https://github.com/hallelx2/vectorless-engine/pkgs/container/vectorless-engine"><img alt="Docker" src="https://img.shields.io/badge/docker-ghcr-2496ED?logo=docker&logoColor=white"/></a>
  <a href="https://github.com/hallelx2/vectorless-engine/stargazers"><img alt="Stars" src="https://img.shields.io/github/stars/hallelx2/vectorless-engine?style=social"/></a>
</p>

<p align="center">
  <a href="#why-vectorless">Why</a> ·
  <a href="#how-it-works">How it works</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="#architecture">Architecture</a> ·
  <a href="#configuration">Configuration</a> ·
  <a href="#roadmap">Roadmap</a>
</p>

---

## Why vectorless

Vector RAG works — until you hit the parts where it doesn't. Chunks lose structure. Top-K is a guess. Embeddings drift. You maintain a second database just to do approximate similarity on bits of text you cut out of context.

**vectorless-engine takes a different path**: at ingest, it builds a structured tree of the document (titles, summaries, metadata) — essentially an LLM-friendly table of contents. At query time, an LLM reads that tree and picks the exact section IDs it needs. The engine returns those sections in full, with their narrative intact.

<p align="center">
  <img src="docs/images/retrieval-comparison.svg" alt="Traditional RAG vs. vectorless" width="100%"/>
</p>

**What you get:**

- **No embeddings** — nothing to recompute when you swap models, nothing to drift.
- **No vector database** — Postgres + object storage is enough.
- **No top-K tuning** — the model picks 1 section or 8, as needed.
- **Full context preserved** — sections are returned whole, not as fragments.
- **Citations for free** — every returned section has a stable ID.
- **Provider-agnostic** — Anthropic, OpenAI, Gemini all plug in behind the same interface.

## How it works

### 1. Ingest — build a structured tree

Upload a document; the engine parses it, splits it along semantic section boundaries (not blind chunks), summarizes each section with a cheap model, and persists the tree. All asynchronous, driven by the queue of your choice.

<p align="center">
  <img src="docs/images/ingest-pipeline.svg" alt="Ingest pipeline" width="100%"/>
</p>

### 2. Query — the LLM reasons over the tree

For small documents the whole tree fits in one prompt. For large documents the engine **splits the tree into budget-sized slices, fires parallel LLM calls, and merges the results** — so you're never bottlenecked on a single model's context window.

<p align="center">
  <img src="docs/images/chunked-reasoning.svg" alt="Chunked-tree parallel reasoning" width="100%"/>
</p>

### 3. Return — full sections, not fragments

The engine fetches the selected sections from object storage and returns them intact. Your downstream model (or agent) gets coherent, cite-able text — not a bag of chunks.

## Architecture

<p align="center">
  <img src="docs/images/architecture.svg" alt="vectorless-engine architecture" width="100%"/>
</p>

The engine is a **single Go binary** with four pluggable boundaries:

| Boundary         | Implementations shipped                                                         |
|------------------|---------------------------------------------------------------------------------|
| **Storage**      | Local filesystem · S3-compatible (AWS S3, Cloudflare R2, MinIO, Backblaze B2, DigitalOcean Spaces) — GCS / Azure planned |
| **Queue**        | [QStash](https://upstash.com/docs/qstash) (serverless) · [River](https://riverqueue.com) (Postgres) · [Asynq](https://github.com/hibiken/asynq) (Redis) |
| **LLM**          | Anthropic · OpenAI · Gemini — all behind one `llm.Client` interface             |
| **Retrieval**    | `single-pass` (small trees, one call) · `chunked-tree` (big trees, parallel map-reduce) |

Everything else — the control plane, dashboard, MCP server, SDKs — lives outside this repo and talks to the engine over its HTTP API. Run the engine standalone; run it behind your own control plane; embed it in your product. Up to you.

## Quick start

### Prerequisites

- Go 1.25+
- Postgres 15+ (for the job queue + document metadata)
- An API key from Anthropic, OpenAI, or Google

### Run locally

```bash
git clone https://github.com/hallelx2/vectorless-engine.git
cd vectorless-engine

cp config.example.yaml config.yaml
# edit config.yaml — set your LLM API key and database URL

docker compose up -d postgres
go run ./cmd/engine --config config.yaml
```

Or run the whole stack containerised:

```bash
export ANTHROPIC_API_KEY=sk-ant-...
docker compose --profile engine up --build
# engine → http://localhost:8080
```

The engine listens on `:8080` by default:

```bash
curl http://localhost:8080/v1/health
# {"status":"ok"}
```

### Ingest and query

```bash
# upload a document
curl -X POST http://localhost:8080/v1/documents \
  -F "file=@whitepaper.pdf"
# → {"document_id":"doc_01H...","status":"pending"}

# poll until ready (status: ready)
curl http://localhost:8080/v1/documents/doc_01H...

# query it
curl -X POST http://localhost:8080/v1/query \
  -H "Content-Type: application/json" \
  -d '{
    "document_id": "doc_01H...",
    "query": "what are the API stability guarantees?"
  }'
# → {"sections": [{"id":"sec_...","title":"...","content":"..."}]}
```

## HTTP API (v1)

| Method | Path                          | Purpose                                |
|--------|-------------------------------|----------------------------------------|
| GET    | `/v1/health`                  | Liveness probe                         |
| GET    | `/v1/version`                 | Engine version                         |
| GET    | `/v1/documents`               | List documents (paginated; `?status`, `?limit`, `?cursor`) |
| POST   | `/v1/documents`               | Ingest a document (async, returns 202) |
| GET    | `/v1/documents/{id}`          | Document metadata + status             |
| DELETE | `/v1/documents/{id}`          | Delete a document                      |
| GET    | `/v1/documents/{id}/tree`     | Full structured tree                   |
| POST   | `/v1/query`                   | Query — returns relevant sections      |
| GET    | `/v1/sections/{id}`           | Fetch a single section in full         |

Routes are versioned under `/v1` from day one. Breaking changes ship under `/v2` with a deprecation window.

## Configuration

The engine reads config from `--config <path>.yaml`, then overlays environment variables prefixed with `VLE_`. Environment variables always win.

Minimal `config.yaml`:

```yaml
server:
  addr: ":8080"

database:
  url: "postgres://vle:vle@localhost:5432/vectorless?sslmode=disable"

storage:
  driver: local          # local | s3
  local:
    root: "./data"

queue:
  driver: river          # qstash | river | asynq
  river:
    num_workers: 8

llm:
  driver: anthropic      # anthropic | openai | gemini
  anthropic:
    api_key: "${ANTHROPIC_API_KEY}"
    model: "claude-sonnet-4-5"
    reasoning_model: "claude-opus-4-5"

retrieval:
  strategy: chunked-tree # single-pass | chunked-tree

log:
  level: info            # debug | info | warn | error
  format: json           # json | console
```

See [`config.example.yaml`](config.example.yaml) for the full reference.

### TLS

The engine is **plaintext HTTP by default** — the recommended production setup is to terminate TLS at a reverse proxy (Caddy, nginx, an ALB, a Kubernetes ingress, Cloudflare) so cert rotation lives outside the binary. For single-node / homelab / direct-to-internet deployments you can opt into direct TLS:

```yaml
server:
  addr: ":8443"
  tls:
    cert_file: "/etc/vectorless/cert.pem"
    key_file:  "/etc/vectorless/key.pem"
    min_version: "1.2"       # "1.2" | "1.3"
```

Or via environment variables: `VLE_TLS_CERT_FILE`, `VLE_TLS_KEY_FILE`.

### Supported document formats

| Format | Parser | Notes |
|---|---|---|
| Markdown | `goldmark` | ATX + Setext headings become section boundaries |
| HTML | `golang.org/x/net/html` | Prefers `<main>`/`<article>`; skips nav/footer/script |
| DOCX | stdlib `archive/zip` + `encoding/xml` | `Heading 1…9` styles become section boundaries |
| PDF | `ledongthuc/pdf` | Font-size heuristic recovers headings from unstructured PDFs |
| Text | stdlib | Single-section fallback |

New parsers drop in behind a one-method `Parser` interface — see [`pkg/parser/`](pkg/parser/).

## Features

- ✅ Structured tree retrieval — no embeddings, no ANN index
- ✅ Pluggable LLM providers (Anthropic, OpenAI, Gemini)
- ✅ Pluggable queue backends (QStash, River, Asynq)
- ✅ Pluggable storage (Local, S3-compatible)
- ✅ Parallel map-reduce over big trees (context-budget-aware)
- ✅ Versioned HTTP API (`/v1`) with OpenAPI spec (coming)
- ✅ Graceful shutdown, structured logging, request IDs
- ✅ Postgres schema + embedded migrations (pgx v5)
- ✅ Document parsers: **Markdown · HTML · DOCX · PDF · Text**
- ✅ Optional direct TLS (opt-in; default is plaintext behind a reverse proxy)
- 🚧 Official SDKs — TypeScript, Python, Go (separate repos)
- 🚧 Dockerfile + Helm chart
- 🚧 Benchmarks vs. traditional RAG

## Roadmap

<p align="center">
  <img src="docs/images/roadmap.svg" alt="Roadmap" width="100%"/>
</p>

- **Phase 0 — scaffold** ✅ — interfaces, HTTP layer, local + QStash + Anthropic stubs
- **Phase 1 — ingest** ✅ — parsers (MD/HTML/DOCX/PDF/TXT), tree builder, summarizer, Postgres migrations, TLS, docker
- **Phase 2 — retrieval** 🚧 — `single-pass` and `chunked-tree` live, real LLM clients, benchmarks
- **Phase 3 — ecosystem** ⏭ — River + Asynq live, S3 live, SDKs, Helm, goreleaser
- **Phase 4 — scale** ⏭ — multi-document queries, streaming, caching, tree compaction

**→ See [`ROADMAP.md`](ROADMAP.md) for the full task list with subtasks and checkboxes.**

Track progress in [GitHub Issues](https://github.com/hallelx2/vectorless-engine/issues) and [Projects](https://github.com/hallelx2/vectorless-engine/projects).

## Project layout

```
cmd/engine/            # main binary entry point
internal/
  api/                 # chi HTTP router, v1 routes (private to the binary)
pkg/
  config/              # YAML + env config with validation
  db/                  # pgx pool, embedded migrations, CRUD helpers
  ingest/              # parse → persist → summarize pipeline
  parser/              # Parser interface + MD / HTML / DOCX / PDF / TXT drivers
  queue/               # Queue interface + QStash / River / Asynq drivers
  retrieval/           # Strategy interface + single-pass / chunked-tree
  storage/             # Storage interface + local / S3 drivers
  tree/                # core tree / section data model
docs/                  # API spec, architecture notes, images
```

LLM provider access lives in a separate module, [`llmgate`](https://github.com/hallelx2/llmgate),
which the engine imports as `github.com/hallelx2/llmgate`. That's
where Anthropic / OpenAI / Gemini clients, retry / budget / cache
middleware, and the cost table live.

## Contributing

Contributions are very welcome — especially parsers, benchmarks, and new LLM / storage drivers. Please open an issue first for anything non-trivial so we can align on the design.

- Run tests: `go test ./...`
- Build binary: `go build -o engine ./cmd/engine`
- Lint: `go vet ./...`

## Related projects

- **vectorless-dashboard** *(private)* — web UI + control plane built on top of this engine
- **vectorless-mcp** *(private)* — Model Context Protocol server for agents
- **@vectorless/sdk-\*** *(open source, coming soon)* — TS / Python / Go SDKs

## Acknowledgements

Inspired by prior work on tree-structured retrieval ([RAPTOR](https://arxiv.org/abs/2401.18059)), the [`llms.txt`](https://llmstxt.org) proposal, and the broader movement toward reasoning-native retrieval.

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
