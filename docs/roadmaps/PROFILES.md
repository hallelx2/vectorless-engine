# Document Profiles roadmap

> Design doc: [../PROFILES.md](../PROFILES.md)

The *when* and *what's next* for domain-aware structuring. Profiles
turn the generic heading tree into a typed, navigable map for a
specialized domain (research papers, medical guidelines, …).

## Current status (summary)

| Phase | Status |
|---|---|
| Phase 0 — generic baseline (heading tree) | Shipped (the MVP) |
| Phase 1 — scaffold + `research-paper` + `medical-guideline` | Not started |
| Phase 2 — domain metadata extraction | Not started |
| Phase 3 — cross-refs + semantic query API | Not started |

The baseline (Phase 0) is what ships today: parsers → heading tree →
per-section summaries → tree retrieval, hardened for real-world PDFs.
Everything below is additive; the `generic` profile preserves current
behavior with zero regression.

**Decisions locked** (see [design doc](../PROFILES.md)): profile
selection is declared → auto → generic; kinds are per-profile over a
fixed `core_kind` set; figures/tables are metadata refs not nodes;
both first profiles ship in parallel.

> **Blocker for medical**: needs a *clean* guideline PDF fixture
> (a real GINA/NICE/WHO doc that isn't encrypted/watermarked). The
> research path can proceed immediately on the arXiv fixture.

## Phase 1 — scaffold + research-paper + medical-guideline (parallel)

One scaffold, two real profiles validated against their fixtures.

- [ ] **`pkg/profile` package**
  - [ ] `Profile` interface (`Name`, `Detect`, `Apply`).
  - [ ] `Registry` that routes a document to the best-fit profile
        (same pattern as `pkg/parser`); selection order declared →
        auto-detect (threshold ~0.6) → `generic`.
  - [ ] `generic` profile — no-op, `Kind=""`. The fallback.
  - [ ] `Kind` + `CoreKind` + `CrossRef` + `DocMeta` types. Fixed
        `core_kind` set: meta / body / reference / appendix /
        navigation.
- [ ] **`tree.Section`**: `Kind` + `CoreKind` fields; `View` exposes
      both.
- [ ] **DB**: `kind` + `core_kind` columns on `sections` (+ migration,
      indexed). Reads/writes carry them.
- [ ] **Ingest** runs the selected profile after `persistTree`
      (`parsing → structuring → summarizing → ready`). Idempotent.
- [ ] **API**: `X-Vectorless-Profile` honored on upload; `profile` +
      per-section `kind`/`core_kind` in document + tree responses.
- [ ] **`research-paper` profile**
  - [ ] `Detect` from outline (Abstract + References + IMRaD-ish
        headings; arXiv/DOI filename hints). No LLM.
  - [ ] Classify — rule pass + one batched LLM call for the residue,
        bounded by the kind vocabulary.
  - [ ] Kinds: Abstract, Contributions, RelatedWork, Method,
        Experiment, Result, Limitation, Conclusion, References,
        Appendix (→ core_kind mapping).
  - [ ] Validate on arXiv: ToC shows `[Method] §3 Model Architecture`.
- [ ] **`medical-guideline` profile**
  - [ ] `Detect` from recommendation/evidence-grade phrasing.
  - [ ] Kinds: Recommendation, Evidence, Population, Intervention,
        Dosage, Contraindication, Monitoring, References.
  - [ ] Validate against a clean guideline PDF *(fixture needed)*.

## Phase 2 — domain metadata extraction

- [ ] **Research metadata**: datasets, metrics, `figure_refs`,
      `table_refs`, `equation_refs` into section `metadata` (refs, not
      nodes — per decision).
- [ ] **Medical metadata**: `evidence_grade`, `condition`, `drug`,
      `population`, `strength_of_recommendation`.
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

- [ ] Auto-detect confidence threshold (start ~0.6, tune on real
      uploads).
- [ ] Re-profiling existing docs when a better profile ships (ties to
      incremental re-ingest).
- [ ] Cross-ref storage: table vs. metadata (Phase 3).

## Related

- [../PROFILES.md](../PROFILES.md) — the design doc.
- [../ENGINE.md](../ENGINE.md) — the engine this builds on.
- [../../ROADMAP.md](../../ROADMAP.md) — root checkbox document.
