# SDKs roadmap

> Design doc: [../SDKS.md](../SDKS.md)

Client libraries for TypeScript, Python, and Go. All generated from
the same proto that `vectorless-server` serves, with a thin
hand-written ergonomics layer on top.

## Phase 0 — proto-driven generation

One-line: `buf generate` produces compiling clients in three
languages.

- [ ] `proto/` lives in `vectorless-server` repo (single source of
      truth)
- [ ] `buf.yaml`, `buf.gen.yaml`, `buf.lock` committed
- [ ] Generation targets:
  - [ ] TS: `@bufbuild/protoc-gen-es` + `@connectrpc/protoc-gen-connect-es`
  - [ ] Python: `grpcio-tools` + `protoc-gen-python-betterproto`
  - [ ] Go: `protoc-gen-go` + `protoc-gen-connect-go`
- [ ] Output lands in repos:
  - [ ] `vectorless-ts` (public)
  - [ ] `vectorless-py` (public)
  - [ ] `vectorless-go` (public) — wraps engine HTTP client
- [ ] Regeneration is a single command (`make generate`) in each
      SDK repo

---

## Phase 1 — ergonomic surface (TS first)

One-line: the hand-written layer that's actually nice to use.

- [ ] **TypeScript (`@vectorless/sdk`)**
  - [ ] `new VectorlessClient({ apiKey, baseURL })`
  - [ ] `client.documents.upload(file, { metadata })` — returns a
        Document with a polling helper
  - [ ] `client.documents.list({ cursor, limit })`
  - [ ] `client.documents.get(id)` with `.tree()`, `.section(id)`
  - [ ] `client.query({ documentId, query, limit })`
  - [ ] Thrown errors are typed: `AuthError`, `QuotaError`,
        `NotFoundError`, `ServerError`
  - [ ] Browser + Node dual entry points (ESM only, no CJS)
  - [ ] Fetch-based; no axios dependency
  - [ ] Streaming query via async iterator (Phase 3)

- [ ] **Python (`vectorless`)**
  - [ ] Sync client + async client (shared core)
  - [ ] Same surface as TS
  - [ ] Pydantic v2 models for request/response
  - [ ] Typed exceptions matching TS
  - [ ] Published to PyPI

- [ ] **Go (`go.vectorless.dev/sdk`)**
  - [ ] Thin wrapper over Connect-generated client
  - [ ] Sensible defaults (timeouts, retries)
  - [ ] No extra struct wrapping — expose proto messages directly

---

## Phase 2 — developer experience

One-line: the stuff that makes SDKs feel maintained.

- [ ] **Docs site**
  - [ ] API reference generated from proto comments (via
        `protoc-gen-doc` or custom)
  - [ ] Hand-written quickstarts per language
  - [ ] Runnable code examples in the docs

- [ ] **Examples repo**
  - [ ] `vectorless-examples/` (public)
  - [ ] Minimal app per SDK: upload + query
  - [ ] Framework-specific: Next.js route handler, FastAPI endpoint,
        Gin handler
  - [ ] Retrieval-in-an-agent example (LangChain, LlamaIndex,
        Haystack integration)

- [ ] **Contract tests**
  - [ ] Shared test harness that every SDK runs against a live
        server
  - [ ] Ensures proto changes surface as SDK failures before
        release

- [ ] **Release automation**
  - [ ] Tag `vX.Y.Z` in each SDK repo -> publishes to npm / PyPI /
        Go module proxy
  - [ ] Changeset-based changelogs (TS via Changesets, Python via
        towncrier)

---

## Phase 3 — streaming + advanced

One-line: features that require per-language shaping.

- [ ] **Streaming query**
  - [ ] TS: async iterator + AbortSignal cancellation
  - [ ] Python: async generator
  - [ ] Go: native channel from Connect

- [ ] **Retries + rate-limit handling**
  - [ ] Respect `Retry-After` on 429
  - [ ] Exponential backoff with jitter
  - [ ] Configurable `maxRetries`, `timeout`

- [ ] **Telemetry opt-in**
  - [ ] OpenTelemetry hooks (TS, Python, Go all have otel SDKs)
  - [ ] Off by default; one-line enable

---

## Phase 4 — community languages

One-line: only when someone asks for them loudly.

- [ ] [?] Rust SDK (prost + tonic)
- [ ] [?] Ruby SDK (gRPC)
- [ ] [?] Java / Kotlin SDK
- [ ] [?] PHP SDK

Policy: a community language only gets added when a non-trivial
customer needs it. Otherwise it's dead weight to maintain.

---

## Cross-cutting

- [ ] Versioning policy: SDK major version tracks proto `/v1` /
      `/v2` boundary
- [ ] Deprecation: 6 months of warnings before removing an SDK
      method
- [ ] Security: SDKs never log API keys; redact in error messages
- [ ] Bundle size (TS): < 30kb minzipped

## Known issues / deferred

- [ ] CLI (`vectorless` npm package) as an SDK surface — maybe, but
      engine CLI already covers this for self-hosters
- [ ] Browser streaming upload needs multipart chunking we haven't
      designed yet

## Related

- [../SDKS.md](../SDKS.md) — design doc.
- [SERVER.md](./SERVER.md) — the proto lives there.
- [MCP.md](./MCP.md) — builds on top of the TS SDK.
