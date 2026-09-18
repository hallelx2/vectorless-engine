# TOC extraction on a Judge — the TOC stage with no generative model

**Date:** 2026-09-18
**Harness:** [`cmd/tocdump -judge-only`](../../cmd/tocdump/main.go) (any chat-completion call fails loudly), [`cmd/tocdump/titles.py`](../../cmd/tocdump/titles.py) (leaf-title recall against the GLM trees), [`cmd/tocdump/coverage.py`](../../cmd/tocdump/coverage.py) (evidence-page gate)
**Corpus:** FinanceBench, 21 10-K filings; gold evidence pages from `PatronusAI/financebench`
**Issue:** HAL-1370
**Question:** with detection and page resolution on Jev, extraction was the only generative call left in the TOC stage and all of its wall clock — 103–840 s per filing. Can code list the contents entries and Jev confirm them, with the same tree at the end?

## Result: yes. The whole TOC stage runs with no chat model, in one to two minutes of API time per filing, for under a tenth of a cent, and lands every gold evidence page.

| | GLM extraction + Jev resolver ([previous evaluation](2026-09-18-jev-page-resolver.md)) | **Jev only** |
|---|---|---|
| documents with a tree | 19 / 21 | **20 / 21** |
| generative calls per document | 1 | **0** |
| gold evidence pages inside a leaf | 44 / 44 | **45 / 45** |
| median leaf span | 36 p | 37 p |
| TOC stage per document | 103–840 s (extraction) + resolver | 12–143 s, mean 44 s, all of it Jev latency |
| cost per document | $0.017–0.095 | **$0.0003–0.0015** |
| requests per document | 3 | 3 |

NIKE_2019_10K, which GLM never extracted (past 900 s twice), has a
21-leaf tree. GENERALMILLS_2020_10K is still the one-page parse
(HAL-1365); it is the only filing without a tree.

## Are the trees the same trees?

Leaf titles of the Jev-built tree against the GLM-built tree, per
document, after normalising case and punctuation and allowing a prefix
match for wrapped titles:

| document | GLM leaves | Jev leaves | recall | precision |
|---|---|---|---|---|
| ADOBE, AMAZON, AMD, BESTBUY, COSTCO, NETFLIX, ORACLE, PEPSICO, WALMART | 20–24 | same | 1.000 | 1.000 |
| BOEING | 23 | 24 | 1.000 | 1.000 |
| LOCKHEEDMARTIN | 23 | 24 | 1.000 | 0.958 |
| JOHNSON_JOHNSON | 36 | 36 | 0.972 | 0.972 |
| KRAFTHEINZ | 63 | 65 | 0.968 | 0.954 |
| AMCOR | 29 | 30 | 0.966 | 0.933 |
| VERIZON | 24 | 23 | 0.958 | 1.000 |
| MGMRESORTS | 23 | 24 | 0.957 | 0.917 |
| 3M | 56 | 55 | 0.893 | 0.909 |
| PFIZER | 40 | 37 | 0.875 | 0.946 |
| INTEL | 26 | 25 | 0.846 | 0.880 |
| **overall** | 543 | 543 | **0.961** | **0.965** |

The GLM tree is the reference only because it existed first; where the
two differ it is not always GLM that is right. The misses on PFIZER are
unnumbered sub-headings GLM read out of the body (`Available
Information`, `Forward-Looking Information…`) that the contents page
does not list; the extra on LOCKHEEDMARTIN is a real entry. INTEL's
contents page lists `Fundamentals of Our Business` and similar headings
under a different layout that the parser splits into fewer entries; it
is the one document under 0.9 and worth a look on its own.

## What the parser had to learn

The first corpus pass scored 0.866 recall with WALMART at 0.045. Every
miss was a shape of contents text, not a Judge verdict:

| shape | filing | handling |
|---|---|---|
| no space between the item number and its title: `Item 1Business 7 Item 1ARisk Factors 14` | WALMART | re-insert the space; a number directly after a label is never a page |
| part label inline before its first item: `PART II Item 5.`, `PART I. Item 1.` | VERIZON, ORACLE, WALMART | the label becomes a container, the run restarts |
| a title wrapped so its page lands mid-title: `… Issuer Purchases of 20 Equity Securities Item 6.` | VERIZON | the words before the next label are the previous entry's tail |
| an item with no page at all: `Item 1. Business` alone on its line | VERIZON | an item-labelled run is an entry; the resolver finds its page |
| parenthesised sub-entries: `… SCHEDULES 111 15(a)(1) Financial Statements 111` | PFIZER | a digit opens an entry |
| the same line twice, once truncated in bold, from the parser's empty-leaf fold | 3M | exact duplicates dropped; a page-less prefix of a fuller entry dropped |
| a column label fused to the first entry: `Page Part I Item 1…` | WALMART | leading furniture skipped |

Second pass: recall 0.961, precision 0.965, WALMART 1.000.

## Method

- **Candidates from code.** `parseContentsEntries` tokenises each line
  of the detected contents pages; an entry closes at a 1–3 digit token
  followed by the end of the line or something that opens an entry (a
  label such as Item or Note, a number, a capitalised word). Structure
  comes from the numbering: Items under the enclosing Part, dotted
  numbers by depth.
- **One Judge request.** A `Noul` per entry — *is this a section a
  reader could turn to, rather than the table's own title, a running
  header, a column label, or a fragment?* — with the contents text
  shared once (~3k characters) and the entry as per-question state.
- **Pages from the resolver** (HAL-1367), which searches the body for
  each title. The printed page numbers are not trusted for placement;
  after resolution they calibrate an offset — the median of physical
  minus printed over the resolved leaves — that places whatever the
  resolver could not.
- **Fallbacks.** Fewer than three confirmed entries declines to the
  generative extractor unchanged. A failed Judge request also falls
  back, recorded on `Usage.Degraded`. `Usage.GenerativeCalls` makes a
  Judge-only build provable; `tocdump -judge-only` makes any generative
  call an error.

## What this means for the pipeline

Detection, extraction, resolution: three requests, no generative call,
roughly a cent per hundred filings. The wall clock is now the Judge
API's latency in the hour of the run (the same documents took 5–12 s an
hour earlier and 40–140 s in this pass), not the work. The remaining
generative steps in ingest are the per-section summaries, HyDE
questions and multi-axis summaries, none of which the page-based
retrieval strategy needs.

## Reproduce

```bash
go run ./cmd/tocdump -docs ~/.cache/vlbench/financebench -out /tmp/trees-jev -judge-only -minimal
python cmd/tocdump/titles.py ~/.cache/vlbench/trees-after /tmp/trees-jev
python cmd/tocdump/coverage.py ~/.cache/vlbench/trees-resolved /tmp/trees-jev
```
