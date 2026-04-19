# Deployment roadmap

> Design doc: [../DEPLOYMENT.md](../DEPLOYMENT.md)

How vectorless actually gets into production — container images,
Fly.io apps, Neon DBs, R2 buckets, Cloudflare Pages, CI/CD, and
the self-host artefacts.

## Phase 0 — build + ship the engine

One-line: one-command local dev, one-command image publish.

- [x] Multi-stage Dockerfile (golang:1.25-alpine -> distroless)
- [x] `docker-compose.yml` for local dev (engine + Postgres + MinIO)
- [ ] Multi-arch image build (`linux/amd64`, `linux/arm64`) via
      `docker buildx`
- [ ] GHCR publishing (`ghcr.io/vectorless/engine`)
- [ ] Image tags: `latest`, `vX.Y.Z`, `vX.Y`, `sha-<short>`
- [ ] `goreleaser` config for binary releases (Linux/macOS/Windows,
      amd64/arm64)
- [ ] SBOM generation via `syft`, attached to GH releases
- [ ] Image signing with `cosign` (keyless via GH OIDC)

---

## Phase 1 — single-region SaaS live

One-line: `api.vectorless.dev` serves real traffic.

- [ ] **Fly.io apps**
  - [ ] `vectorless-server` Fly app, 2 replicas in `lhr` (or closest
        region)
  - [ ] `vectorless-worker` Fly app, 1 replica, autoscaler on queue
        depth
  - [ ] `vectorless-control-plane` Fly app, 2 replicas
  - [ ] Private 6PN networking between apps
  - [ ] Fly secrets: `VLE_DATABASE_URL`, `VLE_S3_*`,
        `VLE_ANTHROPIC_API_KEY`, `STRIPE_SECRET_KEY`, etc.

- [ ] **Data services**
  - [ ] Neon Postgres project for engine DB
  - [ ] Neon Postgres project for control-plane DB (separate)
  - [ ] Cloudflare R2 bucket for document bytes
  - [ ] Upstash Redis for queue + rate limits

- [ ] **DNS + edge**
  - [ ] `vectorless.dev` + `app.vectorless.dev` +
        `api.vectorless.dev` on Cloudflare
  - [ ] WAF rules: block known-bad ASNs, rate limit /v1/query
  - [ ] Cloudflare Pages project for dashboard
  - [ ] Cloudflare Pages project for marketing site

- [ ] **CI/CD**
  - [ ] GitHub Actions workflow per repo
  - [ ] On push to `main`: lint, test, build image, push to GHCR,
        deploy to staging
  - [ ] On tag: deploy to production, publish release

---

## Phase 2 — self-host artefacts

One-line: someone clones a repo and gets vectorless running on their
own infra.

- [ ] **Docker Compose bundle**
  - [ ] `vectorless-compose` repo (public) or folder in engine repo
  - [ ] `docker-compose.yml` with engine + Postgres + Redis + MinIO
  - [ ] README: zero-to-running in < 5 minutes
  - [ ] Sample `.env.example` with every knob documented

- [ ] **Helm chart**
  - [ ] `vectorless-helm` repo (public)
  - [ ] Deployments for server + worker
  - [ ] Service + optional Ingress
  - [ ] Secret templates
  - [ ] HPA on server CPU, HPA on worker queue depth (KEDA)
  - [ ] `values.yaml` with sensible defaults; all overridable
  - [ ] Published to an OCI-backed Helm repo

- [ ] **Terraform module** (opt)
  - [ ] `vectorless-terraform` repo (public)
  - [ ] Modules for: Fly deploy, AWS ECS deploy, GCP Cloud Run
        deploy
  - [ ] Opinionated but override-friendly

---

## Phase 3 — observability stack

One-line: we can tell why a deploy broke without SSHing anywhere.

- [ ] **Logs**
  - [ ] `slog` JSON output in prod across all services
  - [ ] Ship to Axiom (free tier) via Fly log shipper
  - [ ] Retention: 30d hot, 90d cold
  - [ ] Log correlation via request ID + trace ID

- [ ] **Metrics**
  - [ ] `/metrics` on every service
  - [ ] Prometheus running in Fly private net
  - [ ] Grafana Cloud (free tier) for dashboards
  - [ ] Golden-signals dashboard per service
  - [ ] LLM dashboard: tokens/min, $/min, 429 rate by provider

- [ ] **Tracing**
  - [ ] OTLP exporter to Grafana Tempo
  - [ ] Spans: HTTP handler, queue job, parse, summarise, LLM call
  - [ ] Trace sampling: 100% errors, 5% success

- [ ] **Alerting**
  - [ ] Grafana alerts -> PagerDuty (once paying customers)
  - [ ] Initial alerts: 5xx > 1% / 5m, DB down, queue depth > 1k /
        15m, LLM spend > daily budget

---

## Phase 4 — resilience + DR

One-line: an incident doesn't turn into a catastrophe.

- [ ] **Backups**
  - [ ] Neon PITR on paid tier (7-day window)
  - [ ] R2 versioning on all buckets (30-day window)
  - [ ] Cross-region R2 replication for prod bucket

- [ ] **DR drills**
  - [ ] Quarterly restore-from-backup drill into staging
  - [ ] Documented RTO (4h engine, 1h control plane) + RPO (1m)
  - [ ] Runbook reviewed + updated after each drill

- [ ] **Chaos**
  - [ ] Kill-an-app test (Fly app stop) — verify graceful
        degradation
  - [ ] Slow-DB test (toxiproxy in staging)
  - [ ] LLM-provider-down test (router fallback verification)

- [ ] **Runbooks**
  - [ ] `RUNBOOK.md` per service
  - [ ] Deploy / rollback / scale / common alerts
  - [ ] Reviewed on every major deploy

---

## Phase 5 — multi-region + enterprise

One-line: only when a real customer demands it.

- [ ] Second Fly region (`iad` for NA, `lhr` for EU) behind a
      region router
- [ ] Neon region-local read replicas
- [ ] R2 bucket per region
- [ ] Data residency flag in control plane drives routing
- [ ] [?] On-prem air-gapped install bundle (signed tarball + Helm)
- [ ] [?] AWS-native reference deployment (ECS + RDS + S3)
- [ ] SOC 2 Type I evidence collection starts here

---

## Cross-cutting

- [ ] Dependabot on every repo; auto-merge patch bumps after CI
- [ ] Renovate for Dockerfile base images
- [ ] Cost dashboard (Fly + Neon + R2 + Upstash + LLM) reviewed
      monthly
- [ ] Staging environment mirrors prod minus scale (1 replica each)
- [ ] Production deploys are boring: fast, reversible, observable

## Known issues / deferred

- [ ] True active-active multi-region needs conflict resolution
      work on the engine DB; single-primary + read-replicas is the
      pragmatic path until someone pays for more
- [ ] Self-host SSO (Authentik / Keycloak integration examples) —
      documentation-only for a while
- [ ] FedRAMP / HIPAA — not on roadmap; revisit when a qualifying
      customer shows up with budget

## Related

- [../DEPLOYMENT.md](../DEPLOYMENT.md) — design doc.
- [../CONTROL-PLANE.md](../CONTROL-PLANE.md) — the service most
  tightly coupled to this infra.
- [../REPOS.md](../REPOS.md) — which repos produce which artefacts.
