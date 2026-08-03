---
applyTo: "**/*.go"
---

Go backend review for this file. Cite `file:line` + the fix.

- Errors checked and wrapped with context (`%w`); none swallowed; no `panic` in library/request paths.
- Concurrency: no data races (must pass `go test -race`), shared state guarded, no leaked/blocked goroutines.
- `context.Context` plumbed through; cancellation/deadlines honoured on I/O.
- Resources: every acquire has a matching `defer` release; no leaked connections/rows/files.
- Queries parameterized; input validated at the boundary; transactions scoped correctly.
- Tests exercise error and edge paths, not just the happy path. Flag dead code and over-engineering.
