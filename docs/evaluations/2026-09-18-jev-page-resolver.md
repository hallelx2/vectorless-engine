# Page resolution on a Judge — select, don't generate

**Date:** 2026-09-18
**Harness:** [`cmd/tocresolve`](../../cmd/tocresolve/main.go) (resolver alone, over a tree `tocdump` extracted), [`cmd/tocdump/coverage.py`](../../cmd/tocdump/coverage.py) (evidence-page gate against FinanceBench)
**Corpus:** FinanceBench, the 21 10-K filings the harness downloads; gold evidence pages from `PatronusAI/financebench`
**Issues:** HAL-1367 (every 10-K leaf loses its page), HAL-1366 (minimum context)
**Question:** extraction returns correct titles and no pages on every long filing, because the generative call sees 48k of a 500k-character body. Can code find the candidates and a Judge pick the page, in one or two requests, without widening that window?

## Result: yes. Every gold evidence page in 19 filings lands inside a leaf, median span 36 pages against 183, at two requests and under a cent per document.

Resolver only, applied to the tree the baseline produced, page 0 → resolved:

| document | pages | leaves with a page | requests | tokens | elapsed | cost |
|---|---|---|---|---|---|---|
| ADOBE_2022_10K | 88 | 0 → **24 / 24** | 2 | 10,382 | 5–11 s | $0.00045 |
| AMAZON_2019_10K | 68 | 2 → **22 / 22** | 2 | 10,013 | 9–11 s | $0.00043 |
| AMCOR_2020_10K | 135 | 3 → **28 / 29** | 2 | 12,507 | 6–14 s | $0.00053 |
| BOEING_2022_10K | 153 | 1 → **23 / 23** | 2 | 10,731 | 11–12 s | $0.00045 |

Every ADOBE page agrees with the filing's own contents page. The one AMCOR
miss, `Exhibit Index`, is a heading the parser never emits (HAL-1368);
there is nothing on any page to find.

For scale: the baseline's extraction call that produced these trees took
106–271 s per document on GLM and $0.017–0.030, and left the pages at
zero. Elapsed here includes Jev contents-page detection (one request) and
resolution (one request); the spread is the API, not the document.

## The evidence-page gate, 19 of 21 filings

`coverage.py` asks the question that matters for retrieval: is each gold
evidence page inside some leaf, and how big is that leaf? A
whole-document leaf covers everything and locates nothing, so both
columns count.

The honest baseline is the eleven trees the original binary produced,
whose leaves had no pages. Eight more trees came from a 900-second re-run
with the new binary, whose `Build` already resolves pages, so they are
"after" on both sides and are excluded from the before row.

| run | docs | gold pages covered | coverage | median leaf span | build time | requests | cost |
|---|---|---|---|---|---|---|---|
| baseline trees, GLM extraction, no resolver | 11 | 16 / 20 | 0.800 | **183 p** | 1,840 s | 32 | $0.246 |
| same trees, resolved on Jev | 11 | **20 / 20** | **1.000** | **36 p** | 699 s | 20 | $0.0046 |
| all 19 usable trees, resolved | 19 | **44 / 44** | **1.000** | **36 p** | 828 s | 36 | $0.0089 |

Leaves with a page across the 19: **238 → 496 of 543**. Of the 238
before, 225 belong to the eight re-run trees; the eleven old-binary trees
had 13 between them.

The baseline's 0.800 is not location: with no leaf pages,
`deriveEndPages` gives the first leaf of each part the whole part, so a
gold page is "covered" by a 183-page leaf. Resolved, the tightest leaf
holding a gold page is 36 pages at the median, and every gold page in
the set is inside one.

The resolver's build time is Jev API latency in this window — two
requests per document, a few of them retried — not work: the same
documents ran in 5–12 s an hour earlier. Cost is the number to read.

Per document, leaves with a page before → after:

| document | leaves | before → after | note |
|---|---|---|---|
| ADOBE_2022_10K | 24 | 0 → 24 | |
| AMAZON_2019_10K | 22 | 2 → 22 | |
| AMCOR_2020_10K | 29 | 3 → 28 | `Exhibit Index` not in text (HAL-1368) |
| BOEING_2022_10K | 23 | 1 → 23 | |
| COSTCO_2021_10K | 22 | 0 → 22 | |
| INTEL_2021_10K | 26 | 3 → 22 | |
| LOCKHEEDMARTIN_2021_10K | 23 | 1 → 23 | |
| MGMRESORTS_2018_10K | 23 | 0 → 22 | |
| NETFLIX_2017_10K | 20 | 1 → 20 | |
| PEPSICO_2021_10K | 21 | 1 → 21 | detection request failed once; resolved with no exclusions |
| WALMART_2020_10K | 22 | 1 → 22 | |
| 3M_2018_10K | 56 | 39 → 39 | re-run tree, already resolved in Build |
| AMD_2022_10K | 23 | 22 → 22 | re-run tree |
| BESTBUY_2017_10K | 24 | 24 → 24 | re-run tree |
| JOHNSON_JOHNSON_2022_10K | 36 | 30 → 30 | re-run tree |
| KRAFTHEINZ_2019_10K | 63 | 55 → 55 | re-run tree |
| ORACLE_2021_10K | 22 | 22 → 22 | re-run tree |
| PFIZER_2021_10K | 40 | 33 → 33 | re-run tree, extraction 840 s |
| VERIZON_2022_10K | 24 | 0 → 22 | re-run tree; Build's own resolver left 0, `tocresolve` placed 22 — see below |

Not in the set: GENERALMILLS_2020_10K (parser returns one page,
HAL-1365) and NIKE_2019_10K (GLM extraction exceeded 900 s twice). Both
are outside the resolver. Extraction is the one generative call left in
this phase and took 103–840 s per document against the resolver's two
requests.

VERIZON is worth a look: the re-run's `Build` ran the resolver and left
every leaf at 0, while `tocresolve` on the same tree placed 22 of 24
minutes later. The likely cause is a failed Judge request inside Build
(logged, falls back to the generative verifier, which rejects the
printed page numbers) — the same silent-degradation shape as HAL-1364.

The 47 leaves still unplaced on 3M, Johnson & Johnson, Kraft Heinz,
Pfizer and Intel are the next thing to look at; they are deeper
note-level titles and have not been examined yet.

## What it took, in the order each step recovered pages

Each row is one change, measured on ADOBE unless stated. The Judge was
never the limiter: with one exception at the end, every miss was a leaf
whose candidate set was empty because Go could not find the heading.

| step | ADOBE | what was wrong |
|---|---|---|
| page-head window, "begins at the very start" | 12 / 24 | sections that share a page are never at the start of it |
| heuristic contents-page exclusion | 11 / 24 | excluded 30 of 88 pages: "Table of Contents" is a running header on every page. Violates the zero-signal rule; reverted. Exclusion now comes only from what detection *calls* a contents page — [2] |
| "heading on this page", exclusion from detection | 17 / 24 | second-on-page sections (Items 2, 9A, 11–14) recovered |
| **search the whole page for a line-opening hit; show a 600-char window around it** | 20 / 24 | third-and-later sections (Items 3, 4, 9B, 9C) sit past any head-sized window |
| **a part label may precede the heading on its line** | **24 / 24** | the parser joins `PART I` to `ITEM 1. BUSINESS`; every first-in-part section was invisible |
| parser: leaf-cap merge keeps the absorbed title (c6a03b6) | AMCOR 11 → 23, BOEING 15 → 21 | `Item 1B` (body: "None.") + `Item 2. Properties` are the smallest adjacent pair in every 10-K, so `Item 2` was deleted from the text of every filing, and on AMCOR Items 3–5 merged in after it |
| stopwords and punctuation between title words | BOEING +1 | `Exhibits, Financial` in the contents is `Exhibits and Financial` on the page |
| Title-Case typographic tier, short label prefix, only when no line-opening hit exists | AMCOR +4 | `Amcor plc and Subsidiaries Consolidated Balance Sheet (in millions)`; a lowercase `see the consolidated balance sheet` never qualifies |
| loose form for numbered titles: label + number + first word, additive | AMCOR +1 | `Market For Registrant's Common Equity` is `Market for Registrant's Equity` on its page |
| contents-page fallback when exclusion leaves a leaf nothing | BOEING +1 | Item 1 opens on the contents page itself |
| excerpt starts on the hit's own line | (the above) | 150 characters of the previous lines were the contents list; the Judge read the heading as one more entry and said no |

Two `deriveEndPages` corrections rode along, both visible only once
leaves had pages: a container inherits its first child's start (Part II
with no page of its own left Items 1B–4 running to page 99), and a section
never ends before its own page (Item 9B and Item 10 share page 73 across
the Part II / III boundary; 9B's end came out 72 and was cleared to 0).

## What did not work, kept for the record

- **Speculative fan-out** (HAL-1366): asking both detection questions of
  every page in one request was 31% *slower*. Speculation pays only when
  it removes a request.
- **Head-of-page candidates**: 17 / 24 at best. The head is where the
  *first* section of a page is; a 10-K puts four sections on page 34.
- **Any fallback to "the title appears somewhere in the head"**: admitted
  every `see Item 1A` cross-reference as a candidate, one question each,
  and lost nothing when removed.

## Method

- Candidates come from code (`findCandidatePages`): a regexp over the
  title's content words, stopwords and punctuation tolerated between
  them, matched on every page. A hit is a heading if it opens its line
  (a part label or markdown emphasis before it is allowed). Only when
  no line-opening hit exists anywhere is a Title-Cased hit with at most
  five words before it on the line admitted. Pages detection called a
  table of contents are excluded — unless that leaves the leaf nothing.
- Each (leaf, candidate) is one `Noul` on a window from the hit's line
  through 450 characters after, batched under the 24k shared-state
  ceiling. The best probability above `JudgeThreshold` (default 0.5)
  wins; a leaf with no candidate above it keeps whatever extraction
  said, since an absent verdict is not a "no".
- More than four candidates for one title means a running header, not a
  section; the claimed page is kept if there is one.

## Reproduce

```bash
# one document, resolver only, candidate sets printed
go run ./cmd/tocresolve -v -pdf ~/.cache/vlbench/financebench/ADOBE_2022_10K.pdf \
  -tree ~/.cache/vlbench/trees-after/ADOBE_2022_10K.json -out ~/.cache/vlbench/trees-resolved

# what the pipeline actually searched
go run ./cmd/pagedump ~/.cache/vlbench/financebench/ADOBE_2022_10K.pdf | grep -n -i "item 2"

# the gate
python cmd/tocdump/coverage.py ~/.cache/vlbench/trees-after ~/.cache/vlbench/trees-resolved
```
