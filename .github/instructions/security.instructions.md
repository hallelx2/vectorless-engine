---
applyTo: "**"
---

Security review for every changed file, against **OWASP Top 10 (2021)** + **CWE Top 25**. Treat an uncertain security question as a finding and say so. Cite `file:line`, the **OWASP/CWE id**, and the fix.

- **Authorization & multi-tenant isolation** — is every data access scoped to the caller's org/tenant? Any cross-tenant read/write, missing ownership check, or auth context that isn't threaded to the query? (Top risk in `vectorless-control-plane`.)
- **Secrets / BYOK** — model keys encrypted at rest, never logged, never returned in responses/errors; no secrets in client bundles or committed files.
- **Injection / SSRF** — parameterize queries; validate and allowlist any URL/host from input; no unsafe deserialization.
- **Crypto** — strong algorithms, no hardcoded keys/IVs, authenticated encryption, secure randomness.
- **Dependencies** — new packages justified, reputable, no known CVEs.
