# Stores roadmap

> Design doc: [../STORES.md](../STORES.md)

Adding the Org → **Store** → Documents layer. A store is the named
collection you upload into and query against; it also carries the
domain [profile](../PROFILES.md).

## Current status (summary)

| Phase | Status |
|---|---|
| Phase 0 — Org → Documents (no stores) | Shipped (today) |
| Phase 1 — CP + engine core (`stores`, `store_id`, default store) | Not started |
| Phase 2 — dashboard (switcher, stores page, scoped views) | Not started |
| Phase 3 — SDKs + MCP store support | Not started |
| Phase 4 — profile-on-store, store-bound keys, per-store usage | Not started |

Everything is additive: a `default` store per org preserves today's
single-pool behavior with zero breakage.

## Phase 1 — control plane + engine core

The foundation. After this, every document silently belongs to a
store; existing flows keep working via the default store.

- [ ] **CP migration**: `stores` table (id, org_id, name, slug,
      description, profile, timestamps; `UNIQUE(org_id, slug)`).
      `api_keys.store_id` nullable FK. `store_id` on usage tables.
- [ ] **CP model**: `model/store.go`; `StoreID` on `APIKey` +
      `Principal`.
- [ ] **CP store layer**: Create / Get / GetByOrgSlug / ListByOrg /
      Update / Delete; `EnsureDefaultStore(orgID)`.
- [ ] **CP handlers + routes**: CRUD under
      `/admin/v1/orgs/{orgId}/stores`.
- [ ] **CP auto-default**: call `EnsureDefaultStore` wherever orgs are
      ensured (signup / login / me), mirroring auto-org.
- [ ] **CP auth**: `APIKeyAuth` resolves `apiKey.StoreID`; `SessionAuth`
      reads + validates `X-Vectorless-Store` against the org.
- [ ] **CP proxy**: inject `X-Vectorless-Store` in `proxy.Forward`
      (resolution: header → key binding → org default).
- [ ] **Engine migration**: `documents.store_id` + `(org_id, store_id,
      created_at)` index.
- [ ] **Engine db**: `Document.StoreID`; `GetDocument` /
      `ListDocuments` / `DeleteDocument` / `CountSections` / `LoadTree`
      filter by `store_id`; `NewDocument` persists it.
- [ ] **Server handlers**: `requireStoreID` + thread into documents /
      query / connect handlers.
- [ ] **Validate**: upload with + without a store header; confirm
      default-store fallback; confirm cross-store isolation (store A
      can't see store B's docs).

## Phase 2 — dashboard

- [ ] **`lib/cp-proxy.ts`**: `getActiveStore()` + `storeId` option;
      inject `X-Vectorless-Store`.
- [ ] **`StoreProvider`** (parallel to SessionProvider) + localStorage
      persistence of last-selected store.
- [ ] **`StoreSwitcher`** dropdown in the sidebar header.
- [ ] **`/dashboard/stores`** list + create + delete page; CP-proxy
      route(s) for store CRUD.
- [ ] **Scope existing views** to the active store: documents list,
      upload, playground, analytics.

## Phase 3 — SDKs + MCP

- [ ] **TS/Go/Python**: `storeId` client config; `createStore` /
      `listStores` / `getStore` / `deleteStore`; per-call `storeId`
      override on ingest / query / list; transport sends the header
      (precedence: per-call > config > none).
- [ ] **MCP**: `mcp_store` table + `store_id` on `mcp_document`;
      `vectorless_create_store` / `_list_stores` / `_get_store` /
      `_delete_store` tools; ingest/list/query accept + validate
      `store_id`.
- [ ] **Docs**: refresh SDKS.md + MCP.md examples with stores.

## Phase 4 — specialization + accounting

- [ ] **Profile on store**: store declares its profile; CP passes
      `X-Vectorless-Profile`; ingest applies it. (Depends on the
      Profiles Phase 1 scaffold.)
- [ ] **Store-bound API keys**: key creation UI + enforcement;
      store-bound key needs no header.
- [ ] **Per-store usage**: surface usage rollups by store in the
      dashboard analytics.

## Open questions (from design doc)

- [ ] Cross-store query (default: no).
- [ ] Move documents between stores (lean: immutable v1).
- [ ] Slug vs id in dashboard URLs.
- [ ] When per-store quota enforcement (vs. org-level) kicks in.

## Related

- [../STORES.md](../STORES.md) — the design doc.
- [../PROFILES.md](../PROFILES.md) — domains; carried on the store.
- [../../ROADMAP.md](../../ROADMAP.md) — root checkbox document.
