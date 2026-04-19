# vectorless-engine

The vectorless retrieval engine. Reason over document structure instead of
chunking-and-embedding.

Vectorless replaces vector search with **structured tree reasoning**: at ingest
time the engine builds a hierarchical outline of each document (titles,
summaries, metadata); at query time an LLM reasons over the tree to decide
which full sections are needed, and the engine returns those sections in full.

No embeddings. No top-K tuning. No vector database. Just the document's
structure and an LLM that can read it.

## Status

Early scaffold. API surface is stabilizing. Expect breaking changes until v1.0.

## Quick start

```bash
# run locally (requires Postgres)
docker compose up -d
go run ./cmd/engine

# run against the engine
curl http://localhost:8080/v1/health
```

## Architecture

The engine is a single Go binary with three pluggable boundaries:

- **Storage** — where document bytes live. Ships with local filesystem and
  S3-compatible (AWS S3, Cloudflare R2, MinIO, Backblaze B2, DigitalOcean
  Spaces). GCS and Azure Blob planned.
- **Queue** — where ingest jobs live. Ships with QStash (serverless),
  [River](https://github.com/riverqueue/river) (Postgres-backed,
  recommended default), and [Asynq](https://github.com/hibiken/asynq)
  (Redis-backed, higher throughput).
- **Retrieval strategy** — how the engine reasons over the tree. Ships with
  `single-pass` (small trees, one LLM call) and `chunked-tree` (big trees,
  parallel map-reduce across subtrees). Custom strategies are just an
  interface away.

```
┌──────────────────────────────────────────┐
│        vectorless-engine (Go binary)     │
│                                          │
│   HTTP API (v1)                          │
│      │                                   │
│      ├── Ingest: parse → split → tree    │
│      │                                   │
│      ├── Query:  reason(tree) → fetch    │
│      │                                   │
│      └── Admin:  documents, trees, jobs  │
│                                          │
│   Storage | Queue | Retrieval | LLM      │
│   (pluggable)                            │
└──────────────────────────────────────────┘
```

## Endpoints (v1 draft)

| Method | Path                          | Purpose                              |
|--------|-------------------------------|--------------------------------------|
| GET    | `/v1/health`                  | Health probe                         |
| POST   | `/v1/documents`               | Ingest a document (async)            |
| GET    | `/v1/documents/:id`           | Get document metadata + tree         |
| DELETE | `/v1/documents/:id`           | Delete a document                    |
| GET    | `/v1/documents/:id/tree`      | Get the full structured tree         |
| POST   | `/v1/query`                   | Query — returns relevant sections    |
| GET    | `/v1/sections/:id`            | Fetch a section's full content       |

See [`docs/API.md`](docs/API.md) for the full specification.

## Configuration

The engine reads config from environment variables (prefix `VLE_`) or from
`config.yaml`. See [`config.example.yaml`](config.example.yaml).

## License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).
