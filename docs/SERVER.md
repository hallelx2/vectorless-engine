# Server

> The HTTP + gRPC service that fronts the vectorless engine.

## Purpose

Expose the engine as a network service so that non-Go clients (SDKs,
curl, the control plane, MCP adapters, agents) can use it. The server
is a thin transport layer — it does request decoding, auth, and
response encoding. It does not contain retrieval logic, parsing
logic, or LLM logic.

## Repo

`vectorless-server`. Separate repo from the engine. Imports the engine
as a Go module dependency.

Until Phase 2 extraction is done, the server code lives in
`pkg/api/` inside the engine repo and compiles into the engine binary.
After extraction, it becomes its own binary.

## What the server does

- Terminates HTTP/1.1, HTTP/2, and gRPC on the same port.
- Decodes incoming requests into engine-typed Go values.
- Applies middleware: request ID, recovery, logging, metrics, optional
  API-key auth.
- Calls into the engine in-process.
- Encodes engine responses back over the wire.
- Graceful shutdown: stops accepting, drains in-flight, closes engine
  workers.

## What the server does not do

- **Multi-tenancy.** No org concept. No per-tenant quota. That's the
  control plane's job.
- **Billing or usage metering.** Ditto.
- **Database migrations.** The engine owns its schema. The server does
  not touch Postgres directly.
- **Per-user rate limiting.** Only a coarse global rate limit, if any.

## Transport: Connect-RPC

One handler, three transports: **gRPC**, **gRPC-Web**, and **plain
HTTP/JSON**. All driven from the same `.proto` definitions.

Why Connect-RPC (`github.com/bufbuild/connect-go`):

- Single handler implementation, no code duplication between REST and
  RPC surfaces.
- HTTP/JSON path is standard — curl and Postman work with no tooling.
- gRPC path gets full streaming, codegen, and tooling for free.
- TypeScript SDK falls out of the same proto via `@connectrpc/connect-es`.
- Works behind any HTTP/2 proxy (Cloudflare, ALB, nginx).

The alternative — hand-written REST handlers plus a separate gRPC
server — means two implementations of every endpoint drifting apart.
Not worth it.

## API surface (v1)

All routes versioned under `/v1`. Breaking changes ship as `/v2`
alongside a deprecation window.

### Documents

- `POST /v1/documents` — ingest a document. Multipart or JSON body.
  Returns `202 Accepted` with a `document_id`.
- `GET /v1/documents` — list with keyset pagination (`limit`, `cursor`,
  `status`).
- `GET /v1/documents/{id}` — metadata + lifecycle status.
- `GET /v1/documents/{id}/tree` — the compact `View` used for
  reasoning.
- `GET /v1/documents/{id}/source` — stream the original bytes. Optional.
- `DELETE /v1/documents/{id}` — cascades to sections.

### Sections

- `GET /v1/sections/{id}` — section metadata + full content from
  storage.

### Query

- `POST /v1/query` — body: `{ document_id, query, model?, max_tokens?,
  reserved_for_prompt?, max_parallel_calls?, max_sections? }`.
  Returns `{ document_id, query, strategy, model, sections[],
  elapsed_ms }`.

### Health / meta

- `GET /v1/health` — liveness.
- `GET /v1/version` — build info.
- `GET /metrics` — Prometheus scrape.

### Internal

- `POST /internal/jobs/{kind}` — webhook surface for push-based queue
  drivers (e.g. QStash). Verifies signature.

## Authentication

The server has **one pluggable auth mode**. Default is off.

```go
type Authenticator interface {
    Authenticate(r *http.Request) (Principal, error)
}
```

Shipped implementations:

- `NoAuth` — always returns an anonymous principal. Default.
- `StaticAPIKey(key string)` — compares the `Authorization: Bearer ...`
  header to a configured value with `subtle.ConstantTimeCompare`. For
  self-hosters.

Config:

```yaml
auth:
  mode: "none" | "api_key"
  api_key: "vls_live_..."
```

The control plane supplies its own `Authenticator` implementation when
running in SaaS mode — one that validates against the control-plane
database. The server doesn't know or care what mode it's in.

**Invariant: the engine never receives auth info.** By the time a
request reaches the engine, it's already authorised. The engine
doesn't even have a `principal` parameter.

## Middleware stack

In order:

1. `RequestID` — generate or propagate `X-Request-ID`.
2. `RealIP` — honour `X-Forwarded-For` behind a trusted proxy.
3. `Recoverer` — convert panics into 500s with a logged stack trace.
4. `Logging` — structured access log (method, path, status, duration).
5. `Metrics` — Prometheus histograms + counters.
6. `Authenticator` — skipped for `/v1/health` and `/v1/version`.
7. `RateLimit` (optional, per-key) — token bucket per principal.
8. The handler itself.

Each middleware is a `func(http.Handler) http.Handler`. Order matters:
recovery wraps everything so panics are always caught, request ID is
outermost so every log line has it.

## Graceful shutdown

On SIGTERM / SIGINT:

1. Stop accepting new connections (`srv.Shutdown(ctx)`).
2. Drain in-flight HTTP requests (15s timeout).
3. Signal the engine's queue workers to stop.
4. Wait for in-flight jobs to finish or timeout.
5. Close the DB pool, the storage client, the queue driver.
6. Exit 0.

The 15s drain matches Kubernetes' default `terminationGracePeriodSeconds`
so rolling deploys don't cut requests mid-flight.

## TLS

Two modes, both supported:

- **Plaintext (default).** Terminate TLS at the reverse proxy
  (Caddy, nginx, ALB, ingress). This is what 90% of deploys want.
- **Direct TLS.** Opt in via `server.tls.{cert_file, key_file,
  min_version}` config. For single-node deploys without a proxy.

Autocert / Let's Encrypt integration for single-node deploys is a
future optional. Not in v1.

## Observability

- **Structured logging** via `slog` (JSON in prod, console in dev).
  Every log line includes `request_id`, `document_id` where relevant,
  `principal_id` after auth.
- **Prometheus metrics** at `/metrics`:
  - `http_requests_total{method, path, status}`
  - `http_request_duration_seconds{method, path}` (histogram)
  - `queue_job_duration_seconds{kind, status}` (histogram)
  - `llm_tokens_total{provider, model, direction=in|out}`
  - `llm_request_duration_seconds{provider, model}` (histogram)
  - `documents_ingested_total{status}`
- **OpenTelemetry tracing.** Each HTTP request starts a root span;
  engine calls add child spans (parse, summarise, strategy.Select, LLM
  call). OTLP export to whichever collector the operator configures.

## Deployment shape

Same binary, two roles:

```
docker run vectorless-server:1.2.3 server      # HTTP + gRPC + embedded workers
docker run vectorless-server:1.2.3 worker      # queue workers only
```

For small deploys: one `server` container. For larger deploys: one
`server` container behind a load balancer + N `worker` containers on
an autoscaler driven by queue depth.

## Open questions

- **Streaming queries over SSE or gRPC server-streaming.** Connect-RPC
  supports server-streaming natively. The handler layer is ready; the
  engine strategy interface would need a `SelectStream` variant.
- **Presigned-URL passthrough.** When storage supports signed URLs
  (S3, R2), `GET /v1/sections/{id}` could return a URL the client
  fetches directly. Saves bandwidth on large sections.
- **Per-key rate limiting.** Basic token bucket is cheap; needs a
  distributed counter (Redis) in multi-replica deploys.

## Related docs

- [ENGINE.md](./ENGINE.md) — what the server imports.
- [SDKS.md](./SDKS.md) — what generates from the server's proto.
- [CONTROL-PLANE.md](./CONTROL-PLANE.md) — the layer above that
  supplies an `Authenticator` and sits in front of the server in SaaS.
