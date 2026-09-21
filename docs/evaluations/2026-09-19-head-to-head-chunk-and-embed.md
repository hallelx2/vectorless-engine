# Vectorless on Jev against chunk-and-embed, on FinanceBench

**Date:** 2026-09-19
**Harness:** [vectorless-bench](https://github.com/hallelx2/vectorless-bench) `configs/financebench_jev.yaml` — the engine reached through the Python SDK, the baselines in-process
**Corpus:** FinanceBench, 19 filings with questions among the 21 downloaded (68–549 pages), 40 questions, k=5, two repeats
**Engine:** `cmd/engine --local`, `ingest.mode: toc`, `retrieval.strategy: judgewalk`, abstention off, caches off, Judge on, no generative call anywhere in retrieval
**Question:** the post claims chunk-and-embed cannot be this fast and this exact. Is that measured?

## Result

The final run completed all 40 questions and both repeats with no
errors and no engine restart. Every gold evidence page is among the
returned pages for 34 of 40 questions; hit@5 (any returned unit holds
the answer text) is 37 of 40; the answer text is in the first returned
unit for 30 of 40. The six page misses are the four `navbench` had
already found (three Boeing, one Pfizer) plus two where a gold page was
one page off the returned one (Pfizer 70–71 → 71; Verizon 23 and 56 →
23 and 57) — a page-boundary question for the full-page pass, not a
navigation one.

| system | how it retrieves | F1@5 | hit@5 | answer span in top-1 | p50 / query | $ / query | ingest, 19 filings | deterministic across repeats |
|---|---|---|---|---|---|---|---|---|
| **Vectorless, judgewalk on persisted pages, pages returned** (PR #68, final run 2026-09-21) | Jev ranks the TOC's sections, then page heads, then pages; the evidence pages are returned as-is, ahead of any section | 0.498 | **0.925** | **0.750** | 37 s (p95 75 s) | $0.0040 | 1,111 s (58 s / filing, sub-section splitting on) | 0.38 exact, 0.83 Jaccard |
| Vectorless, judgewalk on persisted pages, sections mapped by page range (PR #68 before HAL-1390) | same navigation; the parser's sections covering the evidence pages returned | 0.197 | 0.475 | 0.000 | 29 s | $0.0040 | — | — |
| Vectorless, judgewalk on the section tree (first pass) | same navigation over the parser's sections and their bodies | 0.453 | 0.650 | 0.575 | 50 s | $0.0067 | 1,080 s (57 s / filing) | 0.43 exact, 0.69 Jaccard |
| chunk-and-embed, BGE-small | 512-token chunks, bge-small-en-v1.5 on the CPU, cosine top-5 | 0.170 | 0.375 | 0.225 | 49 ms | $0 | 1,972 s (104 s / filing, 4 threads) | 1.00 |
| BM25 | 512-token chunks, BM25 top-5 | 0.096 | 0.200 | 0.075 | 48 ms | $0 | 2 s | 1.00 |

Scoring: a question's gold is FinanceBench's evidence text and answer; a
returned unit is a hit when it contains the gold span (numbers matched
as numbers). F1@5 is the harness's primary quality; "answer span in
top-1" is whether the first returned unit contains it.

## What the runs taught, in order

The server's judgewalk was navigating the **parser's section tree** over
section bodies. The Jev-built table of contents was persisted for
treewalk alone, and per-page text was never persisted. That scored
hit@5 0.65 where the same navigation over real pages had scored 0.90
in `navbench` — the parser's page attribution is exactly what HAL-1375
had shown to be unreliable. Ingest now persists the pages beside
`documents.toc_tree`, and judgewalk navigates the persisted TOC over
them (PR #68).

That alone made things **worse**: hit@5 0.475, the answer in the first
result never. The right pages were found and then mapped back to the
parser's sections covering them by page range — the same unreliable
attribution, one step later. Page-based retrieval now returns its
pages (HAL-1390): `/v1/query` leads with the evidence pages as units
of their own, `page` and `confidence` set. On the full 40, hit@5 went
0.475 → 0.925 and the answer in the first unit 0.000 → 0.750.

Three more things the run surfaced, each fixed on the way:

- `/v1/query` dropped retrieval's usage, so a client benchmarking
  retrieval alone saw $0 (PR #67).
- `ingest.mode: minimal` skips the TOC stage entirely; a `toc` mode now
  runs the page-based pipeline and nothing else (PR #67).
- The SDK rejected an abstained response for lacking a model name
  (vectorless-sdk #3). Abstention is off for this run so judgewalk's
  low-confidence best guess is scored rather than blanked.

## What the numbers say, and what they do not

- **Latency.** Chunk-and-embed answers a query in 50 ms; Vectorless on
  Jev takes tens of seconds, all of it Judge round-trips. The post's
  speed claim is about *ingest* and about the *generative* path it
  replaced, not about beating a cosine lookup — say so.
- **Ingest.** Embedding 19 filings with a small model on four CPU
  threads took 33 minutes; the Jev TOC stage took 18 minutes for the
  same filings with sub-section splitting on, for $0.11. With a GPU the
  embedding number collapses; with more Jev concurrency so does ours.
- **Exactness.** Even on the section tree, Vectorless put the answer's
  text in its first result 2.6× as often as BGE-small and 7.7× as often
  as BM25. That is the citation claim, and it is measured.
- **Determinism.** The baselines are exact across repeats; judgewalk is
  not (0.43 exact-match of returned sets), because Jev's probabilities
  near the threshold move between calls. A product wants this number
  reported, not hidden.
- **Not measured here:** answer quality from the retrieved context
  (HAL-1387), the tree-walk chat loop on the same questions, Gemini
  embeddings (free-tier quota), and anything on a GPU.

## Reproduce

```bash
# engine, local mode, Judge on, page-based pipeline only
VLE_INGEST_MODE=toc TYPESAFE_API_KEY=… ./engine --local -config config.yaml   # retrieval.strategy: judgewalk, abstain off, cache off
# bench
cd vectorless-bench && VECTORLESS_BASE_URL=http://localhost:7654 vlbench run --config configs/financebench_jev.yaml
```
