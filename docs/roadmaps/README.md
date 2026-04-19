# Roadmaps

> Per-subsystem delivery plans. Checkbox documents, not design docs.

For each subsystem we have:

- A **design doc** in `docs/<SYSTEM>.md` — the *why* and *what*.
- A **roadmap** in `docs/roadmaps/<SYSTEM>.md` — the *when* and the
  *what's next*.

Start with the design doc. Come here to see what's done, what's in
progress, and what's next.

## Index

| Roadmap | Subsystem | Design doc |
|---|---|---|
| [ENGINE.md](./ENGINE.md) | Core engine | [../ENGINE.md](../ENGINE.md) |
| [SERVER.md](./SERVER.md) | HTTP/gRPC transport | [../SERVER.md](../SERVER.md) |
| [LLMGATE.md](./LLMGATE.md) | LLM gateway library | [../LLMGATE.md](../LLMGATE.md) |
| [CONTROL-PLANE.md](./CONTROL-PLANE.md) | SaaS backend | [../CONTROL-PLANE.md](../CONTROL-PLANE.md) |
| [DASHBOARD.md](./DASHBOARD.md) | Web UI | [../DASHBOARD.md](../DASHBOARD.md) |
| [SDKS.md](./SDKS.md) | TS / Python / Go clients | [../SDKS.md](../SDKS.md) |
| [MCP.md](./MCP.md) | MCP adapter | [../MCP.md](../MCP.md) |
| [DEPLOYMENT.md](./DEPLOYMENT.md) | Infra + CI/CD | [../DEPLOYMENT.md](../DEPLOYMENT.md) |

## How these relate to the root `ROADMAP.md`

The file at the repo root (`/ROADMAP.md`) is the **engine's** working
roadmap — the checkbox list used during active engine development.
It's a superset of [ENGINE.md](./ENGINE.md) for now because the engine
repo is the only one that exists yet.

When repos split out (`vectorless-server`, `llmgate`, etc.), each
will grow its own `ROADMAP.md` at its root, and the corresponding file
here becomes a pointer.

## Legend

All roadmaps use the same symbols so a skim tells you status instantly:

- `[x]` — done, shipped.
- `[~]` — in progress, actively being worked.
- `[ ]` — not started, committed.
- `[?]` — idea, plausible but not committed.
- `(opt)` — optional polish, nice-to-have.

## Conventions

- **Before starting** a task: flip `[ ]` -> `[~]` in a tiny commit so
  collaborators see it's claimed.
- **On merge**: flip `[~]` -> `[x]` in the same PR that delivers the
  work.
- **New ideas**: drop them in with `[?]` under the right phase. No
  defensive TODOs — if you wouldn't bet a week on it being built,
  leave it as `[?]` rather than `[ ]`.
- **Removals**: if a task turns out not to make sense, delete it
  rather than leaving a zombie checkbox. Git history keeps the
  decision record.
- **No dates.** Phases are sequenced, not scheduled. Dates are a lie.
  If external commitments demand a timeline, track those in an issue,
  not here.

## Phase numbering

Each subsystem has its own phase numbering (its own "Phase 0" scaffold
phase, its own "Phase 1", etc.). A phase in one subsystem does not
correspond to the same phase in another — e.g. the control plane's
Phase 0 happens long after the engine's Phase 2.

The **rough ordering** of cross-subsystem work, at the project level:

1. Engine phases 0–2 (scaffold, ingest, retrieval) — where we are now.
2. Extract `llmgate` into its own repo.
3. Extract `vectorless-server` into its own repo.
4. SDKs phase 0 (generate from proto).
5. MCP phase 0 (tiny adapter over the TS SDK).
6. Deployment phase 0 (one-region SaaS on Fly + Neon + R2).
7. Control plane phase 0 + Dashboard phase 0 (auth, keys, usage).
8. Billing, plans, Stripe — control plane phase 1.
9. Engine phase 3+ (ecosystem, scale).
