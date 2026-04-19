# Control plane roadmap

> Design doc: [../CONTROL-PLANE.md](../CONTROL-PLANE.md)

The control plane is the SaaS backend that sits in front of
`vectorless-server`. Lives in its own repo (`vectorless-cloud`,
private) from day one — it's the only piece that's never
self-hostable.

## Phase 0 — tenancy foundation

One-line: users can sign up, create orgs, mint API keys.

- [ ] Repo scaffold: Go + chi + Neon Postgres + slog
- [ ] Migrations: `users`, `orgs`, `memberships`, `api_keys`,
      `sessions`
- [ ] Email/password auth (argon2id hashing)
- [ ] OAuth login: GitHub, Google
- [ ] Session cookies (signed, httpOnly, SameSite=Lax)
- [ ] API key mint / list / revoke endpoints
- [ ] API keys stored as SHA-256 hashes; prefix shown in UI
- [ ] Org invite flow (email token)
- [ ] Role: `owner`, `admin`, `member`
- [ ] Minimal admin CLI: create user, grant internal role

---

## Phase 1 — proxy + metering

One-line: the control plane becomes the public front door to
`vectorless-server`.

- [ ] **Reverse proxy**
  - [ ] `api.vectorless.dev` terminates here, not at the server
  - [ ] Resolve API key -> principal (org_id, key_id, scopes)
  - [ ] Inject `X-Vectorless-Org` + `X-Vectorless-Principal` headers
        into the upstream request
  - [ ] Strip incoming auth headers before forwarding
  - [ ] Streaming-safe (don't buffer responses)

- [ ] **Usage metering**
  - [ ] Emit a usage event per request: org_id, endpoint, status,
        tokens_in, tokens_out, llm_cost_usd, duration_ms
  - [ ] Write to `usage_events` (append-only, partitioned by month)
  - [ ] Rollup job: hourly -> `usage_hourly`, daily -> `usage_daily`
  - [ ] `/v1/usage` endpoint: query by org + time range

- [ ] **Quota enforcement**
  - [ ] Per-plan limits (documents, queries/month, storage_bytes)
  - [ ] Check before forwarding; return 429 with plan upgrade hint
        when over
  - [ ] Soft limits (warn) vs hard limits (block)

- [ ] **Health of upstream**
  - [ ] `/v1/health` aggregates server + DB + queue
  - [ ] Circuit breaker around upstream calls

---

## Phase 2 — billing

One-line: Stripe wired up end-to-end.

- [ ] Stripe customer per org, created on first paid action
- [ ] Plans: `free`, `pro`, `team`, `enterprise`
- [ ] Plan limits loaded from a config file, not hardcoded
- [ ] Subscription lifecycle: trial -> active -> past_due -> canceled
- [ ] Stripe webhook handler: `customer.subscription.*`,
      `invoice.*`, `payment_intent.*`
- [ ] Webhook signature verification
- [ ] Webhook replay-safe (idempotency keys)
- [ ] Metered billing for overages (LLM tokens above plan)
- [ ] Invoice preview + download (PDF via Stripe)
- [ ] Dunning emails on failed payments (Stripe handles, we listen)
- [ ] Tax: Stripe Tax on (handles VAT / US sales tax)

---

## Phase 3 — operator tooling

One-line: the things we need to actually run the business.

- [ ] **Admin dashboard surface**
  - [ ] `/admin/*` routes gated by `role=internal`
  - [ ] Org search, user search
  - [ ] Impersonate-as-org (read-only) for support
  - [ ] Plan override (comp accounts, trials)
  - [ ] Manual invoice credits

- [ ] **Outbound webhooks for customers**
  - [ ] Register webhook URLs per org
  - [ ] Events: `document.ingested`, `document.failed`,
        `quota.exceeded`
  - [ ] HMAC signing, retries with backoff, dead-letter after 24h

- [ ] **Notifications**
  - [ ] Transactional email via Resend
  - [ ] Templates: welcome, invite, quota warning, payment failed
  - [ ] Per-user preferences

---

## Phase 4 — enterprise readiness

One-line: the checklist a procurement team hands you.

- [ ] **SSO**
  - [ ] SAML 2.0 via WorkOS or self-rolled
  - [ ] SCIM provisioning for team sync
  - [ ] Enforced-SSO toggle per org

- [ ] **Audit log**
  - [ ] Append-only `audit_events` table
  - [ ] Log: logins, key mint/revoke, plan change, member add/remove
  - [ ] Export as CSV / JSON
  - [ ] 1-year retention minimum

- [ ] **Data residency**
  - [ ] Org flag: `region = us | eu`
  - [ ] Requests routed to region-local server + DB
  - [ ] Cross-region is an explicit opt-in

- [ ] **Security posture**
  - [ ] SOC 2 Type I readiness checklist
  - [ ] Pen-test run before SOC 2 Type II
  - [ ] Public trust center page (policies, subprocessors)

---

## Phase 5 — polish

- [ ] (opt) Usage alerts (email/webhook when 80% of quota)
- [ ] (opt) Cost projection / "what would this cost next month"
- [ ] (opt) Team-level sub-quotas (split an org's budget)
- [ ] (opt) Referral / affiliate program
- [ ] (opt) Self-serve plan downgrade without support ticket

---

## Cross-cutting

- [ ] Migration policy: forward-only, review gate on any destructive
      change
- [ ] Secrets in Fly secrets, never in the repo
- [ ] Load test: 1000 rps through the proxy with p95 < 20ms overhead
- [ ] Chaos: kill upstream server, ensure graceful 503 + retry-after

## Known issues / deferred

- [ ] Multi-region active-active for the control plane DB is hard;
      one region + PITR is fine until a customer demands otherwise
- [ ] Usage rollups at huge scale may need ClickHouse; Postgres
      partitions buy us a long runway

## Related

- [../CONTROL-PLANE.md](../CONTROL-PLANE.md) — design doc.
- [DASHBOARD.md](./DASHBOARD.md) — the UI that talks to this.
- [../DATA.md](../DATA.md) — the control plane DB lives here.
