# Stores

> A **store** is a named collection within an org — the unit you
> upload documents into and query against. Think Pinecone index,
> Qdrant collection, S3 bucket: the addressable container for a body
> of knowledge.

## Purpose

Today the tenancy model is two levels:

```
Org ─▶ Documents ─▶ Sections
```

Every document in an org lives in one undifferentiated pool. That's
fine for a demo, wrong for real use: a team has *multiple* bodies of
knowledge — a research library, a compliance corpus, a product-docs
set — and wants to query within one, not across all of them.

Stores add the missing middle:

```
Org ─▶ Store ─▶ Documents ─▶ Sections
```

A store is "where stuff goes." You create a store, upload into it,
and queries run against it. It's also the natural home for a
**domain Profile** (see [PROFILES.md](./PROFILES.md)): a store *is* a
specialized collection — "my research-papers store", "my clinical-
guidelines store" — so the profile is a property of the store, not a
per-upload flag.

## What a store is (and where it lives)

A store is a **control-plane entity**. It has a name, a slug, an owning
org, a profile, timestamps, and (later) per-store quota + billing
rollups. The control plane owns its lifecycle.

The **engine** does not know stores as entities. It only sees an opaque
`store_id` scoping column on `documents` — exactly how it treats
`org_id` today. The engine never lists or creates stores; it just
filters by `store_id` when told to. This keeps the engine a clean,
single-responsibility retrieval core.

```
control plane          engine
─────────────          ──────
stores table           documents.store_id  (opaque scope column)
  id, org_id,          + every read filters by it
  name, slug,          (same mechanism as org_id today)
  profile, ...
```

## Stores carry the Profile

This is the key unification. The earlier profile-selection rule —
*declared → auto-detect → generic* — resolves cleanly through the
store:

1. **Store's profile** — if the store declares one (`research-paper`),
   every document uploaded into it gets that structuring. This is the
   "declared" path, set once per store instead of per upload.
2. **Auto-detect** — if the store's profile is `auto` (or unset), the
   engine detects per-document.
3. **`generic`** — fallback.

So "specialize a collection for a domain" = "set the store's profile".
The control plane reads the store's profile and passes it to the
engine as `X-Vectorless-Profile` alongside `X-Vectorless-Store`.

## The default store

Every org gets a **`default` store auto-created** with the org (the
same way we auto-create the org on signup). This keeps everything
backward-compatible and zero-friction:

- Existing single-pool behavior = one store called "Default".
- A new user can upload immediately without creating a store first.
- Uploads with no store specified land in `default`.
- The dashboard's "active store" defaults to `default`.

Stores are additive: nothing breaks for callers that never mention one.

## How scoping flows

Identical shape to the existing org scoping, one header deeper.

```
SDK / dashboard
  │  Authorization: Bearer <api-key>     (or session cookie)
  │  X-Vectorless-Store: <store_id>       (optional; default store if omitted)
  ▼
control plane
  • resolve key/session → org
  • resolve store: explicit header, else key's bound store, else org default
  • verify store belongs to org
  • look up store.profile
  ▼  proxy to engine, injecting:
  │    X-Vectorless-Org:     <org_id>
  │    X-Vectorless-Store:   <store_id>
  │    X-Vectorless-Profile: <profile>     (from the store)
  ▼
engine
  • documents.store_id filters every read/write
  • org_id still enforced too (defense in depth + tenant ops)
  • profile drives structuring at ingest
```

API keys may be **bound to a store** (null = org-wide). A store-bound
key needs no `X-Vectorless-Store` header — the binding implies it.
This is how an integrator pins one credential to one collection.

## Change surface by layer

### Engine (`vectorless-engine`)

Smallest change — `store_id` is a sibling of `org_id`.

| Area | Change |
|---|---|
| Migration | Add `documents.store_id TEXT NOT NULL DEFAULT '…default…'` + index `(org_id, store_id, created_at)` |
| `db.Document` | Add `StoreID` field |
| `db` CRUD | `GetDocument` / `ListDocuments` / `DeleteDocument` / `CountSections` / `LoadTree` take + filter `storeID` (alongside `orgID`); `*ForWorker` variants unaffected |
| `NewDocument` | Persist `store_id` |

### Server (`vectorless-server`)

| Area | Change |
|---|---|
| `requireOrgID` | Add `requireStoreID` (reads `X-Vectorless-Store`); thread into every documents/query handler |
| Connect handlers | `orgIDFromConnect` → also pull store header |
| Profile (later) | Read `X-Vectorless-Profile`, pass to ingest |

### Control plane (`vectorless-control-plane`)

The biggest net-new surface — stores are a CP entity.

| Area | Change |
|---|---|
| Migration | New `stores` table (id, org_id, name, slug, description, profile, timestamps; `UNIQUE(org_id, slug)`); add `api_keys.store_id` (nullable FK); add `store_id` to `usage_events` / `usage_daily` / `usage_monthly` |
| Model | `model/store.go` (Store + Create/Update requests); add `StoreID` to `APIKey` + `Principal` |
| Store layer | `store/stores.go` — Create / Get / GetByOrgSlug / ListByOrg / Update / Delete; `EnsureDefaultStore(orgID)` |
| Handlers | `handler/store.go` — CRUD at `/admin/v1/orgs/{orgId}/stores[/{storeId}]` |
| API keys | `HandleCreateAPIKey` accepts optional `store_id`; auth resolves key → store |
| Proxy | Inject `X-Vectorless-Store` (+ `X-Vectorless-Profile`) in `proxy.Forward` |
| Auth middleware | `APIKeyAuth`: resolve `apiKey.StoreID`; `SessionAuth`: read + validate `X-Vectorless-Store` against the org |
| Org create | `EnsureDefaultStore` whenever an org is created (signup/login/me, like auto-org) |

### Dashboard (`vectorless-dashboard`)

| Area | Change |
|---|---|
| `lib/cp-proxy.ts` | Add `getActiveStore()` + `storeId` option; inject `X-Vectorless-Store` next to org |
| State | New `StoreProvider` (parallel to `SessionProvider`) holding the active store; persist last choice to localStorage |
| Nav | `StoreSwitcher` dropdown in the sidebar header; new `/dashboard/stores` list+create page |
| Scoped views | documents list, upload, playground, analytics read the active store |
| API keys | optional store selector on key creation; show scope in the table; `apiKeys.store_id` column in the local DB |

### SDKs (`vectorless-sdk/{typescript,go,python}`) — all three exist

| Area | Change |
|---|---|
| Config | optional `storeId` / `StoreID` / `store_id` (default store for the client) |
| New methods | `createStore`, `listStores`, `getStore`, `deleteStore` |
| Existing methods | `ingestDocument` / `query` / `listDocuments` accept a per-call `storeId` override |
| Transport | send `X-Vectorless-Store` when set; precedence: per-call > client config > none |

### MCP (`vectorless-mcp`) — exists as a Next.js app

| Area | Change |
|---|---|
| Schema | `mcp_store` table; add `store_id` to `mcp_document` |
| Tools | add `vectorless_create_store`, `vectorless_list_stores`, `vectorless_get_store`, `vectorless_delete_store` |
| Handlers | `ingest`/`list`/`query` accept + validate `store_id`, pass to SDK |

## API shape

REST (control-plane management API):

```
POST   /admin/v1/orgs/{orgId}/stores            create
GET    /admin/v1/orgs/{orgId}/stores            list
GET    /admin/v1/orgs/{orgId}/stores/{storeId}  get
PATCH  /admin/v1/orgs/{orgId}/stores/{storeId}  update (name, profile)
DELETE /admin/v1/orgs/{orgId}/stores/{storeId}  delete (cascades documents)
```

Data-plane (`/v1/*`) is unchanged in shape — scoping rides the
`X-Vectorless-Store` header, so no path churn for the engine API or
the SDKs' existing method signatures.

SDK:

```ts
const store = await client.createStore({ name: "Research", profile: "research-paper" });
await client.ingestDocument(pdf, { storeId: store.id, filename: "paper.pdf" });
await client.query(docId, "what's the contribution?", { storeId: store.id });
// or pin the whole client to a store:
const research = new VectorlessClient({ apiKey, storeId: store.id });
```

## Decisions to confirm

- **Default store auto-created per org?** (Proposed: yes — backward
  compat + zero friction.)
- **Profile lives on the store?** (Proposed: yes — unifies stores +
  profiles; profile set once per collection.)
- **Store-bound API keys?** (Proposed: optional; null = org-wide.)
- **Engine scoping: store_id in addition to org_id?** (Proposed: both
  — org for tenant isolation, store for collection scope.)

## Open questions

- **Cross-store query** — ever allow a query spanning multiple stores
  in an org? (Default: no; one store per query. Multi-doc query already
  exists *within* a scope.)
- **Moving documents between stores** — supported, or immutable
  membership? (Lean: immutable for v1; re-upload to move.)
- **Per-store quota / billing** — when do usage rollups need
  `store_id` granularity vs. org-level? (Schema carries it from day
  one; enforcement later.)
- **Slug vs id in URLs** — dashboard routes by slug (`/stores/research`)
  or id?

## Phasing

1. **Phase 1 — CP + engine core.** `stores` table + CRUD; `store_id`
   on engine documents; default store auto-create; proxy injects the
   header; everything lands in `default` transparently. No UI yet.
2. **Phase 2 — dashboard.** StoreProvider + switcher + stores page;
   scope documents/upload/playground/analytics to the active store.
3. **Phase 3 — SDKs + MCP.** Store methods + per-call override across
   TS/Go/Python; MCP store tools.
4. **Phase 4 — profile-on-store + store-bound keys + per-store usage.**

## Related docs

- [PROFILES.md](./PROFILES.md) — domains; a store carries a profile.
- [ENGINE.md](./ENGINE.md) — the `org_id` scoping `store_id` mirrors.
- [CONTROL-PLANE.md](./CONTROL-PLANE.md) — where stores live.
- [DATA.md](./DATA.md) — the schema stores extend.
- [roadmaps/STORES.md](./roadmaps/STORES.md) — the delivery checklist.
