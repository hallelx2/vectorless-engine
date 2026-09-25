# Where the 37 seconds goes, and why a cheap pre-filter cannot take it

**Date:** 2026-09-25
**Harness:** direct probes against the provider (`scratchpad/latency_shape.py`), offline recall over cached page text (`page_recall.py`, `within_section.py`), [`cmd/navbench`](../../cmd/navbench/main.go)
**Corpus:** FinanceBench, 19 filings with questions (2,936 pages), 40 questions
**Issues:** HAL-1371, HAL-1542
**Question:** judgewalk answers in 37 s at the median. Retrieval systems people compare us to answer in 49 ms. How much of the gap is recoverable, and by what?

## Result: most of it is request shape, not work. A local pre-filter cannot help at any scale; batching can.

## 1. The provider prefers many small requests to few large ones

One `Noul` per page, real filing pages as state, three repetitions:

| state tokens | median latency | s per 1k tokens |
|---|---|---|
| 1,589 | 4.2 s | 2.67 |
| 2,724 | 3.6 s | 1.32 |
| 4,919 | 3.8 s | 0.78 |
| 9,253 | 5.9 s | 0.64 |
| 17,159 | 14.3 s | 0.84 |

Latency is flat to about 5k tokens — fixed per-request overhead — then
grows faster than the text does. And concurrency is close to free:

| shape | total work | wall clock |
|---|---|---|
| one request, 16 pages | 17k tokens | 8.3 s |
| 4 parallel × 4 pages | 20k tokens | **3.0 s** |
| 8 parallel × 8 pages | 74k tokens | **4.1 s** |

Four times the total work of a single 17k-token request, in half its
wall clock.

`judgewalk` was packing each page-ranking request to 24,000 tokens —
a number chosen to respect the provider's 32k per-question ceiling,
never for speed — which put every request in the penalty region. The
budget is now 6k (`defaultNavReqTokens`), which keeps each request in
the flat region and leaves the count to the adaptive limiter. The
batches already fan out concurrently.

## 2. A local embedding cannot pre-narrow the document

The appeal is obvious: if BGE-small could shortlist 30 pages in
milliseconds, Jev would judge 30 instead of reading 40, and two round
trips would collapse into one. Measured offline over all 2,936 pages,
"every gold page inside the top k":

| k | BGE-small | BM25 |
|---|---|---|
| 5 | 0.500 | 0.075 |
| 10 | 0.675 | 0.150 |
| 20 | 0.775 | 0.225 |
| 30 | 0.800 | 0.250 |
| 50 | 0.875 | 0.300 |

BGE's ceiling at k=50 is 0.875, below the 0.925 judgewalk already
reaches. A pre-filter that caps the pipeline below where it sits is
not a pre-filter, it is a downgrade.

**Nor inside the sections the tree picked.** Restricting the ranking to
the sections judgewalk chose (median 120 pages, and they contain every
gold page 40/40):

| k | any gold page in top k | every gold page in top k |
|---|---|---|
| 10 | 0.750 | 0.700 |
| 20 | 0.825 | 0.775 |
| 40 | 0.950 | **0.875** |

Picking 40 of those 120 pages by embedding similarity scores 0.875;
Jev reading the heads of the same 120 and picking 40 scores ~0.90+.
The Judge's skim beats the embedding at the same budget.

So the engine's own two-stage narrowing — a structural prior from the
tree, then a read — is better than semantic similarity at every scale
we can measure. That is the thesis, measured from the other side.

## 3. What is left of the gap

Three sequential round trips are inherent to the design: rank sections,
skim, read. Everything else is recoverable:

- **Request shape** (done): the page pass was three ~22k requests at
  ~14 s; at 6k it is ~10 requests in the flat region, in flight
  together.
- **A needless chain link** (not yet done): the skim only depends on the
  ranking because we skim *the chosen sections*. Skim every page instead
  and the two requests fire together — one fewer round trip, and more
  pages scored, not fewer.
- **Moving the skim to ingest** (HAL-1542): a per-page index built once
  makes the skim a lookup over stored answers.

None of that reaches 49 ms, and it should not be claimed. A system that
answers in 49 ms does no model work at query time, which is exactly the
compression that costs it 0.375 against our 0.925. The honest target is
single-digit seconds with the accuracy intact.

## Reproduce

```bash
python scratchpad/latency_shape.py      # needs TYPESAFE_API_KEY; ~30 requests, about a cent
python scratchpad/page_recall.py        # offline, cached page text, no API calls
python scratchpad/within_section.py     # offline
```
