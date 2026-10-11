# Where the 37 seconds goes, and why a cheap pre-filter cannot take it

**Date:** 2026-09-25
**Harness:** direct probes against the provider (`scratchpad/latency_shape.py`), offline recall over cached page text (`page_recall.py`, `within_section.py`), [`cmd/navbench`](../../cmd/navbench/main.go)
**Corpus:** FinanceBench, 19 filings with questions (2,936 pages), 40 questions
**Issues:** HAL-1371, HAL-1542
**Question:** judgewalk answers in 37 s at the median. Retrieval systems people compare us to answer in 49 ms. How much of the gap is recoverable, and by what?

## Result: most of it is request shape, not work. A local pre-filter cannot help at any scale; batching can.

## 1. Request shape barely matters. Two of my own bugs did.

**First probe, and it was wrong.** Varying pages per request moved two
things at once — the text sent and the number of questions asked — and
the session's first calls were cold. It appeared to show large requests
being punished (17k tokens → 14.3 s). Acting on it, the page-ranking
budget was cut 24k → 6k, and a navigation run came back three times
slower.

**Second probe, one variable at a time, warm.** Holding state near 6k
while varying question count, then holding questions at 4 while varying
text, both curves are flat: everything lands between 1.4 s and 2.6 s.
The first two calls of a session take 5-6 s and then it settles. Forty
pages as ten parallel requests took 2.6 s wall; the same forty as
sixteen-page requests took 2.6 s each. Shape is not the lever.

**What was the lever, measured end to end on the same 12 questions:**

| build | requests / question | median s | hit@5 |
|---|---|---|---|
| baseline (24k budget, tokenised packing) | 4.3 | 37.0 | 12/12 |
| length-estimated packing, len/2 | 6.6 | 35.1 | 12/12 |
| **len/4 packing + limiter starting at 16** | **4.2** | **28.6** | 12/12 |

Two bugs, both ours:

- **Packing ran the tokenizer.** Deciding which pages go in which
  request tokenised all forty pages first — 4.0 s of CPU before a
  single call went out, plus the client tokenising the assembled state
  again per request. Packing only needs a safe upper bound, so it now
  estimates from length. The first attempt used len/2, which
  over-estimates real filing text by two and a half times (measured:
  4.9 characters per token), halved every batch, and sent 6.6 requests
  where 4.3 had done — cancelling the saving exactly. len/4 keeps a
  fifth of headroom and restores the batch size.
- **The limiter was starting at 4 and being halved by transient
  failures.** AIMD needs twenty consecutive successes to widen by one,
  which a single interactive query never lives long enough to earn; one
  failed request left the run at concurrency 2 for most of its length.
  A query's four to seven requests are independent, so navbench now
  starts at 16 and the run stayed at 16-18 throughout.

**What is still unexplained.** At 4.2 requests and ~21k tokens each,
warm rates predict roughly 3 s per request and three sequential phases,
so about 9 s. The engine takes 28.6 s. That factor of three is not
accounted for by anything measured here — candidates are provider
behaviour under sustained load versus a short probe, and retry backoff
after transient failures. It should be instrumented per request before
anyone optimises further, rather than guessed at a third time.

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

- **Our own overhead** (done): tokenised packing and a limiter that
  started narrow, together 37 s → 28.6 s on the same questions.
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
