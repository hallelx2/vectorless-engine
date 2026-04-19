# Architecture

> The whole vectorless stack, layer by layer. Read this first.

## One-sentence summary

Vectorless is a retrieval system that reasons over document structure
(titles + summaries, organised as a tree) instead of over vector
embeddings. The stack is five layers: an embeddable **engine**, a
transport **server**, a multi-tenant **control plane**, a human-facing
**dashboard**, and language **SDKs** plus an **MCP adapter** for agents.

## The layers

```
+------------------------------------------------------------------+
|  HUMANS & APPS                                                   |
|  +-----------+  +---------------+  +---------+  +-------------+  |
|  | Dashboard |  | User's app    |  | Claude  |  | curl /      |  |
|  | (web UI)  |  | + SDK (ts/py) |  | + agent |  | Postman     |  |
|  +-----+-----+  +-------+-------+  +----+----+  +------+------+  |
+--------|----------------|---------------|-------------|----------+
         | session        | API key       | MCP stdio   | API key
         v                v               v             |
+---------------------------------------------------+   |
|  CONTROL PLANE            (SaaS only)             |   |
|  - users, orgs, API keys                          |   |
|  - billing, quotas, metering                      |   |
|  - authenticates, rate-limits, forwards           |   |
+-----------------------+---------------------------+   |
                        | service-to-service            |
                        v                               v
+-------------------------------------------------------------+
|  VECTORLESS SERVER         (HTTP + gRPC via Connect-RPC)    |
|  - thin transport over the engine                           |
|  - optional single-key auth for self-host                   |
|  - no tenant concept, no billing                            |
+------------------------------+------------------------------+
                               | Go import (in-process)
                               v
+-------------------------------------------------------------+
|  VECTORLESS ENGINE          (Go library + worker daemon)    |
|  - parse, ingest, tree build, retrieval, LLM orchestration  |
|  - NO AUTH. trusts its caller.                              |
|  - Postgres + S3 + queue underneath                         |
+------------------------+---------+--------------------------+
                         |         |
                         v         v
                    +--------+ +--------+ +--------+
                    | llmgate| | Postgres| | S3     |
                    +--------+ +--------+ +--------+
```

## The layers, defined

### 1. Engine

A Go library plus a long-running daemon. Zero auth, zero tenant awareness.
Does one thing: turn documents into hierarchical trees and answer queries
by reasoning over those trees with an LLM. Runs as `vectorless-engine`
with subcommands (`server`, `worker`, `ingest`, `query`, `migrate`).

Importable as a Go module, so it can be embedded directly into another
Go application without an HTTP round-trip.

See [ENGINE.md](./ENGINE.md).

### 2. Server

A thin transport layer on top of the engine. Exposes the engine over
HTTP/JSON and gRPC from the same handler (via Connect-RPC). Adds an
optional single-API-key authentication middleware so self-hosters can
put it on the public internet safely.

Imports the engine as a Go module. Ships as its own binary
(`vectorless-server`) in its own repo.

See [SERVER.md](./SERVER.md).

### 3. Control plane

The SaaS-only layer. Owns multi-tenant concerns: users, organisations,
API keys, billing, quotas, usage metering. Sits in front of the
vectorless-server on an internal network and forwards authenticated
requests through.

Never open-sourced. The engine and server are community assets; the
control plane is the business.

See [CONTROL-PLANE.md](./CONTROL-PLANE.md).

### 4. Dashboard

A Next.js web app that is the human face of the control plane.
Customers log in, create organisations, issue API keys, upload test
documents, view usage, manage billing. Talks to the control plane —
never directly to the server or engine.

See [DASHBOARD.md](./DASHBOARD.md).

### 5. SDKs + MCP

Thin clients generated from a single `.proto` schema. One per language:
TypeScript, Python, Go. Same API surface, same types, same methods.
Users point the SDK at either the SaaS URL or their self-hosted server
URL — the SDK cannot tell the difference.

An MCP adapter exposes vectorless as a tool to LLM agents (Claude
Desktop, Cursor, etc.). It is an MCP server that internally uses the
SDK, so it inherits whatever the user configured.

See [SDKS.md](./SDKS.md) and [MCP.md](./MCP.md).

## Where authentication lives

| Layer | Auth model | Why |
|---|---|---|
| Engine | None | A deployment-trusted library. Like Postgres: it doesn't know who your users are. |
| Server | Optional single static API key | Just enough to put a self-hosted server on the public internet without getting pwned. One key, in config. |
| Control plane | Real multi-tenant auth — JWTs for humans, scoped API keys for apps | This is where users, orgs, billing, and rate limits live. |
| Dashboard | Session cookie / JWT via OAuth or email+password | It's a web app talking to the control plane. |
| SDKs | Pass through whichever the user configured | SDK doesn't care: it just attaches the `Authorization` header it was given. |
| MCP | Same as the SDK it wraps | MCP server reads an API key from its config. |

The **key invariant**: the engine itself is always auth-less. Everything
above it adds one more layer of authentication that makes sense for its
audience.

## How a request flows — the two scenarios

### Self-hosted

```
user's app
  -> SDK
  -> https://vls.my-company.com   (= vectorless-server)
     - validates the single API key against the configured value
     - forwards to the engine in-process
  -> engine
     - loads the document tree from Postgres
     - runs retrieval strategy
     - returns selected sections
```

Two network hops. The engine and server run in the same binary — they're
just different Go packages. Simple.

### SaaS

```
user's app
  -> SDK
  -> https://api.vectorless.dev   (= control plane)
     - validates the API key against the control plane DB
     - resolves org, plan, quota
     - rejects if over quota (429)
     - records one unit of usage for billing
     - forwards to the internal vectorless-server
  -> vectorless-server (private network)
     - skips auth (control plane already authenticated)
     - calls the engine
  -> engine
     - same as above
  <- response flows back up the chain
  -> control plane records outcome, returns to SDK
```

One extra hop (control plane -> server), but on an internal network it's
effectively free (sub-millisecond). The user sees one API.

## The design decisions that shape everything

These are the choices that cascade through the rest of the docs. Change
any of them and a lot of the rest stops making sense.

1. **The engine is a library first, a daemon second.**
   Anyone who wants to embed vectorless in a Go app does so with
   `go get`. The HTTP server is a transport, not the primary interface.

2. **No vectors, no embeddings, no index.**
   The tree itself is the retrieval index. This is the core product
   thesis. See [DATA.md](./DATA.md).

3. **Postgres for state, S3 for bytes, queue for work.**
   Three primitives, all boring, all replaceable behind interfaces.
   Not NoSQL. See [DATA.md](./DATA.md).

4. **gRPC + HTTP/JSON from one handler, via Connect-RPC.**
   One `.proto` file is the source of truth for the API. SDKs fall out
   of it. See [SDKS.md](./SDKS.md).

5. **LLM access goes through a gateway package (`llmgate`).**
   Providers live behind one interface. Router + fallback + cost
   tracking + capability flags. Depends on `langchaingo/llms` for the
   adapter code. See [LLMGATE.md](./LLMGATE.md).

6. **Engine-as-core, Control-plane-as-business.**
   The engine and server are open-source under Apache-2.0. The control
   plane is closed. This is the commercial moat. See [REPOS.md](./REPOS.md).

7. **Same SDK for SaaS and self-host.**
   The only difference is the base URL. This keeps the developer
   experience identical regardless of how they deploy.

## What vectorless explicitly is not

- **Not a search engine.** No inverted index, no BM25, no ranking.
  Retrieval is a structured reasoning call, not a similarity lookup.
- **Not an LLM itself.** It orchestrates LLM calls; it does not run
  model inference. `llmgate` talks to Anthropic, OpenAI, Gemini, Ollama.
- **Not a document store.** It stores documents as a side effect of
  indexing them for retrieval. It is not a replacement for S3, MinIO,
  or a content-management system.
- **Not a chat framework.** It returns relevant sections. Turning those
  into a conversation is the calling application's job.

## Open architectural questions

These are not yet decided. Flag if you have a strong opinion.

- **Streaming responses.** The query endpoint currently waits for the
  full strategy run before returning. SSE would let sections stream
  as the model picks them. Worth it? (Probably yes, but after v1.)
- **Multi-document queries.** Today a query targets one document.
  Multi-doc reasoning across a corpus needs a different prompt shape
  and merge policy. Deferred to Phase 4.
- **Access control inside a document.** If a section should be
  visible to user A but not user B, the engine has no model for that.
  Today it's the control plane's job. Might migrate into the engine
  later as metadata-driven visibility rules.
- **Embedding hybrid mode.** Some queries genuinely benefit from
  embedding-based coarse retrieval before tree reasoning. Worth
  prototyping once the pure-tree numbers are solid enough to compare
  against.

## Related docs

- [REPOS.md](./REPOS.md) — which of these layers is in which repo, and
  what's public vs private.
- [DEPLOYMENT.md](./DEPLOYMENT.md) — where each layer runs in
  production.
- [DATA.md](./DATA.md) — why Postgres, what the schema looks like.
- The root `ROADMAP.md` — phase-by-phase delivery of these layers.
