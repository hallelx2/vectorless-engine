# Dashboard

> The web UI for the control plane. The human face of vectorless.

## Purpose

Give customers a browser UI to sign up, manage their org and API keys,
upload test documents, run test queries, and view usage / billing.
Everything the control plane exposes as API, the dashboard exposes as
UI.

## Repo

`vectorless-dashboard`. **Private.**

Tightly coupled to the control-plane API and to our design system.
Not useful standalone. Keeping it private avoids drive-by UX PRs and
keeps A/B-test configs out of public view.

## What the dashboard does

- Auth flows: signup, login, password reset, email verification, SSO
  (enterprise).
- Org management: create, rename, invite members, change roles.
- API keys: issue, name, scope, rotate, revoke.
- Documents: upload a doc to test ingest, view the tree, run a query,
  see selected sections highlighted.
- Usage: charts for ingests, queries, tokens, $ over time.
- Billing: view plan, upgrade, download invoices, change card.
- Settings: profile, notifications, webhook URLs.

## What the dashboard does not do

- **Engine logic.** Every action is a call to the control-plane API.
  The dashboard never reaches past the control plane.
- **Heavy data processing.** Tree rendering for a uploaded test doc is
  the biggest compute it does, and that's a server component anyway.
- **Embedded playground for customers' production data.** Test data
  uploaded in the dashboard is marked as scratch and auto-deletes
  after 30 days.

## Architecture

```
Browser
  |
  v
Next.js app (dashboard)          <-- SSR + client components
  |
  v
Control-plane /admin API         <-- session JWT
  |
  v
Postgres (control-plane DB)
```

The dashboard is a fully-hydrated Next.js app. Most pages are server
components that call the control plane server-side (faster initial
load, tokens never in the browser unnecessarily). Client components
handle interactive bits: forms, live charts, the tree viewer.

## Tech stack

- **Framework:** Next.js 15 (App Router). Server components for data,
  client components for interactivity.
- **UI kit:** shadcn/ui on Tailwind. Copy-paste components, no
  dependency on a heavy design system.
- **Auth:** NextAuth.js (Auth.js) with email/password via the control
  plane as the provider. Google + GitHub OAuth for faster signup.
- **Forms:** React Hook Form + Zod validation. Same Zod schemas are
  generated from the control-plane proto where possible.
- **State:** TanStack Query for server state. Zustand for the little
  client-only state there is (active org, theme).
- **Charts:** Recharts or Tremor — simple, composable, no D3 unless we
  really need custom viz.
- **Tree viewer:** custom component that renders the document tree,
  allows click-to-expand, highlights sections picked by a query.
- **Analytics:** PostHog (product analytics) + Vercel Analytics or
  Cloudflare Web Analytics (page views). PostHog self-hosted or cloud
  depending on cost.

## Design principles

- **Two-column layout.** Persistent left sidebar (orgs, docs,
  settings), main content on the right.
- **Keyboard-first where possible.** `⌘K` command palette for
  power users (jump to doc, create key, switch org).
- **Dark mode by default.** Developers expect it.
- **Density over spacing.** We are not a consumer app. Information
  density beats whitespace.
- **Zero marketing copy.** This is a product UI, not a landing page.
  The marketing lives at `vectorless.dev`, not `app.vectorless.dev`.

## Auth flow

1. `/signup` — email + password form.
2. Dashboard calls `POST /admin/v1/auth/signup`.
3. Control plane creates user + org (org name = "Personal"), sends
   verification email.
4. Dashboard shows "check your email."
5. User clicks verification link -> `/verify?token=...` -> control
   plane confirms, redirects to dashboard.
6. Session cookie set. User sees the dashboard.

SSO (enterprise plan): WorkOS or Clerk integration, configured per
org. Off for v1.

## Key screens

### Dashboard home

- "Hello, $name" header.
- Usage summary cards (this month: X docs, Y queries, Z tokens).
- Recent documents (last 10).
- Recent queries (last 10).

### Documents

- List view with search + status filter.
- "Upload" button opens a modal with drag-and-drop + file picker.
- Clicking a document opens the detail view.

### Document detail

- Left: the tree as a collapsible outline, depth-indented.
- Right: the query panel. Type a query, hit enter, see selected
  sections highlighted in the tree, with an inline preview of each
  picked section's content.
- Useful for debugging: "why did this query not return the section I
  expected?"

### API keys

- Table: key name, prefix, scopes, last-used, created-at.
- "Create key" flow — shows the full key once, then only the prefix.
- Scope checkboxes: `documents:read`, `documents:write`,
  `queries:run`.

### Usage

- Bar chart of queries / docs over the last 30 / 90 / 365 days.
- Current plan + quota progress bars.
- "Upgrade" CTA when > 80% of quota.

### Settings / billing

- Plan details, Stripe customer portal link for card + invoices.
- Webhook URL for async events.
- Org name, slug, danger zone (delete org).

## Deployment

- Vercel (Hobby while pre-revenue, Pro when commercial) **or**
  Cloudflare Pages.
- Preview deploys per PR.
- Production at `app.vectorless.dev`.
- Environment variables managed in the deploy platform's UI; no
  secrets in the repo.
- CDN cache: static assets only. All data requests hit the control
  plane live.

## Open questions

- **Onboarding.** First-time signup flow: do we drop them into a
  tutorial that ingests a sample doc, or a blank dashboard with an
  empty-state nudge? A/B test once there's traffic.
- **Multi-org UX.** Most users will be in one org. A tiny minority
  will be in 3+ (consultants, integrators). The active-org switcher
  should be unobtrusive for the 99% and fast for the 1%.
- **Inviting non-users.** Flow when an invitee doesn't have an account
  yet: pending invite stored on the org, accepted on signup.
- **Custom domains.** Enterprise customers want to embed the
  dashboard behind their own DNS. Deferred.

## Related docs

- [CONTROL-PLANE.md](./CONTROL-PLANE.md) — the backend this UI talks
  to.
- [ARCHITECTURE.md](./ARCHITECTURE.md) — where the dashboard sits in
  the stack.
