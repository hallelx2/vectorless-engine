# Document Profiles roadmap

> Design doc: [../PROFILES.md](../PROFILES.md)

The *when* and *what's next* for domain-aware structuring. Profiles
turn the generic heading tree into a typed, navigable map for a
specialized domain (research papers, medical guidelines, …).

## Current status (summary)

| Phase | Status |
|---|---|
| Phase 0 — generic baseline (heading tree) | Shipped (the MVP) |
| Phase 1 — profile scaffold + `research-paper` | Not started |
| Phase 2 — domain metadata extraction + `medical-guideline` | Not started |
| Phase 3 — cross-refs + semantic query API | Not started |

The baseline (Phase 0) is what ships today: parsers → heading tree →
per-section summaries → tree retrieval, hardened for real-world PDFs.
Everything below is additive; the `generic` profile preserves current
behavior with zero regression.

## Phase 1 — profile scaffold + research-paper

The foundation. Get one real profile working end-to-end against a
clean test fixture (arXiv "Attention Is All You Need").

- [ ] **`pkg/profile` package**
  - [ ] `Profile` interface (`Name`, `Detect`, `Apply`).
  - [ ] `Registry` that routes a document to the best-fit profile
        (same pattern as `pkg/parser`).
  - [ ] `generic` profile — no-op, `Kind=""`. The fallback.
  - [ ] `Kind` type + `CrossRef` type + `DocMeta` input.
- [ ] **`tree.Section.Kind`** field; `View` exposes `kind`.
- [ ] **DB**: `kind` column on `sections` (+ migration, indexed).
      Reads/writes carry it.
- [ ] **`research-paper` profile**
  - [ ] `Detect` from outline (Abstract + References + IMRaD-ish
        headings; arXiv/DOI filename hints). No LLM.
  - [ ] `Classify` — rule pass (keyword/regex on titles) + one
        batched LLM call for the residue, bounded by the kind
        vocabulary.
  - [ ] Kinds: Abstract, Contributions, RelatedWork, Method,
        Experiment, Result, Limitation, Conclusion, References,
        Appendix.
- [ ] **Ingest** runs the selected profile after `persistTree`
      (`parsing → structuring → summarizing → ready`). Idempotent.
- [ ] **Profile selection**: honor declared `X-Vectorless-Profile`;
      else auto-detect; else `generic`. (Resolve the
      declared/auto/both open question first.)
- [ ] **Validate** on the arXiv paper: every section gets a sensible
      kind; the map's ToC shows `[Method] §3 Model Architecture` etc.

## Phase 2 — domain metadata + medical-guideline

- [ ] **Research metadata extraction**: datasets, metrics, figure /
      table / equation refs into section `metadata`.
- [ ] **Figures / tables**: decide nodes vs. metadata refs; implement.
- [ ] **`medical-guideline` profile**
  - [ ] `Detect` from recommendation/evidence-grade phrasing.
  - [ ] Kinds: Recommendation, Evidence, Population, Intervention,
        Dosage, Contraindication, Monitoring, References.
  - [ ] Extract `evidence_grade`, `condition`, `drug`, `population`.
  - [ ] Validate against a representative guideline PDF (need fixture).
- [ ] **Retrieval becomes profile-aware**: pass the profile name +
      kind glossary into the select prompt.

## Phase 3 — cross-refs + semantic navigation

- [ ] **Cross-references**: citation graph for papers; recommendation
      → evidence for guidelines. Decide `section_refs` table vs.
      metadata edges.
- [ ] **Query API `kinds[]` filter**: "select only Recommendation
      sections" — structural filter, no LLM call.
- [ ] **Claim ↔ evidence links** surfaced in the map + query results.
- [ ] **`synthetic` profile** (optional): build a tree for
      structure-less documents (flat OCR) via recursive summarization —
      no embeddings.

## Open questions (carry from design doc)

- [ ] Default selection: declared-only / auto-only / both + threshold.
- [ ] Kind taxonomy: free per-profile strings vs. shared cross-domain
      core.
- [ ] Re-profiling existing docs when a better profile ships (ties to
      incremental re-ingest).
- [ ] Cross-ref storage: table vs. metadata.

## Related

- [../PROFILES.md](../PROFILES.md) — the design doc.
- [../ENGINE.md](../ENGINE.md) — the engine this builds on.
- [../../ROADMAP.md](../../ROADMAP.md) — root checkbox document.
