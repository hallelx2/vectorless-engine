# vectorless — design docs

This folder is the source of truth for **how the vectorless project is
organised** and **what each subsystem is for**. It is not a user manual
(see the project README for that). It is a design / architecture reference
for contributors and for the author's future self.

Every doc here answers the same five questions:

1. What is this thing?
2. Why does it exist separately?
3. What are its boundaries — what it does and does not do?
4. What are the concrete design decisions we've committed to?
5. What's open, deferred, or explicitly rejected?

## Index

Start with **ARCHITECTURE.md** for the big picture, then drill into the
specific subsystem you're working on.

| Doc | Subject |
|---|---|
| [ARCHITECTURE.md](./ARCHITECTURE.md) | The whole stack, layer by layer. Read this first. |
| [REPOS.md](./REPOS.md) | Which repositories exist, which are public vs private, when to split. |
| [ENGINE.md](./ENGINE.md) | The core retrieval engine — library + daemon. |
| [SERVER.md](./SERVER.md) | The HTTP + gRPC service that fronts the engine. |
| [LLMGATE.md](./LLMGATE.md) | The "LiteLLM for Go" gateway layer. |
| [CONTROL-PLANE.md](./CONTROL-PLANE.md) | The SaaS backend — tenants, keys, billing. |
| [DASHBOARD.md](./DASHBOARD.md) | The web UI for the control plane. |
| [SDKS.md](./SDKS.md) | TypeScript, Python, Go client libraries. |
| [MCP.md](./MCP.md) | Model Context Protocol adapter for agents. |
| [DATA.md](./DATA.md) | Data model decisions — why Postgres, schema shape. |
| [DEPLOYMENT.md](./DEPLOYMENT.md) | Where and how each piece runs in production. |

## Roadmaps

Every design doc above has a paired **roadmap** in
[`roadmaps/`](./roadmaps/) — the phase-by-phase checkbox list that
tracks *when* each piece gets built. Design docs here are the *why*;
roadmaps are the *when*. Start at
[roadmaps/README.md](./roadmaps/README.md) for the index and
cross-subsystem ordering.

The engine's working checkbox list also lives at the repo root as
[`/ROADMAP.md`](../ROADMAP.md) for now — it's a superset of
[roadmaps/ENGINE.md](./roadmaps/ENGINE.md) and will become a pointer
when the engine splits out of this monorepo.

## Conventions

- **Living documents.** Edit freely as the design evolves. Rename or
  delete sections rather than leaving stale ones. If a decision is
  reversed, say so and link to the ADR that reversed it.
- **Plain Markdown.** No emojis unless explicitly needed. No binary
  diagrams — use ASCII boxes so the docs render the same everywhere,
  including in `less` and on GitHub.
- **Reasoning, not just outcomes.** Every "we do X" sentence should be
  paired with *why* — otherwise future-you won't know whether it's safe
  to change.
- **Link between docs.** When one subsystem depends on another, link
  the related doc rather than duplicating content.

## When to add a doc

Add a new doc here when:

- A new subsystem is introduced (new repo, new long-lived service).
- A cross-cutting concern spans multiple subsystems and doesn't fit in
  any single one (security model, release process, etc.).
- An architectural decision is reversed or re-litigated — record it as
  an ADR under `docs/adr/` with a date and the reason.

Don't add a doc for things that belong in the code itself
(function-level usage, package docstrings) or in the roadmap (specific
tasks and deadlines).
