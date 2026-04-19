# MCP adapter

> Expose vectorless as a tool to LLM agents via the Model Context
> Protocol.

## Purpose

Let agents running in Claude Desktop, Cursor, Zed, Continue, and any
other MCP-capable client use vectorless without a custom integration.
The agent just sees "vectorless is available" and can call
`vectorless.query` like it's a built-in tool.

## Repo

`vectorless-mcp`. Public, Apache-2.0.

Tiny. Its whole job is translating MCP's tool-call protocol into
calls on the vectorless SDK.

## What is MCP

Model Context Protocol is Anthropic's open protocol for attaching
tools and data sources to LLM applications. An MCP *server* exposes
tools; an MCP *client* (Claude Desktop, Cursor, etc.) loads the
server and makes its tools available to the model.

Transport is typically stdio (for local servers) or HTTP/SSE (for
remote servers). Vectorless-mcp will support both.

## What the adapter does

- Advertise a fixed set of tools:
  - `vectorless_ingest_document` — upload a file or text for indexing.
  - `vectorless_list_documents` — list what's been ingested.
  - `vectorless_query` — ask a question against one or more documents.
  - `vectorless_get_section` — retrieve full section content by ID.
- On each tool invocation: validate args, call the corresponding SDK
  method, return the result.
- Handle auth: read `VECTORLESS_API_KEY` and optional
  `VECTORLESS_BASE_URL` from config.
- Work over both stdio (Claude Desktop default) and HTTP/SSE (remote
  deploys).

## What the adapter does not do

- **Retrieval logic.** It's a thin shim over the SDK.
- **Agent orchestration.** That's the MCP client's job.
- **Persistence.** No local cache, no local state beyond the current
  request.

## Installation model

### Claude Desktop (local stdio)

User edits `~/.config/Claude/claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "vectorless": {
      "command": "npx",
      "args": ["-y", "@vectorless/mcp"],
      "env": {
        "VECTORLESS_API_KEY": "vls_live_...",
        "VECTORLESS_BASE_URL": "https://api.vectorless.dev"
      }
    }
  }
}
```

Claude Desktop spawns the process on launch. The server talks MCP
over stdio.

### Cursor / Zed / Continue

Similar config — each MCP-capable editor has its own config file, all
specifying command + args + env.

### Remote (HTTP/SSE)

For shared team use, `vectorless-mcp` can run as a long-lived service
speaking MCP over HTTP/SSE. Agents connect to a URL. Useful for:

- Team workspaces where everyone's agent should see the same corpus.
- Hosted MCP directories (e.g. the MCP registry).

## Language choice

**TypeScript / Node.** The MCP SDK is best-in-class in TypeScript,
npm publishing is simple, `npx` install gives zero-friction onboarding
for users.

Go + Python MCP SDKs exist but are newer and have smaller audiences.
If we ever need a bundled-into-a-single-binary adapter, we revisit.

Dependencies:

- `@modelcontextprotocol/sdk` — the official MCP SDK.
- `@vectorless/sdk` — our own TS SDK for the actual API calls.

## Tool definitions

Each tool is declared with a JSON Schema for inputs and a short
description. The model uses these to decide when to call the tool.

```typescript
server.tool(
  "vectorless_query",
  {
    description:
      "Search an ingested document for sections relevant to a query. Use this when the user asks about documents they've uploaded.",
    inputSchema: {
      type: "object",
      required: ["document_id", "query"],
      properties: {
        document_id: {
          type: "string",
          description: "ID of the document to search.",
        },
        query: {
          type: "string",
          description: "Natural-language question or topic.",
        },
      },
    },
  },
  async ({ document_id, query }) => {
    const result = await client.query({ documentId: document_id, query });
    return {
      content: result.sections.map((s) => ({
        type: "text",
        text: `## ${s.title}\n\n${s.content}`,
      })),
    };
  },
);
```

Tool descriptions are the *prompt* that teaches the agent when to use
the tool. Write them carefully; iterate based on observed behaviour.

## Configuration

All via env vars, since MCP clients pass them in cleanly:

```
VECTORLESS_API_KEY        (required)
VECTORLESS_BASE_URL       (default: https://api.vectorless.dev)
VECTORLESS_DEFAULT_DOCS   (optional: comma-separated doc IDs to restrict to)
VECTORLESS_LOG_LEVEL      (default: info)
```

No config files. MCP is spawned per session; env vars are the clean
way to ship config.

## Usage patterns

### One-shot: quick query from chat

```
User: "What does the handbook say about remote work?"
Agent: [calls vectorless_list_documents to find the handbook]
Agent: [calls vectorless_query with that doc ID and the question]
Agent: "According to section 3.2 of the handbook, ..."
```

### Ingest-then-query in the same conversation

```
User: [drops a PDF into Claude Desktop]
Agent: [calls vectorless_ingest_document with the PDF bytes]
Agent: "Got it, indexed. What would you like to know?"
User: "Give me a summary of chapter 4."
Agent: [calls vectorless_query]
```

Ingest is async — the adapter either polls for readiness before
returning or returns the pending document ID and lets the agent
poll. Probably polls internally with a sane timeout (30 seconds) so
the agent UX feels synchronous.

## Error handling

Errors from the SDK bubble up as MCP tool errors with the underlying
message. The adapter adds context: which tool failed, which document
ID, which request ID.

Auth errors specifically trigger a friendly message: *"Your
`VECTORLESS_API_KEY` is missing or invalid. Check your MCP config."*

## Security

- API keys live in the MCP client's config (local file on the user's
  machine). The adapter itself is stateless.
- For remote mode (HTTP/SSE), each agent connection presents its own
  key; the server validates and scopes.
- No document contents or queries are logged by the adapter — only
  metadata (tool name, request ID, status, latency).

## Packaging

- npm package: `@vectorless/mcp`.
- Runnable via `npx -y @vectorless/mcp` with no install.
- Single `dist/index.js` shipped; no native deps.
- Also publish to the MCP registry when we have one so users can
  discover it without editing JSON.

## Open questions

- **Multi-document queries from the agent.** If the user hasn't
  specified which document, should the adapter let the agent search
  across all docs in the org? Probably yes, gated by a config flag.
- **Streaming responses.** MCP supports streaming tool outputs. When
  the server gains streaming queries, the adapter should stream too —
  better UX for long queries.
- **Auto-context.** Could we implement an MCP *resource* (not a tool)
  that pre-loads a small context about the user's docs, so the agent
  knows what's available without having to call a list tool first?

## Related docs

- [SDKS.md](./SDKS.md) — the adapter consumes the TS SDK.
- [ARCHITECTURE.md](./ARCHITECTURE.md) — where MCP fits in the stack.
