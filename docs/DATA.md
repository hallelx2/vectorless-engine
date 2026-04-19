# Data model

> Why Postgres, what the schema looks like, where bytes live.

## The three-primitive rule

Every piece of vectorless state lives in exactly one of three places:

1. **Postgres** — structured state. Documents, sections, lifecycle,
   metadata, tree summaries. The database of record.
2. **Object storage (S3-compatible)** — raw bytes. Original uploaded
   documents, section content that is too big for a DB row.
3. **Queue** — transient work items. Ingest jobs. Never the source of
   truth for anything.

If it doesn't fit one of these three, we don't store it. This keeps
the deployment model simple: one database, one bucket, one queue.

## Why Postgres, not NoSQL

The tree *looks* like a document-store use case — it's hierarchical,
you'd think MongoDB or Firestore is the natural fit. Resist this
instinct.

80% of what we actually query is not the tree body, it's the metadata
around it:

- "list documents with status=ready, newest first, paginated" —
  relational query.
- "count documents per tenant for billing" — aggregate.
- "find sections whose summary contains these keywords" — full-text
  search.
- "cascade-delete everything when a document is deleted" — referential
  integrity.

Every one of those is free in Postgres and annoying in NoSQL stores.
And we do not have the one problem that forces NoSQL: we are not doing
petabyte-scale random-key lookups against a schema-less store.

**Postgres + JSONB gives us the "document store feel" where we want
it (per-doc snapshots, flexible metadata) without leaving the
relational world.** One database, one set of operational patterns.

## Schema

### `documents`

```
id             text PRIMARY KEY               -- "doc_<uuid>"
title          text NOT NULL
content_type   text NOT NULL                  -- "application/pdf", "text/markdown", ...
source_ref     text NOT NULL                  -- key into object storage
status         text NOT NULL                  -- pending | parsing | summarizing | ready | failed
error_message  text NOT NULL DEFAULT ''
byte_size      bigint NOT NULL DEFAULT 0
metadata       jsonb NOT NULL DEFAULT '{}'    -- free-form per-doc metadata
tree_snapshot  jsonb                          -- (planned) denormalised tree.View
created_at     timestamptz NOT NULL DEFAULT now()
updated_at     timestamptz NOT NULL DEFAULT now()
```

Indexes:

- PK on `id`.
- `(status, created_at DESC)` for the list endpoint.
- GIN on `metadata` for future JSONB queries.

**Lifecycle:** `pending -> parsing -> summarizing -> ready` on the
happy path, `-> failed` on any stage failure (with `error_message`
populated).

### `sections`

```
id              text PRIMARY KEY              -- "sec_<uuid>"
document_id     text NOT NULL REFERENCES documents(id) ON DELETE CASCADE
parent_id       text REFERENCES sections(id) ON DELETE CASCADE  -- nullable for root
ordinal         int NOT NULL                  -- position among siblings
depth           int NOT NULL                  -- 0 for root
title           text NOT NULL
summary         text NOT NULL DEFAULT ''      -- LLM-generated or excerpt fallback
content_ref     text NOT NULL DEFAULT ''      -- key into object storage for full text
token_count     int NOT NULL DEFAULT 0
metadata        jsonb NOT NULL DEFAULT '{}'
created_at      timestamptz NOT NULL DEFAULT now()
updated_at      timestamptz NOT NULL DEFAULT now()
```

Indexes:

- PK on `id`.
- `(document_id, depth, ordinal)` for tree load ordering.
- `(parent_id)` for children lookup.

Self-referential via `parent_id`. `CASCADE DELETE` means removing a
document cleans up all its sections automatically.

### `schema_migrations`

Standard migrations tracking table. Engine applies embedded SQL files
from `internal/db/migrations/*.sql` at boot in ID order. Idempotent.

### What the engine does NOT store in Postgres

- **Raw document bytes.** Goes into object storage at
  `documents/<doc_id>/source/<filename>`.
- **Full section content.** Also object storage, at
  `documents/<doc_id>/sections/<section_id>.txt`. Postgres gets only
  the `content_ref` key.
- **Vector embeddings.** None exist. By design.
- **User accounts, billing, org data.** Control plane's database, not
  the engine's.

## The `tree_snapshot` optimisation

Planned, not shipped yet. Idea:

After ingestion completes, the engine computes the full `tree.View`
(the compact representation used for LLM reasoning) and writes it to
`documents.tree_snapshot` as JSONB.

At query time, `LoadTree` becomes a single-row read instead of a
recursive sections walk. Saves one Postgres round-trip and all the
ORM-style reconstruction that goes with it.

Trade-off: the snapshot goes stale if someone edits sections directly.
We solve that by only writing the snapshot at the end of ingest (when
everything is consistent) and invalidating it on any re-ingest. Since
we don't support "edit a section" as a public API, staleness is a
non-issue.

When this lands, it's a migration + a write at the end of the ingest
pipeline. Non-breaking.

## Object storage layout

```
<bucket>/
  documents/
    <doc_id>/
      source/
        <original_filename>          <-- raw uploaded bytes
      sections/
        <sec_id_1>.txt
        <sec_id_2>.txt
        ...
```

- One prefix per document. Deleting a document is one `DELETE` in
  Postgres (cascades to sections) plus one prefix delete in storage.
- Text-only section files. If future ingest pipelines output other
  shapes (parsed AST, syntax-highlighted HTML), put them under
  additional subdirectories (`sections/ast/<id>.json`, etc.).
- Content-type is stored per object for HTTP serving.

### Storage driver interface

```go
type Storage interface {
    Put(ctx, key, io.Reader, Metadata) error
    Get(ctx, key) (io.ReadCloser, Metadata, error)
    Delete(ctx, key) error
    DeletePrefix(ctx, prefix) error
    SignedURL(ctx, key, ttl) (string, error)   // optional, nil-error if unsupported
}
```

Drivers:

- **Local** — filesystem, for dev.
- **S3-compatible** — AWS S3, Cloudflare R2, MinIO, Backblaze B2,
  Google Cloud Storage (via S3 interop), DigitalOcean Spaces. One
  driver, many endpoints.
- **GCS / Azure** — optional, add only when a user asks.

## Queue

Queue is transient. Failing jobs retry; succeeding jobs are discarded.

### Interface

```go
type Queue interface {
    Enqueue(ctx, Job) error
    Register(kind Kind, handler Handler)
    Start(ctx) error
    Close() error
}
```

### Job kinds

- `ingest_document` — run the parse -> persist -> summarise pipeline
  for a newly-uploaded document.
- (future) `reingest_document`, `compact_tree`, `warm_cache`.

### Drivers

- **River** (default) — Postgres-backed. Same DB as the data plane,
  one fewer service to run, ACID semantics for enqueue.
- **Asynq** — Redis-backed. Higher throughput, needs Redis.
- **QStash** — HTTP-based. Good for serverless deploys on Cloudflare
  Workers or Vercel where Postgres and Redis aren't always available.

## Migrations

### Philosophy

- SQL, not an ORM migration DSL. Raw SQL is the forever-language.
- Embedded into the binary via `//go:embed migrations/*.sql`. No
  separate migration tool to install.
- Applied automatically at boot, tracked in `schema_migrations`.
  Idempotent.
- One-way. Down migrations are a 2005 practice; in 2026, forward-only
  plus a restore-from-backup plan is how grown-up services work.

### Naming

```
0001_init.up.sql
0002_add_tree_snapshot.up.sql
0003_add_metadata_gin.up.sql
```

Numeric prefix for ordering, descriptive slug. No timestamps —
branches should merge cleanly; renumber if two migrations land with
the same prefix.

### Rollout strategy

All migrations must be **backwards-compatible with the previous
engine version** for the duration of a rolling deploy:

- Adding a column with a default: safe.
- Adding an index: usually safe; `CREATE INDEX CONCURRENTLY` for
  large tables.
- Adding a NOT-NULL column: two migrations — (1) add nullable with
  default, backfill, (2) next release adds NOT NULL constraint.
- Dropping a column: two releases — (1) stop writing to it, (2)
  remove it in the next release.

This discipline means zero-downtime deploys, always.

## Full-text search (future)

When retrieval needs keyword search as a hint layer alongside LLM
reasoning:

- Add a `summary_tsv tsvector` generated column on `sections`.
- GIN index on `summary_tsv`.
- Use `plainto_tsquery` for user queries.

This stays in Postgres. No Elasticsearch, no Meilisearch, no
specialised engine. Postgres full-text gets us 80% of the way for 5%
of the operational cost.

## Consistency and transactions

- Every engine write is wrapped in a transaction where multiple rows
  change atomically. Ingest: all sections written in one tx per
  document.
- Storage writes happen before DB writes. If the DB insert fails, the
  object is orphaned — cleaned up by a background reaper that looks
  for storage objects with no matching DB row. Eventual consistency,
  not lost data.
- Queue enqueue happens **after** the DB insert, in the same tx when
  using River (which makes this trivial — it's a SQL insert into a
  queue table). This gives us exactly-once enqueue semantics.

## Multi-tenancy in the engine

The engine has **zero tenant concept**. Every document belongs to
whatever logical tenant the calling layer tracks.

In SaaS deploys, the control plane prefixes every `document_id` with
the org ID or stores the org mapping in its own database. The engine
neither knows nor cares — it's just a key to it.

This means:

- No `org_id` columns in the engine schema.
- No row-level security policies.
- No "did this user ingest this document" checks — the control
  plane authorised the call before the engine saw it.

If this becomes painful (e.g. the control plane wants to enforce
org-scoped queries at the DB level for defence-in-depth), we add an
optional `tenant_id` column and a tenant-scoped connection pool.
Not before.

## Open questions

- **Hot / cold storage split.** Old documents could move to cheaper
  storage (S3 Glacier, R2's infrequent-access tier). Worth it only
  at scale.
- **Per-section versioning.** Re-ingesting a document today blows
  away old sections. Stable IDs for unchanged sections + version rows
  for changed ones would let us cite "section X as of date Y."
  Deferred to Phase 4.
- **Cross-region replication.** If SaaS goes multi-region, the engine
  DB and storage need a replication strategy. Logical replication
  + S3 cross-region replication is the baseline.

## Related docs

- [ENGINE.md](./ENGINE.md) — what produces and consumes this data.
- [ARCHITECTURE.md](./ARCHITECTURE.md) — where the data layer sits.
- [DEPLOYMENT.md](./DEPLOYMENT.md) — which managed services host the
  DB, bucket, queue.
