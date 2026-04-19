# MCP roadmap

> Design doc: [../MCP.md](../MCP.md)

The MCP (Model Context Protocol) adapter lets Claude Desktop,
Cursor, Zed, and other MCP-speaking clients use vectorless as a
retrieval tool. Thin TypeScript package over the TS SDK.

## Phase 0 — minimum viable server

One-line: Claude Desktop can query a vectorless doc via MCP.

- [ ] `vectorless-mcp` repo (public)
- [ ] Package: `@vectorless/mcp`
- [ ] Depends on `@vectorless/sdk` + `@modelcontextprotocol/sdk`
- [ ] Stdio transport (the default MCP clients speak)
- [ ] Config via env: `VECTORLESS_API_KEY`, `VECTORLESS_BASE_URL`
- [ ] Tools exposed:
  - [ ] `vectorless_list_documents`
  - [ ] `vectorless_get_document`
  - [ ] `vectorless_query` (document_id + query -> sections)
  - [ ] `vectorless_get_section`
- [ ] Tool descriptions are agent-friendly (LLM reads these to
      decide when to call)
- [ ] Error surface: tool errors returned as MCP errors, not thrown
- [ ] Published to npm as runnable binary: `npx @vectorless/mcp`

---

## Phase 1 — ingest from the agent side

One-line: an agent can add a doc to vectorless without leaving the
chat.

- [ ] `vectorless_ingest_file` tool (takes a local path, uploads)
- [ ] `vectorless_ingest_url` tool (fetches + uploads)
- [ ] Polling helper built in — tool returns when ingest is `ready`
      or surfaces `failed`
- [ ] Size guard: refuse files > configurable cap (default 50MB)
- [ ] MIME whitelist (PDF, markdown, plain text, docx)

---

## Phase 2 — prompts + resources

One-line: go beyond tools — expose vectorless as MCP resources too.

- [ ] **Resources**
  - [ ] `vectorless://documents/{id}` — full tree as a resource
  - [ ] `vectorless://documents/{id}/sections/{id}` — section
        content
  - [ ] Subscription support so agents see updates

- [ ] **Prompts**
  - [ ] `summarize-document` — takes a document_id, returns a prompt
  - [ ] `answer-with-sources` — query + document -> prompt template
        that cites sections by ID

---

## Phase 3 — transport + deployment options

One-line: beyond stdio, so hosted agents can use it.

- [ ] **HTTP/SSE transport**
  - [ ] `npx @vectorless/mcp --http --port 3333`
  - [ ] For hosted agents (not Claude Desktop, but server-side
        LLM pipelines)
  - [ ] Auth via `Authorization: Bearer` header

- [ ] **Hosted MCP endpoint**
  - [ ] `mcp.vectorless.dev` — managed MCP server tied to a user's
        API key
  - [ ] OAuth flow for agent clients (once MCP spec settles)
  - [ ] Rate limits aligned with control plane quotas

- [ ] **Desktop extensions (DXT)**
  - [ ] `.dxt` bundle for Claude Desktop 1-click install
  - [ ] Signed manifest
  - [ ] Auto-update channel

---

## Phase 4 — ergonomics + polish

- [ ] Interactive `npx @vectorless/mcp init` — walks through API
      key, writes `claude_desktop_config.json` snippet
- [ ] Logging toggle (off by default; MCP stdio is sensitive to
      stdout noise)
- [ ] Local dev mode pointing at `http://localhost:8080`
- [ ] Telemetry: opt-in usage pings (tool call counts) to help
      prioritise which tools matter

---

## Cross-cutting

- [ ] MCP spec version pinning — track the spec, update when
      breaking changes land
- [ ] Conformance: run against the MCP reference client in CI
- [ ] Examples: one short video of using vectorless via Claude
      Desktop
- [ ] Security: tool descriptions never leak full paths or
      internals

## Known issues / deferred

- [ ] MCP auth story is still evolving in the spec; we stick with
      env-var API key for stdio and Bearer for HTTP until the spec
      adds a first-class flow
- [ ] Multi-tenant hosted MCP is a Phase 3 project, not Phase 0 —
      stdio covers 95% of current use
- [ ] Python MCP SDK exists too; skipping until a user asks,
      because TS covers all major MCP clients today

## Related

- [../MCP.md](../MCP.md) — design doc.
- [SDKS.md](./SDKS.md) — the TS SDK this builds on.
- [DASHBOARD.md](./DASHBOARD.md) — where users mint the API keys
  this adapter uses.
