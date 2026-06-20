<p align="center">
  <img src="docs/images/banner.png" alt="Vectorless — reasoning-based document retrieval" width="100%"/>
</p>

<p align="center">
  <strong>A retrieval engine that reasons over document <em>structure</em> — not embeddings.</strong><br/>
  Parse a document into a tree, let an LLM navigate it, return whole sections with citations.
</p>

<p align="center">
  <a href="LICENSE"><img alt="License: Apache 2.0" src="https://img.shields.io/badge/license-Apache%202.0-blue.svg"/></a>
  <a href="go.mod"><img alt="Go version" src="https://img.shields.io/badge/go-%3E%3D1.25-00ADD8?logo=go&logoColor=white"/></a>
  <a href="https://hub.docker.com/r/halleluyaholudele/vectorless"><img alt="Docker Hub" src="https://img.shields.io/badge/docker-halleluyaholudele%2Fvectorless-2496ED?logo=docker&logoColor=white"/></a>
  <a href="https://github.com/hallelx2/vectorless-engine/actions"><img alt="CI" src="https://img.shields.io/badge/ci-passing-3FB950"/></a>
  <a href="https://github.com/hallelx2/vectorless-engine/stargazers"><img alt="Stars" src="https://img.shields.io/github/stars/hallelx2/vectorless-engine?style=social"/></a>
</p>

<p align="center">
  <a href="#quick-start">Quick start</a> ·
  <a href="#how-it-works">How it works</a> ·
  <a href="#why-not-vector-rag">Why not vector RAG</a> ·
  <a href="#http-api">API</a> ·
  <a href="#sdks">SDKs</a> ·
  <a href="#benchmarks">Benchmarks</a> ·
  <a href="#configuration">Config</a>
</p>

---

## Quick start

One container — engine, a bundled Postgres, and a web UI. The only thing you bring is an LLM key.

```bash
docker run -p 8080:8080 -p 7654:7654 \
  -e VLE_LLM_ANTHROPIC_API_KEY=<your key> \
  halleluyaholudele/vectorless
# UI  → http://localhost:8080
# API → http://localhost:7654
```

<p align="center">
  <img src="docs/images/screenshot-install.png" alt="docker run halleluyaholudele/vectorless" width="88%"/>
</p>

Open **http://localhost:8080**, drop in a PDF, and ask a question. You can also set your API key from the dashboard (gear icon) instead of `-e` — it stays in your browser and is sent per request ([BYOK](#bring-your-own-key)).

<p align="center">
  <img src="docs/images/screenshot-dashboard.png" alt="Vectorless dashboard — cited answer with source page preview" width="100%"/>
</p>

> Works with **GLM / Z.ai**, **Anthropic**, **OpenAI**, and **Gemini**. The image defaults to GLM (`glm-4.6`) via the Anthropic-compatible gateway; override with `-e VLE_LLM_*`.

<details>
<summary>Run from source (Go 1.25+ · Postgres)</summary>

```bash
git clone https://github.com/hallelx2/vectorless-engine.git && cd vectorless-engine
docker compose up -d postgres
export VLE_LLM_ANTHROPIC_API_KEY=<your key>
go run ./cmd/engine --local      # zero-config local mode on :7654
```
</details>

## How it works

At **ingest** the engine parses a document into a hierarchical tree of real sections — titles, page ranges, structure — and persists it. There is **no chunking** and **nothing is embedded**. At **query** time an LLM navigates that tree the way a person flips to the right page, reads the relevant sections in full, and answers with citations.

<p align="center">
  <img src="docs/images/how-it-works.svg" alt="Ingest → navigate → read → cited answer" width="100%"/>
</p>

- **No embeddings** — nothing to recompute when you swap models, nothing to drift.
- **No vector database** — Postgres + object storage is enough.
- **No top-K tuning** — the model reads 1 section or 8, as the question needs.
- **Citations for free** — every answer carries page ranges and verbatim quotes.

## Why not vector RAG

Vector RAG works until you hit the parts where it doesn't: chunks lose structure, top-K is a guess, embeddings drift, and you maintain a second database to do approximate similarity over fragments you cut out of context.

<p align="center">
  <img src="docs/images/vs-rag.svg" alt="Traditional vector RAG vs Vectorless" width="100%"/>
</p>

## Bring your own key

The engine boots **without** a provider key and accepts credentials per request, so a self-hosted or Docker user configures the key from the dashboard — never baked into the server.

```bash
curl -X POST http://localhost:7654/v1/answer/treewalk \
  -H 'Content-Type: application/json' \
  -H 'X-LLM-Api-Key: <your key>' \
  -d '{"document_id":"doc_…","query":"What were FY2018 capital expenditures?"}'
```

Headers: `X-LLM-Api-Key` (required), `X-LLM-Provider` · `X-LLM-Base-Url` · `X-LLM-Model` (optional; inherit the server defaults).

## HTTP API

Routes are versioned under `/v1` from day one.

| Method | Path | Purpose |
|--------|------|---------|
| `GET`  | `/v1/health` · `/v1/version`     | Liveness / build |
| `POST` | `/v1/documents`                  | Ingest a document (multipart; async, 202) |
| `GET`  | `/v1/documents` · `/v1/documents/{id}` | List / get status |
| `GET`  | `/v1/documents/{id}/tree`        | Structured section tree |
| `GET`  | `/v1/documents/{id}/source`      | Stream the original bytes |
| `GET`  | `/v1/sections/{id}`              | One section, full content |
| `POST` | `/v1/answer/treewalk`            | **Ask** — cited answer in one round-trip |
| `POST` | `/v1/query`                      | Retrieve relevant sections |
| `POST` | `/v1/replay`                     | Replay any answer byte-for-byte (`trace_token`) |

## SDKs

Official clients for **TypeScript**, **Python**, and **Go** ([`vectorless-sdk`](https://github.com/hallelx2/vectorless-sdk)). Point them at the local engine:

```python
from vectorless import VectorlessClient
client = VectorlessClient(base_url="http://localhost:7654")
doc = client.wait_for_ready(client.ingest_document("10-K.pdf").document_id)
ans = client.answer_treewalk(doc.id, "What were FY2018 capital expenditures?",
                             llm_key="<your key>")   # BYOK
print(ans.answer, ans.citations)
```

## Benchmarks

We benchmark on **FinanceBench** — SEC filings whose answers live in dense financial tables, the hard case for retrieval. The harness ([`vectorless-bench`](https://github.com/hallelx2/vectorless-bench)) scores **page/section-grounded recall of the evidence** and reports **quality alongside cost and latency** — because quality is meaningless without its price. It runs `treewalk` against a BM25 lexical floor and the upstream **PageIndex** library on equal footing (same model, same hardware, cold cache), and uses rank-based statistics across tasks rather than cherry-picked wins.

```bash
# reproduce — points the harness at any running engine
VECTORLESS_BASE_URL=http://localhost:7654 \
  vlbench run --config configs/financebench_glm_fast.yaml
vlbench report runs/<stamp> --k 5
```

Every run writes a manifest stamped with model, hardware, library versions, and git commit, so numbers are reproducible and audit-able. Headline results are published with the launch.

## Architecture

A single Go binary with four pluggable boundaries:

| Boundary | Implementations |
|----------|-----------------|
| **Storage** | Local filesystem · S3-compatible (R2, MinIO, B2, Spaces) |
| **Queue**   | River (Postgres) · Asynq (Redis) · QStash (serverless) |
| **LLM**     | Anthropic · OpenAI · Gemini — and any Anthropic-compatible gateway (GLM/Z.ai) |
| **Retrieval** | `treewalk` (page-based agentic — the default) · `single-pass` · `chunked-tree` |

## Configuration

Config layers, in increasing priority: `--config <yaml>` → `VLE_*` env vars → CLI flags. See [`config.example.yaml`](config.example.yaml) for the full reference. Minimal:

```yaml
database: { url: "postgres://vectorless:vectorless@localhost:5432/vectorless?sslmode=disable" }
storage:  { driver: local, local: { root: "./data/documents" } }
queue:    { driver: river }
llm:
  driver: anthropic
  anthropic: { api_key: "${VLE_LLM_ANTHROPIC_API_KEY}", base_url: "https://api.z.ai/api/anthropic/v1", model: "glm-4.6" }
```

> **Anthropic-compatible gateways (GLM/Z.ai):** `base_url` **must include `/v1`** — the client posts to `${base_url}/messages`.

> **Windows + local storage:** Windows Defender real-time protection scans
> each freshly written file and briefly hides it from `os.Stat`/`os.Open`,
> which under heavy concurrent ingestion can surface as transient
> `object not found` errors. The local backend rides through this window with
> a short internal retry, but for large bulk loads **add a Defender exclusion
> for your storage root** (`local.root` / `VLE_STORAGE_LOCAL_ROOT`):
> `Add-MpPreference -ExclusionPath "C:\path\to\data\documents"`. Linux has no
> such scan-hold and needs no exclusion.

### Supported formats

PDF (positioned text + tables via [`pdftable`](https://github.com/hallelx2/pdftable)) · Markdown · HTML · DOCX · Text.

## Related

- [`vectorless-sdk`](https://github.com/hallelx2/vectorless-sdk) — TS / Python / Go clients
- [`vectorless-bench`](https://github.com/hallelx2/vectorless-bench) — the benchmark harness
- [`llmgate`](https://github.com/hallelx2/llmgate) — provider clients, retries, pricing

## License

[Apache 2.0](LICENSE).
