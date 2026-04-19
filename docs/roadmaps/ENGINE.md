# Engine roadmap

> Design doc: [../ENGINE.md](../ENGINE.md)

For the engine specifically, the authoritative checkbox list lives at
the **repo root**: [`/ROADMAP.md`](../../ROADMAP.md).

This file exists so that every subsystem has a discoverable roadmap in
the same place, and so that when the engine eventually splits out of
this monorepo, there's already a roadmap file with the right name.

## Current status (summary)

| Phase | Status |
|---|---|
| Phase 0 — scaffold | Shipped |
| Phase 1 — ingest | Shipped |
| Phase 2 — retrieval | Shipped |
| Phase 3 — ecosystem | Not started |
| Phase 4 — scale | Not started |

For the detailed checklist (sub-tasks, optional polish, known issues),
see [`/ROADMAP.md`](../../ROADMAP.md).

## Engine-specific next priorities

These are the items from the root roadmap that are the natural next
things to work on after Phase 2:

- [ ] **Refactor: `internal/` -> `pkg/`**
  - [ ] Move `internal/llm` -> `pkg/llm` (interface stays, impls stay).
  - [ ] Move `internal/retrieval` -> `pkg/retrieval`.
  - [ ] Move `internal/ingest` -> `pkg/ingest`.
  - [ ] Move `internal/tree` -> `pkg/tree` (already exported-ish).
  - [ ] Move `internal/parser` -> `pkg/parser`.
  - [ ] Move `internal/storage` -> `pkg/storage`.
  - [ ] Move `internal/db` -> `pkg/db`.
  - [ ] Keep `internal/api` for now — moves to `vectorless-server`
        repo when extracted.

- [ ] **CLI subcommands (cobra)**
  - [ ] `vectorless-engine server` — current default.
  - [ ] `vectorless-engine worker` — queue workers only.
  - [ ] `vectorless-engine migrate` — run DB migrations explicitly.
  - [ ] `vectorless-engine ingest FILE` — one-shot CLI ingest for
        testing.
  - [ ] `vectorless-engine query DOC_ID QUERY` — one-shot CLI query.
  - [ ] `vectorless-engine version` — print build info.
  - [ ] `vectorless-engine config print` — print effective config
        (secrets redacted).
  - [ ] `vectorless-engine config check` — validate config and exit.

- [ ] **Config layers — YAML + env + CLI flags**
  - [ ] Define precedence: defaults -> YAML -> `VLE_*` env -> flags
  - [ ] `pflag` bindings for every YAML key
        (dot-path flag names: `--server.addr`, `--llm.driver`, …)
  - [ ] Thin merger over `mapstructure` decode (no viper)
  - [ ] `--config=path.yaml` flag; default `./config.yaml` if present
  - [ ] `config.Validate()` reports *which layer* supplied a bad
        value, so users know where to fix it
  - [ ] Secrets policy: prefer env / mounted file; warn when
        `--*api_key=` is seen on the command line
  - [ ] Tests: same effective config reachable via YAML-only,
        env-only, and flags-only (round-trip via
        `config print`)

- [ ] **Swap handwritten Anthropic client for `langchaingo/llms`
      through the llmgate package.** Delete the direct HTTP client;
      keep the `llm.Client` interface; implement adapters.

- [ ] **`tree_snapshot` denormalisation** — write the compact
      `tree.View` to `documents.tree_snapshot` JSONB at the end of
      ingest so query-time loads are one-row reads.

- [ ] **Parallel summarisation** via errgroup + semaphore (today
      sequential; large docs serialise too long).

## Deferred to Phase 3+

See [`/ROADMAP.md`](../../ROADMAP.md). Highlights:

- Live Queue drivers (River, Asynq) — currently stubs.
- S3-compatible storage driver live (currently local-only).
- OCR for scanned PDFs.
- Incremental re-ingest with stable section IDs.
- Multi-document queries.
- Tree caching, tree compaction.

## Related

- [../ENGINE.md](../ENGINE.md) — the design doc.
- [../LLMGATE.md](../LLMGATE.md) — the package the engine will
  depend on for LLM access.
- [../../ROADMAP.md](../../ROADMAP.md) — the working checkbox
  document.
