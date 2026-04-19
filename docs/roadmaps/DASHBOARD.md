# Dashboard roadmap

> Design doc: [../DASHBOARD.md](../DASHBOARD.md)

The web UI at `app.vectorless.dev`. Next.js, deployed to Cloudflare
Pages. Lives in its own repo (`vectorless-dashboard`, private) from
the start.

## Phase 0 — scaffold + auth

One-line: a logged-in user can see a page that says their name.

- [ ] Next.js 15 (App Router) + TypeScript + Tailwind v4
- [ ] shadcn/ui installed
- [ ] TanStack Query configured
- [ ] NextAuth.js with GitHub + Google providers
- [ ] Session proxied to the control plane session cookie (no
      duplicate session store)
- [ ] `/login`, `/logout`, `/onboarding` routes
- [ ] Org switcher in the top-right (reads memberships from control
      plane)
- [ ] Empty-state layout: sidebar + content area
- [ ] Cloudflare Pages deploy on `main` -> `app.vectorless.dev`

---

## Phase 1 — documents + query

One-line: you can upload a doc and run a query from the UI.

- [ ] **Documents page**
  - [ ] List view with status pill (pending / ingesting / ready /
        failed)
  - [ ] Upload (drag-drop + file picker) -> `POST /v1/documents`
  - [ ] Detail view: metadata, section tree, delete button
  - [ ] Tree viewer (expand/collapse, click to view section)
  - [ ] Section detail: title + summary + raw content

- [ ] **Query playground**
  - [ ] Pick a document
  - [ ] Textarea for query, "Run" button
  - [ ] Show returned sections with snippet + highlight
  - [ ] Show timing, token usage, cost
  - [ ] Copy-as-curl for the request

- [ ] **API keys page**
  - [ ] List keys with prefix, created_at, last_used_at
  - [ ] Create key (one-time reveal)
  - [ ] Revoke key (with confirmation)

---

## Phase 2 — usage + billing

One-line: the account tab.

- [ ] **Usage dashboard**
  - [ ] Queries/day line chart (last 30 days)
  - [ ] Tokens in/out stacked bar
  - [ ] LLM cost breakdown by provider
  - [ ] Top documents by query volume
  - [ ] Storage used + document count vs plan limit

- [ ] **Billing**
  - [ ] Current plan + renewal date
  - [ ] Upgrade / downgrade flow (Stripe Checkout)
  - [ ] Update payment method (Stripe Customer Portal)
  - [ ] Invoice history
  - [ ] Projected cost for the current cycle

- [ ] **Team**
  - [ ] Member list with role
  - [ ] Invite by email
  - [ ] Remove member
  - [ ] Transfer ownership

---

## Phase 3 — polish + ergonomics

One-line: things that make the product feel alive.

- [ ] **Realtime status**
  - [ ] SSE from control plane for ingest status
  - [ ] Optimistic updates on upload
  - [ ] Toast on document ready / failed

- [ ] **Search**
  - [ ] ⌘K command palette (docs, keys, settings)
  - [ ] Full-text document title search

- [ ] **Dark mode**
  - [ ] System + manual toggle
  - [ ] Persisted per user

- [ ] **Empty states + onboarding**
  - [ ] First-run wizard: upload a sample doc, run a sample query
  - [ ] Tooltips on the query playground explaining each column

- [ ] **Accessibility**
  - [ ] axe-core in CI
  - [ ] Keyboard-only nav works end-to-end
  - [ ] Colour contrast passes WCAG AA

---

## Phase 4 — enterprise UX

- [ ] SSO login flow (SAML initiate button on custom domain)
- [ ] Audit log viewer (filter, export CSV)
- [ ] Data residency badge + region chooser on signup
- [ ] Seat management for SCIM-provisioned teams

---

## Phase 5 — marketing integration

- [ ] Public doc-share links (opt-in, read-only tree view)
- [ ] Embeddable query widget (`<script src="...">`) for docs sites
- [ ] Changelog page driven by the blog RSS feed

---

## Cross-cutting

- [ ] e2e tests with Playwright on critical flows (signup, upload,
      query, key mint, upgrade)
- [ ] Storybook for shadcn-derived components
- [ ] Bundle size budget: < 200kb JS on first load per route
- [ ] Lighthouse perf > 90 on dashboard home

## Known issues / deferred

- [ ] Native mobile app — not planned; PWA manifest is enough
- [ ] Offline mode — not planned; dashboard is online-only
- [ ] Marketing site (`vectorless.dev`) is a separate Next.js app,
      own roadmap not worth a full file yet

## Related

- [../DASHBOARD.md](../DASHBOARD.md) — design doc.
- [CONTROL-PLANE.md](./CONTROL-PLANE.md) — the API the dashboard
  consumes.
- [SDKS.md](./SDKS.md) — the TS SDK the dashboard uses internally.
