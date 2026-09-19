# Leaf granularity — splitting the sections that hold the evidence

**Date:** 2026-09-19
**Harness:** [`cmd/tocdump -split`](../../cmd/tocdump/main.go), [`cmd/tocdump/coverage.py`](../../cmd/tocdump/coverage.py), [`cmd/tocdump/titles.py`](../../cmd/tocdump/titles.py), [`cmd/navbench`](../../cmd/navbench/main.go)
**Corpus:** FinanceBench, 21 10-K filings, 47 gold evidence pages, 40 questions
**Issue:** HAL-1374
**Question:** a 10-K's tree is fine where nothing is (Items 1B–4, a paragraph each) and coarse where everything is (Item 8 and its notes, 70 pages under one title). Does splitting the big leaves at their own headings, on the Judge, make retrieval find more and cite tighter?

## Result: right section 39 → 40 of 40; the leaf holding a gold page shrinks from 37 pages to 6 at the median; pages found stays at 36 of 40. Splitting defaults on at 20 pages.

| tree | leaves / filing | span of the leaf holding a gold page, median | gold pages inside a leaf | right section | every gold page found | requests / question | $ / question |
|---|---|---|---|---|---|---|---|
| unsplit (contents-page grain) | 23 | **37 p** | 47 / 47 | 39 / 40 | 36 / 40 | 4.2 | 0.0035 |
| split over 8 | 69 | 1 p | 47 / 47 | **40 / 40** | 36 / 40 | 4.3 | 0.0041 |
| split over 12 | 67 | 1 p | 47 / 47 | **40 / 40** | 35 / 40 | 4.4 | 0.0041 |
| split over 20 | 68 | 1 p | 47 / 47 | **40 / 40** | 36 / 40 | 4.3 | 0.0040 |
| split over 12, tightened budget | 38 | 7 p | **39 / 47** | 33 / 40 | 30 / 40 | 4.2 | 0.0037 |
| **split over 20, final** | **54** | **6 p** | **47 / 47** | **40 / 40** | **36 / 40** | 4.3 | 0.0039 |

All navigation rows use the navigator after the fix described below.
The TOC stage pays for the split: 271 Judge requests over the corpus
against 63, about $0.006 per filing against $0.0007, and 489 s wall at
parallel 8 against 122 s. Leaf titles against the unsplit tree: recall
0.971 (17 of 589 lost to sub-leaf re-titling), precision 0.542 — every
sub-leaf is an "extra" by construction.

## What the sweep taught, in order

1. **The first pass made navigation worse, and the tree was not at
   fault.** At T=12 the right-section rate fell 37 → 33 and pages read
   fell 40 → 16. The navigator took a fixed five sections; on a tree of
   80 one-page notes that was five pages. Sections are now taken in
   rank order until the page budget is gathered. Same trees: right
   section 33 → 40, evidence 31 → 35. (The bench's own `-leaves 3`
   default masked the fix for one run.)
2. **Every threshold produced about the same leaf count: 69, 67 and 68
   per filing (medians) at 8, 12 and 20.** The per-page cap, not the
   content, was deciding, and it kept the earliest headings rather than
   the best. Sub-leaves are now kept by
   the Judge's probability.
3. **Tightening the budget to one sub-leaf per half-threshold lost
   coverage: 47 → 39 of 47.** Not because headings were dropped — because
   sub-leaves began at the first confirmed heading, so the pages before
   it (Item 8's index and the auditor's report) belonged to no leaf. A
   gold page there was inside nothing. The section's opening is now its
   first sub-leaf, titled as the parent. The loose budget was restored.
4. **8, 12 and 20 navigate alike.** 20 makes the fewest sub-leaves and
   costs the least, so it is the default whenever a Judge is set.

## Where the sub-leaves come from

Two sources, in order of trust, both "select, don't generate":

- **A nested contents page** inside the leaf — Item 8 opens with an
  index of statements and notes with page numbers. Parsed, confirmed and
  resolved exactly as the document's own contents page is.
- **Heading-shaped lines** — short, line-opening, Title Case or capitals,
  not repeated on three or more pages (a running header is not a
  heading). Each is judged with the lines that follow it: *is this a
  heading a reader could turn to, rather than page furniture?* Boeing's
  Item 7 became fifteen sub-sections from "Consolidated Results of
  Operations" to "Contingent Obligations"; its Item 8 became the six
  statements and twenty-one notes.

## What is still missed

Four questions, the same four as before the split: Boeing's legal
proceedings (the "see Note 21" hop now has a Note 21 leaf to reach, and
still misses the page inside it), and three page-level misses inside
the right section. Section choice is solved on this corpus; the
full-page question, or what it is shown, is next.

## Reproduce

```bash
go run ./cmd/tocdump -docs ~/.cache/vlbench/financebench -out /tmp/t -judge-only -minimal -parallel 8          # split on by default
go run ./cmd/tocdump -docs ~/.cache/vlbench/financebench -out /tmp/t0 -judge-only -minimal -parallel 8 -split -1  # off
python cmd/tocdump/coverage.py /tmp/t0 /tmp/t
go run ./cmd/navbench -questions ~/.cache/vlbench/financebench-questions.jsonl -trees /tmp/t -pdfs ~/.cache/vlbench/financebench -parallel 4
```
