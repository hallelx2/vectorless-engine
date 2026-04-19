# Server roadmap

> Design doc: [../SERVER.md](../SERVER.md)

The server starts life inside the engine repo as `pkg/api/` and
extracts to `vectorless-server` once Phase 1 is done.

## Phase 0 — in-repo scaffold *(current)*

One-line: HTTP routes exist inside the engine binary.

- [x] chi router with RequestID / RealIP / Recoverer middleware
- [x] `/v1/health`, `/v1/version`
- [x] `POST /v1/documents` (multipart + JSON)
- [x] `GET /v1/documents`, `GET /v1/documents/{id}`
- [x] `GET /v1/documents/{id}/tree`
- [x] `GET /v1/sections/{id}`
- [x] `DELETE /v1/documents/{id}`
- [x] `POST /v1/query` (live, calls retrieval strategy)
- [x] Direct-TLS opt-in via config
- [ ] `GET /v1/documents/{id}/source` — stream original bytes back
- [ ] (opt) Presigned-URL passthrough when storage supports it

---

## Phase 1 — proto + Connect-RPC

One-line: define the API once in proto, serve it as both HTTP/JSON
and gRPC from one handler.

- [ ] **Proto definition**
  - [ ] Pick a proto layout: `proto/vectorless/v1/*.proto`
  - [ ] `documents.proto` — Document message, Create / Get / List /
        Delete / GetTree / GetSection
  - [ ] `query.proto` — QueryRequest, QueryResponse, Section
  - [ ] `health.proto` — Health check
  - [ ] Document every field with comments (they become API docs)

- [ ] **Connect-RPC wiring**
  - [ ] Add `github.com/bufbuild/connect-go` dependency
  - [ ] Generate Go server + client stubs via `buf generate`
  - [ ] Implement `DocumentsServiceHandler`, `QueryServiceHandler`
  - [ ] Mount Connect handlers on the chi router alongside existing
        hand-written routes (dual-surface during transition)
  - [ ] Deprecation path for old hand-written routes: keep them for
        one release, log a warning header

- [ ] **Auth middleware**
  - [ ] `Authenticator` interface with `Principal` type
  - [ ] `NoAuth` default
  - [ ] `StaticAPIKey(key)` impl for self-hosters
  - [ ] Constant-time comparison for keys
  - [ ] `VLE_AUTH_MODE` + `VLE_AUTH_API_KEY` env vars
  - [ ] Excluded paths: `/v1/health`, `/v1/version`, `/metrics`

- [ ] **Observability**
  - [ ] Access log middleware (structured, one line per request)
  - [ ] Prometheus metrics endpoint `/metrics`
  - [ ] `http_requests_total` + `http_request_duration_seconds`
  - [ ] OpenTelemetry tracing — root span per request

---

## Phase 2 — extract to `vectorless-server` repo

One-line: move the HTTP/gRPC layer out so the engine is pure library.

- [ ] Create `vectorless-server` repo
- [ ] Move `pkg/api/` out, import the engine as a Go module
- [ ] Own its own `cmd/server/main.go` (no engine subcommands)
- [ ] Own its own Dockerfile + release pipeline
- [ ] Own its own `ROADMAP.md` (this file becomes a pointer)
- [ ] Keep engine's CLI subcommands (`worker`, `ingest`, `query`,
      `migrate`) — server subcommand stays too, delegating to
      vectorless-server binary via import

---

## Phase 3 — streaming + quality-of-life

One-line: everything that's annoying to not have by now.

- [ ] **Streaming queries**
  - [ ] Connect-RPC server-streaming for `QueryStream`
  - [ ] Engine strategy interface gains `SelectStream` variant
  - [ ] SSE HTTP fallback for non-Connect clients
  - [ ] Timeout + back-pressure behaviour documented

- [ ] **Per-principal rate limiting**
  - [ ] Token bucket middleware
  - [ ] In-memory for single-node
  - [ ] Redis-backed for multi-replica (Upstash)
  - [ ] Config: `rate_limit.requests_per_minute`

- [ ] **Request size + timeout governance**
  - [ ] Configurable max body size (currently 32MB for multipart)
  - [ ] Per-endpoint timeout overrides
  - [ ] 413 Request Entity Too Large responses

- [ ] **Queue webhook surface (for QStash-style push queues)**
  - [ ] `POST /internal/jobs/{kind}` — verify provider signature,
        decode body, dispatch to the registered handler
  - [ ] QStash HMAC verification
  - [ ] Replay protection via request ID

---

## Phase 4 — ecosystem

- [ ] (opt) Autocert / Let's Encrypt for single-node deploys
- [ ] (opt) HTTP/3 / QUIC support (Go 1.25+ has it)
- [ ] (opt) CORS middleware for browser-based SDK usage
- [ ] (opt) Idempotency-key support on `POST /v1/documents`
- [ ] (opt) GraphQL surface (only if a customer asks)

---

## Cross-cutting

- [ ] API versioning policy documented (when to bump /v1 -> /v2)
- [ ] Integration tests spinning up Postgres + MinIO + server in CI
- [ ] Contract tests that every SDK passes against the server
- [ ] OpenAPI export from proto for non-Connect tooling

## Related

- [../SERVER.md](../SERVER.md) — design doc.
- [SDKS.md](./SDKS.md) — consumers of the proto.
- [DEPLOYMENT.md](./DEPLOYMENT.md) — how this gets to production.
