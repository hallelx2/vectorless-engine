# Document Profiles

> Domain-aware structuring. The layer that turns a generic heading
> tree into a navigable, *intelligent* map an agent can reason over
> in a specialized domain.

## Purpose

The engine's promise is: hand an agent a navigable map of a document
and it does the rest — no embeddings, no RAG, no vector metrics (see
[ENGINE.md](./ENGINE.md)). Today that map is **structurally generic**:
it knows "this is a heading at depth 2", not "this is the *Methods*
section" or "this is a Grade-A *recommendation*".

Generic structure is a great MVP and the right foundation. But
Vectorless earns its keep in **specialized domains** — research
papers, clinical guidelines, legal contracts, financial filings —
where documents follow strong conventions and an agent benefits from
*semantic* navigation, not just structural. "Jump to the Methods
section." "Show me every recommendation with evidence grade A." "What
were the contributions and how were they validated?"

A **Document Profile** is the unit that encodes a domain's conventions
and projects them onto the map. Profiles are the *guards* that make
Vectorless reliable for a specialized system: a schema that constrains
and enriches the structure so agents navigate meaning, not just
indentation.

## The core idea

A profile takes the generic tree the parser produced and returns the
same tree with three things added:

1. **A `Kind` on every section** — the canonical section type in that
   domain (`Abstract`, `Methods`, `Recommendation`, `Dosage`, …).
2. **Typed metadata per section** — structured fields the domain cares
   about (`evidence_grade=A`, `dataset=WMT14`, `population=adults`).
3. **Cross-references** — links the raw outline doesn't capture
   (citations, "see §3", recommendation → supporting evidence).

The parser still does the structural work (headings → tree). The
profile does the *semantic* work on top. They compose; neither knows
about the other's internals.

```
bytes ──parser──▶ generic tree ──profile──▶ typed, enriched map
        (structure)              (semantics)
```

## What a profile is

A profile is a pluggable module, the same pattern as `pkg/parser`.
One profile per domain; a registry routes a document to the best fit.

```go
// pkg/profile

// Kind is a domain-canonical section type. Open string, not a global
// enum — each profile defines its own vocabulary as constants.
type Kind string

// CrossRef is a typed edge the raw outline didn't capture.
type CrossRef struct {
    From SectionID
    To   SectionID // or external (a citation key, a URL)
    Rel  string    // "cites" | "evidence_for" | "see_also" | ...
}

type Profile interface {
    // Name is the stable id ("research-paper", "medical-guideline").
    Name() string

    // Detect returns 0..1 confidence that this profile fits the
    // document, given its outline + lightweight metadata. Cheap:
    // title/heading heuristics only, no LLM, no content fetch.
    Detect(t *tree.Tree, meta DocMeta) float64

    // Apply classifies every section into a Kind, extracts typed
    // metadata, and returns cross-references. May make a small number
    // of LLM calls (ideally one batched call per document). The
    // tree is annotated in place; cross-refs are returned.
    Apply(ctx context.Context, t *tree.Tree, llm llm.Client) ([]CrossRef, error)
}
```

`DocMeta` carries what's known before content fetch: filename,
content-type, page count, byte size, and the flat list of heading
titles. Enough for `Detect` to decide cheaply.

### Classification: rules first, LLM to finish

Most section titles are unambiguous — "Abstract", "3 Methods",
"References", "Contraindications" classify by keyword/regex with zero
model cost. `Apply` runs the rule pass first, then makes **one batched
LLM call** for the residue: "Here is the outline of a research paper;
assign each of these untyped sections to one of {Method, Result,
…}." One call per document, cheap, and the profile's taxonomy bounds
the output so the model can't invent kinds.

This mirrors the engine's whole philosophy: structure does the heavy
lifting; the LLM is used surgically, not as a hammer.

## The Section gains a Kind

`tree.Section` adds a `Kind` field and the `sections` table adds a
`kind` column (indexed). The existing `metadata map[string]string`
column carries the typed fields — no schema churn there.

```
sections
  ...
  kind        TEXT NOT NULL DEFAULT ''   -- profile-specific type, e.g. "Method"
  core_kind   TEXT NOT NULL DEFAULT ''   -- shared cross-domain bucket
  metadata    JSONB                       -- typed domain fields (exists)
```

### Kind taxonomy: per-profile kinds over a shared core *(decided)*

Each profile defines its own precise vocabulary (`Method`,
`Recommendation`, `Dosage`, …) — that's what makes a domain
expressive. But every profile kind also maps to a **small shared
core** so cross-domain tooling (dashboards, filters, the generic UI)
can assume *something* about any document:

```
core_kind ∈ { meta | body | reference | appendix | navigation }
```

| Profile kind | core_kind |
|---|---|
| Abstract, Contributions | `meta` |
| Method, Result, Recommendation, Dosage | `body` |
| References | `reference` |
| Appendix, Attention Visualizations | `appendix` |
| "Part II" (container with no own text) | `navigation` |

So a generic filter like "exclude references" or "only substantive
body" works on *any* document via `core_kind`, while a research-paper
tool filters precisely on `kind = "Method"`. Profiles own the mapping;
the core set is fixed and small.

The tree `View` (what the agent reasons over) exposes `kind` next to
`title` + `summary`, so the ToC the agent reads is already typed:

```
[Abstract]            Attention Is All You Need — abstract
[Method]   §3 Model Architecture — encoder/decoder with multi-head attention
[Result]   §6 Results — BLEU 28.4 on WMT14 EN-DE, new SOTA
```

A `kind` column also unlocks **structural query filters**: "select
only `Recommendation` sections", "list all `Result` nodes" — without
an LLM call at all.

## Domain catalog

### `generic` (baseline, ships first)

A no-op profile: `Kind = ""` for everything, no extraction, no
cross-refs. This is exactly today's behavior. Every document that no
specialized profile claims falls back to `generic`, so adding profiles
is strictly additive — zero regression risk.

### `research-paper`

| | |
|---|---|
| **Detect** | Has an `Abstract` + `References`; arXiv-style filename or DOI; IMRaD-ish heading set. |
| **Kinds** | `Abstract`, `Contributions`, `RelatedWork`, `Method`, `Experiment`, `Result`, `Limitation`, `Conclusion`, `References`, `Appendix`, `Figure`, `Table` |
| **Metadata** | `datasets`, `metrics`, `figure_refs`, `table_refs`, `equation_refs` |
| **Cross-refs** | citation graph (section → reference keys); claim → experiment that supports it |
| **Agent win** | "main contribution + how validated?" → `Contributions` → `Experiment` → `Result` |

### `medical-guideline`

| | |
|---|---|
| **Detect** | Recommendation/evidence-grade phrasing; GINA/NICE/WHO-style structure; ICD/drug terms. |
| **Kinds** | `Recommendation`, `Evidence`, `Population`, `Intervention`, `Dosage`, `Contraindication`, `Monitoring`, `References` |
| **Metadata** | `evidence_grade` (A/B/C/D), `condition`, `drug`, `population`, `strength_of_recommendation` |
| **Cross-refs** | recommendation → its evidence source; intervention → contraindication |
| **Agent win** | "first-line Tx for X + evidence grade?" → filter `Recommendation` by `condition=X`, return with `dosage` + `evidence_grade` |

Future candidates: `legal-contract` (clauses, definitions, exhibits,
obligations), `financial-filing` (10-K MD&A, risk factors,
statements), `api-docs`, `clinical-note` (SOAP).

## Profile selection: declared, else auto-detect *(decided)*

A document resolves its profile in this order:

1. **Declared** — if the caller sets `X-Vectorless-Profile:
   research-paper` (or the SDK arg), use it. Explicit, predictable,
   best for a specialized system that *knows* its corpus.
2. **Auto-detected** — otherwise run every profile's `Detect` and pick
   the highest-confidence one above a threshold. Detection is cheap
   (outline heuristics, no LLM), so always-on costs nothing.
3. **`generic`** — if nothing clears the threshold, fall back to the
   no-op profile (today's behavior).

The detection confidence threshold is the one knob left to tune (start
conservative, ~0.6, so a weak match prefers `generic` over a wrong
guess).

## How it plugs into the engine

Minimal, surgical changes — profiles slot between existing stages.

- **`pkg/profile/`** — new package: `Profile` interface, `Registry`,
  `generic`, `research-paper`. Same shape as `pkg/parser`.
- **`pkg/tree`** — `Section.Kind` field; `View` exposes it.
- **`pkg/db`** — `kind` column + migration; reads/writes it.
- **`pkg/ingest`** — after `persistTree`, run the selected profile's
  `Apply`, persist `kind` + metadata + cross-refs. New lifecycle is
  `parsing → structuring → summarizing → ready` (structuring = profile
  apply). Idempotent like every other stage.
- **`pkg/retrieval`** — the chunked-tree / single-pass prompt becomes
  profile-aware: it's handed the profile's name + kind glossary so the
  model reasons with domain context ("Methods describes *how*, Results
  describes *outcomes*"). Query API gains an optional `kinds` filter.
- **server / API** — `X-Vectorless-Profile` on upload; `profile` +
  per-section `kind` in document/tree responses; `kinds[]` filter on
  `/v1/query`.

The `generic` fallback means none of this changes behavior for
documents that don't opt in.

## Why this and not…

- **…RAPTOR / embeddings / a vector tree?** RAPTOR builds a *synthetic*
  tree by embedding chunks, clustering them, and recursively
  summarizing clusters — it still needs a vector store and similarity
  search. That's the exact dependency Vectorless is positioned
  against. Profiles enrich the document's *real* structure; no
  embeddings enter the system. (If a document has *no* usable
  structure — flat OCR, a wall of text — a future `synthetic` profile
  could build a tree via recursive summarization *without* embeddings.
  That's the one place RAPTOR's idea is worth borrowing, minus the
  vectors.)
- **…a fine-tuned section classifier?** Overkill and brittle across
  domains. Rules + one bounded LLM call per doc is cheaper, debuggable,
  and adapts to a new domain by writing a profile, not retraining.
- **…just better prompts on the generic tree?** Prompts can't add a
  `kind` column you can filter on, or a citation graph, or guarantee a
  paper has a Methods section. Profiles make the structure *typed and
  queryable*, not just better-described.

## Baseline: how structuring works today (the MVP)

So the design builds on a written starting point:

- **Parsers** (`pkg/parser`): Markdown, HTML, DOCX, Text, PDF. Each
  returns a `*tree.Tree` from the document's headings. PDF uses a
  font-size heuristic (+ the `/Outlines` bookmark table when present),
  with recent hardening: pdfcpu decrypt-with-empty-password for
  owner-encrypted PDFs, a tuned word-space threshold (0.20·fontSize),
  invalid-UTF-8 / control-char scrubbing before storage + LLM, a
  mojibake-title guard, and publisher/license boilerplate filtering.
- **Structure**: heading hierarchy → `Section` tree. Multiple
  top-level sections are wrapped in a synthetic root so the whole
  document is reachable.
- **Summaries**: one LLM sentence per section (parallel via errgroup),
  with a truncated-excerpt fallback when the LLM call fails.
- **Retrieval**: `single-pass` (whole tree in one call) or
  `chunked-tree` (slice → parallel select → merge). The map is the
  index; no embeddings anywhere.
- **Multi-tenancy**: every section is scoped by `org_id` on the parent
  document; reads/writes filter by it.

Profiles are the **next layer up**: same parsers, same retrieval, same
no-embeddings stance — a semantic typing + enrichment pass in between.

## Decisions locked

- **Profile selection** — declared → auto-detect → `generic`. ✓
- **Kind taxonomy** — per-profile kinds, each mapped to a fixed shared
  `core_kind` ({meta, body, reference, appendix, navigation}). ✓
- **First domains** — `research-paper` *and* `medical-guideline`, built
  in parallel on one scaffold. ✓ (needs a clean medical fixture.)
- **Figures / tables** — metadata refs (`figure_refs`, `table_refs`)
  on the owning section, *not* first-class nodes. An agent sees "this
  section references Table 2"; we don't make Table 2 its own
  navigable node (revisit only if a domain demands it). ✓

## Open questions

- **Detection threshold** — what confidence makes auto-detect safe
  without false-claiming a `generic` doc? Start ~0.6, tune on real
  uploads.
- **Re-profiling** — if a better profile ships later, do we re-apply
  to existing docs? (Tie into the incremental re-ingest work in
  [ENGINE.md](./ENGINE.md) open questions.)
- **Cross-refs storage** — new `section_refs` table vs. edges in
  section metadata. A table is queryable; metadata is cheaper.
  (Phase 3 concern.)

## Related docs

- [ENGINE.md](./ENGINE.md) — the core engine this builds on.
- [DATA.md](./DATA.md) — Postgres vs object storage; the `sections`
  schema the `kind` column extends.
- [roadmaps/PROFILES.md](./roadmaps/PROFILES.md) — the phased delivery
  plan.
