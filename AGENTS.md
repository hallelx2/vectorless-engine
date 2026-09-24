# AGENTS.md — Vectorless engineering & review standard

This file is the shared brief for every AI agent that touches this codebase (Jules, Copilot, Claude, Cursor). It is synced from `hallelx2/dev-standards`. Follow it exactly.

## Context
Vectorless is reasoning-based document retrieval: parse a document into a hierarchical tree and let an LLM agent navigate it — no chunking, no embeddings, no vector DB. Multi-repo: Go engine/control-plane/libraries (`vectorless-engine`, `vectorless-control-plane`, `llmgate`, `pdftable`), TypeScript/Next surfaces (`vectorless-dashboard`, `vectorless-docs`, `vectorless-sdk`).

## Workflow (non-negotiable)
- **One issue → one branch → one PR → one outcome.** Use the Linear branch name (`halleluyaholudele/hal-<n>-<title>`). Never commit to `main` directly.
- Put **`Closes HAL-<n>`** in the **PR description** so Linear links + auto-closes on merge.
- **No AI attribution** in commits, PRs, or any artifact. Author as the user alone.
- "Done" = real build + tests + lint pass (run them — `go build ./... && go test ./...`, `bun run build` / `npm run build`), not just typecheck.
- Every new finding becomes a **tracked Linear issue**, not a loose comment.

## The review bar (what every reviewer checks, in order)
1. **Right thing** — matches the issue's acceptance criteria; no scope creep.
2. **Done right** — correctness, error handling, tests that *prove* behavior, simplicity (no over-engineering, no dead code).
3. **Safe** — security: authorization, **multi-tenant isolation**, secrets/BYOK handling, injection/SSRF, crypto, dependency risk.

Specialized reviewers live in `.github/agents/` — tag the relevant one (or `@jules`) on a PR for a deep pass; path-scoped rubrics in `.github/instructions/` apply automatically.

## Go conventions
- Wrap errors with context (`fmt.Errorf("...: %w", err)`); never swallow. Honour `context.Context` cancellation/timeouts.
- Concurrency: no data races (code must pass `go test -race`); guard shared state; clean up goroutines and resources (`defer Close()`).
- No `panic` in library paths; parameterize all queries; validate external input.

## TypeScript / Next conventions
- Respect server/client component boundaries; no secrets in client bundles.
- Never `dangerouslySetInnerHTML` without sanitization; keep a11y intact; avoid needless re-renders.
- Reuse the real design tokens/components — never invent brand/logo/colors (see the design source of truth).

## Security must-haves (Vectorless-specific)
- **BYOK keys**: encrypted at rest (AES-256-GCM), never logged, never returned in responses.
- **Multi-tenant**: every query/store access scoped to the caller's org/tenant — no cross-tenant reads or writes. This is the #1 risk in `vectorless-control-plane`.
