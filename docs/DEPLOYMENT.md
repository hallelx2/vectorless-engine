# Deployment

> Where each piece of vectorless runs in production, and why.

## Principles

- **Portable by default.** The engine + server run anywhere containers
  run. Don't lock into one cloud at the platform level.
- **Stateless compute, externalised state.** Engine and server hold no
  persistent state — it's all in Postgres, S3, and the queue. This
  makes scaling, rollouts, and multi-region trivial later.
- **Two clouds maximum.** Vendor sprawl is a tax. Pick a primary
  compute provider, a primary data provider, a CDN — stop.
- **Managed services for data, self-run for compute.** Running
  Postgres ourselves doesn't pay off until > $50k/mo in DB spend.
  Running compute ourselves saves money earlier because containers
  are commodity.

## Three deployment targets

Vectorless has three customer-facing deployment models. The code and
images are identical; only the wrapping differs.

### 1. Self-host (Docker Compose)

For: individual developers, small teams, internal tooling.

One `docker-compose.yml` with:

- `engine` container (runs both server + worker roles).
- `postgres` container.
- `redis` or embedded queue.
- `minio` container (S3-compatible local storage).

Users clone the repo, `docker compose up`, open `localhost:8080`.
Zero auth by default; enable API-key auth in config for public use.

### 2. Self-host (Kubernetes)

For: enterprises running their own infrastructure.

Ship a Helm chart (`vectorless-helm` repo):

- `Deployment` for the server (horizontally scalable).
- `Deployment` for the worker (independently scalable, HPA on queue
  depth).
- `Service` + optional `Ingress` for the server.
- `Secret` for DB URL, S3 credentials, LLM API keys.
- Sensible defaults; overrideable via values file.

Users bring their own Postgres (RDS, CloudSQL), S3 (S3, GCS, R2),
LLM keys.

### 3. Managed SaaS (`api.vectorless.dev`)

For: everyone else.

We run the stack. Customers get an API key and never see infrastructure.

## The SaaS stack

Concrete services, as currently planned. Subject to revision as we
learn.

```
                 Cloudflare  (DNS, WAF, rate limit at edge)
                      |
                      v
                 api.vectorless.dev
                      |
                      v
                 Fly.io app: control-plane         <-- Go service
                      |                                 |
                      |                                 v
                      |                            Neon Postgres
                      |                            (control-plane DB)
                      |
                      v (Fly private 6PN)
                 Fly.io app: vectorless-server     <-- Go service
                 Fly.io app: vectorless-worker     <-- Go service
                      |                                 |
                      v                                 v
                 Neon Postgres                     Cloudflare R2
                 (engine DB)                       (document bytes)
                      |
                      v
                 Upstash Redis                     <-- queue + rate-limit counters
                      |
                      +-----> llmgate --> Anthropic / OpenAI / Gemini

   app.vectorless.dev (Cloudflare Pages)           <-- dashboard (Next.js)
   vectorless.dev     (Cloudflare Pages)           <-- marketing
```

### Why these choices

**Fly.io for compute.**

- Single-binary Go deploys in ~30 seconds.
- Multi-region from day one without Kubernetes.
- Generous free tier, cheap beyond it.
- Private networking between apps via 6PN (no VPC setup).
- WireGuard mesh for internal traffic — zero-trust by default.

Alternative considered: Kubernetes on EKS. Too much ops overhead for
solo development. When we grow, the Helm chart lets any customer run
us on K8s; we don't have to.

**Neon for Postgres.**

- Serverless, scales to zero when idle (important for early-stage cost).
- Branching: every PR gets a DB branch for preview deploys.
- Postgres 16, no vendor-specific extensions required.
- Easy migration to AWS RDS or CloudSQL if we ever outgrow it.

Alternative: Supabase. Bundles too much (auth, storage, realtime) that
we already handle elsewhere.

**Cloudflare R2 for object storage.**

- S3-compatible API (our driver works as-is).
- No egress fees — LLMs and clients pulling documents doesn't get
  taxed.
- Free up to 10GB, then very cheap.
- Single-region, but S3-compatible so multi-region replication is
  a config change later.

Alternative: AWS S3. Fine, but egress gets expensive as queries grow.

**Upstash Redis for queue + rate limiting.**

- Serverless Redis. Pay per request, scales to zero.
- REST API available for edge workers.
- For the engine queue, we use the Asynq driver against Upstash.

Alternative: River on Postgres. Simpler (one fewer service), but Asynq
on Redis scales better for high job volumes.

**Cloudflare (DNS + WAF + edge).**

- DDoS protection.
- Zero-config HTTPS.
- WAF rules block obvious abuse.
- Workers for any edge logic we need later.

**Cloudflare Pages for dashboard + marketing.**

- Free for commercial use (unlike Vercel Hobby).
- Deploys from GitHub org with no friction.
- Edge-by-default for the marketing site's landing page.

Alternative: Vercel Pro ($20/month). Slightly nicer DX for Next.js,
but more expensive and has the org / Hobby / commercial issue.

### Why not AWS end-to-end

We could run all of this on AWS (ECS + RDS + S3 + ElastiCache + Route53
+ CloudFront). Reasons we don't:

- Cold-start cost: AWS billing + ops + IAM + VPC setup is a
  multi-week project.
- No scale-to-zero: RDS + ElastiCache cost money at idle, Fly + Neon
  + Upstash don't.
- Vendor lock-in on a scale our revenue doesn't justify yet.

We move to AWS when: an enterprise customer requires VPC peering,
or when compute exceeds Fly's sweet spot (~$500/month).

## Container images

### Building

Multi-stage Dockerfile, already in the engine repo:

```
Stage 1: golang:1.25-alpine    -- build static binary
Stage 2: gcr.io/distroless/static -- runtime, ~10MB total
```

Distroless = no shell, no package manager, minimal CVE surface.

### Publishing

- GitHub Container Registry (GHCR): `ghcr.io/vectorless/engine`,
  `ghcr.io/vectorless/server`, etc.
- Tags:
  - `latest` — the most recent release.
  - `vX.Y.Z` — exact version.
  - `sha-<short>` — commit SHA for reproducibility.
  - `vX.Y` — floating pointer to latest patch of a minor version.

### Multi-arch

- `linux/amd64` (servers).
- `linux/arm64` (Graviton instances, Apple Silicon dev).

Built via `docker buildx` in CI.

## Configuration management

- **Environment variables** are the primary config surface. Every
  knob has a `VLE_*` env var.
- **YAML file** (optional) for local dev. Env vars override it.
- **No runtime config reloading.** Redeploy to change config.
  Simpler, fewer footguns.
- **No secrets in config files.** API keys, DB URLs — all env vars.

Production secrets:

- Fly.io: `fly secrets set VLE_ANTHROPIC_API_KEY=...`.
- Kubernetes: `Secret` mounted as env.
- Cloudflare Workers (if relevant): Workers KV or Secrets.

## CI / CD

GitHub Actions. Per repo:

### On push to `main`

1. Lint (`golangci-lint`).
2. Test (`go test ./...`).
3. Build multi-arch image.
4. Push to GHCR with `:main` and `:sha-<short>` tags.
5. (Optional) deploy to staging: `fly deploy --config fly.staging.toml`.

### On tag `vX.Y.Z`

1. All of the above.
2. Tag image as `vX.Y.Z` + `latest`.
3. Build release binaries via `goreleaser` — Linux, macOS, Windows,
   amd64 + arm64.
4. Publish release on GitHub with binaries + SBOM.
5. Deploy to production: `fly deploy --config fly.toml`.

### Supply chain

- **SBOM** generated by `syft` and attached to releases.
- **Image signing** with `cosign` (keyless, via GitHub OIDC).
- **Dependabot** for Go module updates; auto-merge patch bumps after CI.

## Scaling

### Server

- Stateless. Scale out horizontally behind a load balancer.
- `vectorless-server` replicas: target 60-70% CPU under load; HPA
  driven by CPU or request rate.
- Session affinity: **not required.** Any request goes to any replica.

### Worker

- Stateless. Scale out on queue depth.
- Asynq ships a metrics exporter; HPA or Fly autoscaler triggers on
  queue length per replica.
- Start with 1 replica; grow.

### Database

- Start on Neon's smallest tier.
- Move to paid tier when connection count or storage demands it.
- Read replicas for the engine DB if query volume outgrows write
  volume (not soon).

### LLM rate limits

- Provider-level rate limits (Anthropic: 50 RPM default, higher on
  paid) are the real ceiling long before compute.
- `llmgate` router with fallback to secondary providers absorbs bursts.
- Monitor `llm_request_duration_seconds` and 429 counters; request
  higher limits from providers as volume grows.

## Observability

### Logging

- `slog` structured JSON in prod.
- Shipped to Axiom (Axiom free tier is generous) or Datadog when
  enterprise customers demand SIEM integration.
- Retention: 30 days hot, 90 days cold.

### Metrics

- Prometheus scrape endpoint (`/metrics`) on every service.
- Prometheus running in the Fly private network, Grafana Cloud for
  dashboards (free tier).
- Golden signals per service: rate, errors, duration. Plus LLM tokens
  and $ spent per minute.

### Tracing

- OpenTelemetry. OTLP exporter to Grafana Tempo (free tier) or
  Honeycomb.
- Spans on: HTTP handler, queue job, parse, summarise, LLM call.

### Alerting

- Alerts via Grafana Cloud -> PagerDuty (once paying customers exist).
- Pages on: 5xx rate > 1% for 5 min, DB down, queue depth > 1k for
  15 min.
- Warnings on: LLM spend > daily budget, 4xx spike, slow query p95.

## Disaster recovery

### Backup strategy

- **Postgres:** Neon continuous backup (point-in-time restore to any
  second in the last 7 days on paid tier).
- **Object storage:** R2 lifecycle policy with versioning on all
  objects for 30 days; cross-region replication to a second bucket
  for prod.
- **Control plane DB:** same as engine DB.

### RTO / RPO targets

- **RPO** (max data loss): 1 minute. Achieved by Neon PITR + R2
  versioning.
- **RTO** (max downtime): 4 hours for engine, 1 hour for control plane
  (because checkouts stop working without it).

### Drill schedule

Quarterly: restore from backup into a staging env and verify. Don't
skip this; untested backups are wishes.

## Runbooks

Each service ships with a `RUNBOOK.md` describing:

- How to deploy.
- How to roll back.
- How to scale up / down.
- How to read the dashboards.
- Common alerts and their remediation.

These live next to the code, not in a wiki.

## Open questions

- **Multi-region launch.** When does it make sense? Probably when an
  enterprise customer asks (data residency). Before that, one region
  is correct.
- **On-prem packaging.** Some enterprise customers will want "run this
  entirely on our network." Helm chart + air-gapped image mirror is
  the baseline. Pricing tier, not a technical project.
- **Cost-attribution at scale.** When individual customers drive
  different LLM spend, we need per-org cost accounting in the control
  plane to price accurately. Already sketched — see
  [CONTROL-PLANE.md](./CONTROL-PLANE.md).

## Related docs

- [ARCHITECTURE.md](./ARCHITECTURE.md) — the layers being deployed.
- [CONTROL-PLANE.md](./CONTROL-PLANE.md) — the SaaS backend that sits
  in front.
- [DATA.md](./DATA.md) — what lives in each data service.
