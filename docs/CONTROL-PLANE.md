# Control plane

> The multi-tenant SaaS backend. This is the business.

## Purpose

Turn the open-source engine + server into a commercial service. Handle
everything the engine refuses to know about: users, organisations, API
keys, billing, quotas, usage metering, fraud prevention, customer
support tooling.

## Repo

`vectorless-control-plane`. **Private.** Not open-sourced. This is the
commercial moat.

If a "community edition" of multi-tenant hosting ever makes sense, it
would be a separate, smaller repo that excludes billing, Stripe, quota
enforcement, and fraud logic.

## What the control plane does

- Own the `users`, `orgs`, `memberships`, `api_keys`, `usage`,
  `invoices` tables. All the SaaS-level state.
- Authenticate every incoming request:
  - From the dashboard: session cookie / JWT, resolved to a user +
    active org.
  - From the SDK: API key, resolved to an org (API keys are
    org-scoped, not user-scoped).
- Authorise: does this principal have permission to do this action on
  this resource?
- Enforce quotas: is the org over its plan's document / query / token
  limits?
- Meter usage: record the request for billing.
- Proxy the (now-authorised) request to `vectorless-server` on an
  internal network.
- Emit webhooks to customers for async events (ingest complete, query
  complete if async).
- Handle Stripe webhooks: subscription changes, payment failures,
  plan upgrades.
- Export metrics: revenue, active orgs, failed payments, etc.

## What the control plane does not do

- **Retrieval logic.** It forwards to the server, which calls the
  engine. It has no tree, no parser, no LLM call.
- **Document storage.** Documents live in whatever S3 bucket the
  engine is configured with.
- **UI.** The dashboard is a separate repo.
- **Engine configuration.** It does not set LLM models, strategies, or
  parser options. That's server/engine config, done once at deploy
  time.

## Architecture position

```
SDK / dashboard
  |
  v
[CONTROL PLANE]          <-- authenticate, authorise, meter, rate-limit
  |
  v (internal network, not public)
[VECTORLESS SERVER]      <-- pass through, no further auth
  |
  v (Go in-process)
[VECTORLESS ENGINE]
```

The control plane is the only thing a SaaS customer's request ever
sees directly. The server and engine are never on the public internet
in SaaS mode.

## Core data model (sketch)

```sql
users            (id, email, password_hash, email_verified_at, ...)
orgs             (id, name, slug, stripe_customer_id, plan, created_at)
memberships      (user_id, org_id, role)
api_keys         (id, org_id, prefix, hash, scopes, last_used_at, revoked_at)
usage            (org_id, day, documents_ingested, queries_ran,
                  tokens_in, tokens_out, cost_cents)
invoices         (id, org_id, stripe_invoice_id, amount_cents, status)
audit_log        (id, org_id, user_id, action, resource, at)
```

All Postgres. Separate database from the engine's Postgres — control
plane and engine state should never mix, even if they're the same
physical instance.

## API shape

Two surfaces:

### 1. Customer-facing proxy (`api.vectorless.dev`)

Mirrors the vectorless-server's API exactly. Same paths, same
request/response shapes. This is the URL SDKs point at.

The proxy layer:

1. Validates `Authorization: Bearer vls_live_...`.
2. Resolves the key to an org.
3. Checks per-plan quotas.
4. Forwards to `https://internal.vectorless-server/v1/...` with an
   internal auth token.
5. Records usage when the response comes back.
6. Returns to the caller.

### 2. Management API (`api.vectorless.dev/admin/v1/...`)

Dashboard-facing CRUD:

- Signup / login / session.
- Create org, invite members, update billing info.
- Issue / revoke / rotate API keys.
- View usage, invoices, plan, downgrade / upgrade.
- Admin-only: list all orgs, impersonate, set plan overrides
  (customer support tools).

Not available via SDK. Dashboard only.

## Authentication

Two principal types, unified in a single `Principal` struct with a
type discriminator:

- **User** — authenticated via session cookie or JWT. Has a linked
  user row and an *active org* selected via `X-Vectorless-Org` header
  or session state.
- **API key** — authenticated via `Authorization: Bearer`. Scoped
  directly to an org (no user involved).

API keys are stored hashed with `argon2id`. The prefix (`vls_live_abc`)
is stored plaintext for identification; the suffix is hashed. On every
request: compute prefix + hash, look up by prefix, compare hash with
constant-time comparison.

## Authorisation

Role-based per org:

- `owner` — full control, including billing and delete.
- `admin` — full control except delete org and change billing owner.
- `member` — read documents, run queries.
- `viewer` — read-only.

API keys inherit a scope set at creation time: `documents:read`,
`documents:write`, `queries:run`. Most keys get all three; strict
integrations can scope down.

## Quota and rate limiting

Per plan:

- Documents ingested per month.
- Queries per month.
- LLM tokens per month (pass-through cost).
- Requests per second (rate limit, not a quota).

Implementation:

- Rate limit via Redis token bucket per key.
- Monthly quota checked against the `usage` table at request time;
  rejected with `429` and `Retry-After: <next-month>` if over.
- Overage billing is a plan feature, not a default: if the plan
  allows overage, the request proceeds and the `usage` row gets
  incremented, with billing running a nightly job to compute overages.

## Billing

Stripe is the source of truth for payments. The control plane is the
source of truth for *usage*.

Flow:

1. Nightly job aggregates yesterday's `usage` rows per org.
2. Translates to Stripe usage records via the metered-billing API
   for metered line items.
3. Stripe generates the monthly invoice.
4. Stripe webhook tells us when payment succeeds / fails.
5. On failure: grace period, then downgrade to the free plan and
   rate-limit to zero until paid.

Plan tiers (sketch):

- **Free** — 100 docs, 500 queries / month. Community support.
- **Pro** — $50/month. 2k docs, 20k queries. Email support.
- **Team** — $200/month. 20k docs, 200k queries. SSO, audit log,
  priority support.
- **Enterprise** — contact sales. On-prem option, custom SLA.

Pricing is a marketing decision, not an engineering one. The
control-plane code doesn't hardcode numbers; plan limits live in a
`plans` table.

## Deployment

- Fly.io app with Postgres on Neon (or RDS if we want VPC peering
  later) and Redis on Upstash.
- Behind Cloudflare for DDoS protection and rate limiting at the
  edge.
- Internal network to the vectorless-server via Fly's private
  6PN or a VPC peering connection.

See [DEPLOYMENT.md](./DEPLOYMENT.md) for the full stack.

## Tech stack choice

**Language:** probably Go, to match the rest of the stack and reuse
`pkg/db` / `pkg/storage` patterns. The alternative would be Rust
(performance, but overkill for CRUD) or TypeScript (huge ecosystem for
SaaS plumbing, but splits the team).

**Framework:** same chi-based HTTP setup as the server, or Connect-RPC
if we want the admin API to be proto-defined as well (probably yes).

**Stripe SDK:** official `github.com/stripe/stripe-go`.

**Auth library:** hand-roll session + cookie handling. Adding Auth0 /
Clerk / WorkOS comes later if enterprise SSO sales require it.

**Email:** transactional provider — Resend or Postmark. Templated
via Go `text/template` + a theme. Only types: verify email, password
reset, invoice failed, invite teammate.

## Open questions

- **Region strategy.** Do we start with one region (US-East) and add
  others for compliance? Multi-region introduces data-residency
  complexity that isn't worth it for the first 100 customers.
- **BYO-LLM.** Larger customers will want to bring their own Anthropic
  / OpenAI keys so we don't mark up tokens. The control plane should
  support this as a plan feature.
- **Audit log export.** Enterprise requirement, often a compliance
  checkbox. Schema is easy; streaming export to customer S3 is the
  harder part.
- **Role customisation.** The four canned roles are fine for v1;
  custom roles via a permissions matrix is a later feature.

## Related docs

- [DASHBOARD.md](./DASHBOARD.md) — the UI for this backend.
- [SERVER.md](./SERVER.md) — what sits behind the control plane.
- [DEPLOYMENT.md](./DEPLOYMENT.md) — infra for `api.vectorless.dev`.
