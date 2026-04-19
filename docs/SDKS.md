# SDKs

> TypeScript, Python, Go client libraries. Generated from one proto.

## Purpose

Make vectorless trivially callable from the three languages most
likely to integrate with it. Maintain API parity across all three by
generating them from a single source of truth.

## Repo

`vectorless-sdks` monorepo:

```
vectorless-sdks/
  packages/
    ts/         - TypeScript / Node / Edge
    python/     - Python 3.10+
    go/         - Go 1.21+
  proto/        - synced from vectorless-proto (or submodule)
```

One repo until a second-language maintainer appears. Then split per
language.

## What an SDK does

- Provide typed client methods for every server endpoint:
  `ingest`, `listDocuments`, `getDocument`, `getTree`, `getSection`,
  `deleteDocument`, `query`.
- Handle auth: inject `Authorization: Bearer <key>`.
- Retry on transient errors with backoff.
- Stream long operations (query with SSE or gRPC streaming) when the
  transport supports it.
- Give the user a pleasant, idiomatic API that feels native to the
  language.

## What an SDK does not do

- **Open documents directly** — no local parsing. That's the server's
  job. The SDK takes bytes or a file handle and uploads.
- **Cache trees or queries** — add-on responsibility, not an SDK
  baseline feature.
- **Provide a full agent framework** — users bring their own.

## The contract

All three SDKs call the **same server API** over HTTP/gRPC. The only
difference between SaaS and self-host is the base URL.

```
Self-host:  new VectorlessClient({ baseURL: "https://vls.mycompany.com", apiKey })
SaaS:       new VectorlessClient({ baseURL: "https://api.vectorless.dev", apiKey })
```

SDKs are transport-agnostic at the protocol level (Connect-RPC
produces clients that work over both HTTP/JSON and gRPC), but for
simplicity the public examples always use HTTP/JSON.

## Generation

- **Source of truth:** `.proto` files in `vectorless-proto`.
- **TypeScript:** `@connectrpc/protoc-gen-connect-es` +
  `@bufbuild/protoc-gen-es`. Output is plain TypeScript, uses
  `fetch`, works in Node 18+, Deno, Bun, Cloudflare Workers, Vercel
  Edge, browsers.
- **Python:** `connecpy` (Connect-RPC Python) or `betterproto`. Output
  is `async`/`await` with optional sync wrapper. Type-hinted throughout.
- **Go:** `connect-go` generates the client. The repo can also be
  consumed directly by Go programs that want the grpc server stubs.

Generation runs as a CI job when the proto changes; output is
committed into each language package so users don't need `protoc` to
install the SDK.

## TypeScript SDK

### Shape

```typescript
import { Vectorless } from "@vectorless/sdk";

const client = new Vectorless({
  baseURL: "https://api.vectorless.dev",
  apiKey: process.env.VECTORLESS_API_KEY!,
});

// Ingest
const { documentId } = await client.documents.create({
  filename: "handbook.pdf",
  content: fs.readFileSync("handbook.pdf"),
});

// Poll until ready
let doc;
do {
  doc = await client.documents.get(documentId);
  if (doc.status === "failed") throw new Error(doc.errorMessage);
  await sleep(1000);
} while (doc.status !== "ready");

// Query
const result = await client.query({
  documentId,
  query: "What is the vacation policy?",
});

for (const section of result.sections) {
  console.log(section.title, section.content);
}
```

### Runtimes

- Node.js 18+ (native fetch).
- Browser (works, but API keys in browser is almost always a mistake —
  warn loudly in the docs).
- Cloudflare Workers, Vercel Edge, Deno, Bun — all via native fetch.

### Package

- `@vectorless/sdk` on npm.
- Dual-published ESM + CJS.
- Zero runtime dependencies beyond `@bufbuild/protobuf` and
  `@connectrpc/connect`.

## Python SDK

### Shape

```python
from vectorless import Vectorless

client = Vectorless(
    base_url="https://api.vectorless.dev",
    api_key=os.environ["VECTORLESS_API_KEY"],
)

# Ingest
with open("handbook.pdf", "rb") as f:
    doc = client.documents.create(filename="handbook.pdf", content=f.read())

# Poll
while doc.status != "ready":
    if doc.status == "failed":
        raise RuntimeError(doc.error_message)
    time.sleep(1)
    doc = client.documents.get(doc.id)

# Query
result = client.query(document_id=doc.id, query="What is the vacation policy?")
for section in result.sections:
    print(section.title, section.content)
```

### Runtimes

- Python 3.10+.
- Both sync (`Vectorless`) and async (`AsyncVectorless`) client classes.
- Dependencies: `httpx`, `pydantic`, generated protobuf types.

### Package

- `vectorless` on PyPI.
- Installable via `pip install vectorless` or `uv add vectorless`.

## Go SDK

### Shape

```go
import "go.vectorless.dev/vectorless-go"

client := vectorless.NewClient(&vectorless.Config{
    BaseURL: "https://api.vectorless.dev",
    APIKey:  os.Getenv("VECTORLESS_API_KEY"),
})

doc, err := client.Documents.Create(ctx, &vectorless.CreateDocumentRequest{
    Filename: "handbook.pdf",
    Content:  bytes,
})
// ...
```

### Use cases

- Other Go services integrating over the network (same as TS/Python).
- **Not** for embedding vectorless inside a Go app — that should
  import `go.vectorless.dev/engine/pkg/retrieval` directly, no HTTP.

### Package

- `go.vectorless.dev/vectorless-go` (vanity path).
- Standard library + `connect-go`. Nothing else.

## The "embed it" shortcut (Go only)

Only Go consumers have the option to skip HTTP entirely and use the
engine as a library. The other languages always go through the server
or SaaS:

```go
// Embed the engine directly.
import (
    "github.com/hallelx2/vectorless-engine/pkg/ingest"
    "github.com/hallelx2/vectorless-engine/pkg/retrieval"
    "github.com/hallelx2/vectorless-engine/pkg/tree"
)
```

This is intentional. FFI bindings to the Go engine from Python/TS
were considered and rejected — see [ARCHITECTURE.md](./ARCHITECTURE.md)
for the reasoning.

## Error handling

All SDKs surface errors as typed exceptions / error values:

- `VectorlessAuthError` — 401 / 403.
- `VectorlessQuotaError` — 429 from the control plane.
- `VectorlessValidationError` — 400 with field-level details.
- `VectorlessNotFoundError` — 404.
- `VectorlessServerError` — 5xx (retryable).
- `VectorlessNetworkError` — connection issues (retryable).

Each carries the underlying `request_id` for support triage.

## Retry policy

By default: 3 retries on 429, 5xx, and network errors, with
exponential backoff + jitter. Respects `Retry-After` headers.
Non-retryable errors (4xx other than 429) fail immediately.

Users can disable or customise:

```typescript
new Vectorless({ ..., maxRetries: 0 });
```

## Streaming

When the server gains streaming support:

- TypeScript: `for await (const section of client.queryStream(...))`.
- Python: `async for section in client.query_stream(...):`.
- Go: iterator channel or `Next()` method per Go streaming conventions.

Until then, queries are request-response.

## Versioning

- SDKs are versioned independently per language, following SemVer.
- Breaking API changes in the server (a new major /v2 path) trigger a
  new SDK major version.
- Non-breaking server additions (new endpoint) get a minor SDK bump.
- Language-specific fixes get patch bumps.

## Testing

- Contract tests: each SDK has a suite that runs against a local
  `vectorless-server` spun up in CI (docker-compose).
- Unit tests for the retry / auth / error-mapping layer per language.
- A shared "golden" directory with canonical request/response pairs
  so all three SDKs verify the same behaviour.

## Documentation

- Each SDK ships with a README and a `docs/` folder.
- `vectorless-docs` (the central docs site) has per-language tabs for
  every code example.
- Auto-generated API reference from proto comments.

## Open questions

- **Pagination ergonomics.** `listDocuments` returns a cursor. Do
  SDKs expose iterators (`for await`, generators) or keep it raw and
  let users paginate manually? Iterators are nicer but hide errors.
- **File uploads.** Should we support multipart streaming for large
  docs, or always read into memory? Streaming is better for huge
  PDFs but more complex per language.
- **Logging hooks.** Opinionated or minimal? Probably minimal — let
  users plug in their own logger via a `logger` option.

## Related docs

- [ARCHITECTURE.md](./ARCHITECTURE.md) — where SDKs sit.
- [SERVER.md](./SERVER.md) — the API shape the SDKs call.
- [MCP.md](./MCP.md) — an SDK consumer.
