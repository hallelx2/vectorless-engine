# Repositories

> Which repos exist, which are public vs private, and when to split them.

## Principle

Open-source anything a developer needs to **trust** the tech, **self-host**
it, or **extend** it. Keep private only what is literally the business —
billing, the dashboard, prod infra config.

This is the Supabase / Temporal / LiveKit posture: open code, paid
hosting. It works because the code is genuinely useful on its own,
which earns the reputation that the paid product converts from.

## The full inventory

### Core product — public, Apache-2.0

| Repo | What it is |
|---|---|
| `vectorless-engine` | The Go library + worker daemon. Parsing, tree, retrieval, LLM orchestration. |
| `vectorless-server` | HTTP + gRPC transport over the engine. Optional single-key auth. |
| `vectorless-proto` | `.proto` files. Single source of truth for API contracts. |
| `vectorless-mcp` | Model Context Protocol adapter for agents. |

### Libraries — public, Apache-2.0

| Repo | What it is |
|---|---|
| `llmgate` | "LiteLLM for Go" — provider-agnostic LLM client with router, fallback, cost tracking. Useful beyond vectorless. |
| `treeparse` *(maybe, later)* | Document parser subsystem (Markdown, HTML, DOCX, PDF -> hierarchical outline). Extract only if demand appears. |

### SDKs — public, Apache-2.0

| Repo | What it is |
|---|---|
| `vectorless-sdks` | Monorepo with `packages/ts`, `packages/python`, `packages/go`. |

Split into separate repos only when a second-language maintainer
appears. Until then the monorepo keeps CI simple.

### Deploy & docs — public, Apache-2.0

| Repo | What it is |
|---|---|
| `vectorless-docs` | Documentation site (Mintlify or Docusaurus). |
| `vectorless-examples` | Runnable example apps. |
| `vectorless-helm` | Helm chart for Kubernetes. |
| `vectorless-terraform` | Terraform modules for AWS/GCP/Azure. |
| `vectorless-benchmarks` | Reproducible retrieval-quality benchmark suite. |

### SaaS only — private

| Repo | What it is |
|---|---|
| `vectorless-control-plane` | Multi-tenant backend: users, orgs, keys, billing, quotas. |
| `vectorless-dashboard` | Next.js web UI for the control plane. |
| `vectorless-cloud` | Infra-as-code for `api.vectorless.dev` — terraform state, Helm values, secrets config, runbooks. |

### Marketing — public (eventually)

| Repo | What it is |
|---|---|
| `vectorless-dev` (or `vectorless-marketing`) | The `vectorless.dev` landing page + blog. Public from v1. |

## License choice

**Apache-2.0 everywhere that is public.** Not MIT, not AGPL.

- Apache-2.0 has an explicit patent grant that MIT lacks. Enterprise
  legal teams care about this.
- Apache-2.0 is the "Temporal / LiveKit / Cloudflare" license — it
  signals "we are a company comfortable with permissive OSS."
- AGPL scares enterprise buyers away. Only use it if you're pursuing
  the "Elastic / MongoDB" hostile-to-cloud-providers strategy, which
  needs revenue before it becomes defensible.

## Public vs private — the criterion

Ask: *would a developer evaluating vectorless need to see this code to
decide whether to use it?*

- Engine source? **Yes** — they need to know it's real.
- Server source? **Yes** — they need to self-host.
- SDK source? **Yes** — they need to patch bugs.
- Billing logic? **No** — it's your moat.
- Dashboard? **No** — they'll never run it; they get SaaS or they
  self-host without a dashboard.
- Prod terraform? **No** — it leaks your attack surface.

## Naming

- All public repos under the eventual `github.com/vectorless` org.
- Hyphenated names: `vectorless-engine`, `vectorless-server`, not
  `vectorlessEngine`.
- Libraries that stand alone drop the prefix: `llmgate`, not
  `vectorless-llmgate`. They're meant to be useful outside vectorless.
- Binary names match repo names minus the `vectorless-` prefix where
  it's obvious: `engine`, `server`, `dashboard`.

## GitHub account vs organisation

### Today: stay on a personal account

`github.com/hallelx2/vectorless-engine` works fine while:

- You are the only contributor.
- There is no legal company entity yet.
- Revenue is zero and Vercel Hobby is acceptable.

GitHub transfers preserve everything (stars, forks, issues, PRs,
release tags, git history) and set up automatic URL redirects. Moving
later costs a Saturday, not a migration project.

### Do today: claim the org name

Create the empty `vectorless` organisation on GitHub right now. Free,
takes 60 seconds, prevents squatters from grabbing the namespace.

### Do today: claim a vanity Go import path

Host a static HTML file at `go.vectorless.dev` with the standard
`<meta name="go-import">` tag. Start every new Go module path as
`go.vectorless.dev/<repo>` instead of `github.com/hallelx2/<repo>`.

Benefits:

- Move repos freely later with zero import-path churn.
- Signals "this is a real project" — `k8s.io/*`, `go.uber.org/*`, and
  `sigs.k8s.io/*` all do this.
- 15 minutes of setup.

### Move to the org when any one of these hits

- You hire someone.
- You incorporate a legal entity.
- You do a public launch (HN, PH, Twitter thread).

Until then, personal is correct.

## Vercel and GitHub orgs

Vercel Hobby (free) does not permit commercial deployments. The
dashboard and marketing site will be commercial the moment you switch
on Stripe. Options:

1. **Keep Vercel-deployed repos personal**, everything else in the org.
   Works indefinitely while pre-revenue. Awkward at 5+ collaborators.
2. **Cloudflare Pages** for the Vercel-style repos. Free, commercial
   use allowed, deploys from any GitHub org. This is the recommended
   path once the org is in use.
3. **Vercel Pro ($20/user/month)** for commercial use. Buy it if the
   DX is worth it.

## Initial vs eventual split

Don't stand up 14 repos on day one. Build in this order:

**Now:** One repo, `vectorless-engine`, containing everything. Move
packages from `internal/` to `pkg/` to prove the boundaries. No
external splits yet.

**Extraction 1: `llmgate`.** Cleanest boundary, no coupling to
vectorless-specific concerns, good standalone OSS story.

**Extraction 2: `vectorless-server`.** Splits HTTP/gRPC surface from
engine internals. Also pulls `vectorless-proto` out.

**Extraction 3: SDKs.** Start as `vectorless-sdks` monorepo.

**Extraction 4: MCP, examples, docs, helm.** One by one as needed.

**Later (SaaS):** control plane, dashboard, cloud infra. Private from
day one of their existence.

Each extraction is reversible for a few weeks — just merge back into
the parent. After that, issues and stars pile up and a merge becomes a
migration.

## Related docs

- [ARCHITECTURE.md](./ARCHITECTURE.md) — what each of these repos
  contains in terms of the system layers.
- [DEPLOYMENT.md](./DEPLOYMENT.md) — where each repo's output runs.
