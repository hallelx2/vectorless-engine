# Retrieval navigation on a Judge — and the page bug it found

**Date:** 2026-09-18
**Harness:** [`cmd/navbench`](../../cmd/navbench/main.go) (evidence-page recall per question, no generative call), [`cmd/tocdump -judge-only`](../../cmd/tocdump/main.go), [`cmd/tocdump/coverage.py`](../../cmd/tocdump/coverage.py), [`cmd/tocdump/titles.py`](../../cmd/tocdump/titles.py)
**Corpus:** FinanceBench, 40 questions on the 21 downloaded 10-K filings, gold evidence pages from `PatronusAI/financebench`
**Issues:** HAL-1371 (navigation), HAL-1375 (per-page text), HAL-1374 (leaf granularity)
**Question:** `TreeWalkStrategy` navigates with up to eight chat-completion hops. Can a Judge navigate instead — rank the tree's leaves, then the pages — and land the gold evidence page, with no generative call?

## Result: 34 of 40 questions have every gold page in the evidence set, 37 of 40 have it inside the chosen section, at about four Jev requests, 40 pages read and $0.003 per question.

| run | change | questions | right section | every gold page found | pages read | requests | s / question | $ / question |
|---|---|---|---|---|---|---|---|---|
| 1 | leaves → full pages, 3 leaves, 40 pages | 38 | 32 (0.842) | 30 (0.789) | 35.9 | 2.9 | 8.9 | 0.0021 |
| 2 | + coarse pass over page heads, page dedup | 35 † | 29 (0.829) | 25 (0.714) | 36.0 | 3.7 | 22.5 | 0.0028 |
| 3 | + leaf prompt no longer rules out risk sections | 38 | 34 (0.895) | 29 (0.763) | 36.2 | 3.7 | 16.4 | 0.0028 |
| **5** | **+ real per-page text (HAL-1375), + follow cross-references** | **40** | **37 (0.925)** | **34 (0.850)** | 40.7 | 4.1 | 20.6 | 0.0029 |

† three Jev requests failed after four minutes of retries in run 2; those questions are excluded from its counts and its seconds are inflated by the retries. Run 4 (follow only, old pages) was stopped when the page bug was found.

Seconds per question are Jev API latency in the hour of the run — the
same 21 filings' whole TOC stage took 44 s each in one pass and 9.5 s in
the next — and are reported, not engineered around.

## The bug the near-misses found

Run 3's misses were one page off: gold 12 with evidence 11, gold 59
with 58, gold 57 with 56. Page text was being assembled from
*sections* — each section's whole content keyed under the page it
started on — so a section running from 58 into 59 put page 59's opening
under "page 58", and a page that started no section had no entry at
all. AMD_2022_10K had 104 page entries for a 121-page PDF; PEPSICO's
549 pages showed as 170.

Every page-level stage had been working on that: detection, resolution,
the coverage gate, and navigation. `ParsedDoc.Pages` is now built from
the parser's own rows, grouped by physical page. Re-running the whole
TOC stage on real pages:

| | section-start pages (jev2) | real pages (jev3) |
|---|---|---|
| filings with a tree | 20 / 21 | **21 / 21** (GENERALMILLS, the "one-page parse" of HAL-1365, was this bug) |
| gold evidence pages inside a leaf | 45 / 45 | **47 / 47** |
| median leaf span | 37 p | 37 p |
| leaf title recall / precision against jev2 | — | 0.975 / 0.966 |
| TOC stage per filing | 44 s | 9.5 s (Jev latency in the hour) |

## What each navigation change did

- **Leaf ranking, then full pages** (run 1): the shape works. One Noul
  per leaf, one per page of the best three leaves. 30 of 38.
- **Coarse pass over page heads** (run 2): meant to let a 70-page Item
  8 into the page budget. It did not move hits on its own and adds a
  request; kept because run 5 needs it to gather 120 pages from five
  leaves.
- **The leaf prompt** (run 3): the first version said "a risk-factors
  section holds neither" the figures nor the discussion. Three Boeing
  questions are answered in Risk Factors; the Judge ranked it last
  because it was told to. Right-section rate 0.842 → 0.895 once the
  instruction described what each kind of section holds without ruling
  any out. A prompt is a measured artifact; see the prompt-versioning
  proposal.
- **Real pages + follow** (run 5): the page fix turned the off-by-one
  misses into hits (0.763 → 0.850) and brought the two GENERALMILLS
  questions into the set. Following "see Note 21" fires only when the
  tree has a leaf to follow to; on 10-K trees with 20-odd leaves it
  rarely does, which is HAL-1374's point.

## The six that are still missed

| filing | gold | evidence | why |
|---|---|---|---|
| BOEING | 113 | 26, 27 | "see Note 21" — the tree has no note-level leaf to follow to (HAL-1374) |
| BOEING | 8, 10, 14 | 12, 14 | right section; two of three pages ranked below threshold |
| BOEING | 55 | 24, 77, 76, 79 | right section; page missed |
| KRAFTHEINZ | 50, 52 | 50, 124 | one of two gold pages |
| PFIZER | 59 | 57, 41 | right section; neighbouring page chosen |
| PFIZER | 70, 71 | 51, 9 | wrong section |

Four of six are page-level misses inside the right section — the
full-pass question, or how much of a page it sees, is the next thing
to measure. The other two want finer leaves.

## Method

- `JudgeNavigator.RankLeaves`: one request per 120 leaves; state is
  the question plus each leaf's title, path, page range and summary.
- Gather the pages of the best five leaves, up to 120, each page once.
- Over 40 pages: `rankPages` on 700-character heads, keep the best 40.
- `RankPages` on full pages (6,000 chars), batched so the request's
  whole state stays under 24k tokens — the provider treats the state
  object as shared context for every question. Evidence is every page
  at or above 0.5; never fewer than the best two.
- `referencedLeaves`: `Note N` / `Item N` mentions on evidence pages
  that name an unread leaf → its pages ranked in one more request.
- `JudgeWalkStrategy` is the same over a section tree, selectable as
  `strategy=judgewalk` whenever `llm.judge` is configured. It writes
  no answer: that is the caller's one generative call, over the
  evidence only.

## Reproduce

```bash
go run ./cmd/tocdump -docs ~/.cache/vlbench/financebench -out /tmp/trees -judge-only -minimal
go run ./cmd/navbench -questions ~/.cache/vlbench/financebench-questions.jsonl -trees /tmp/trees -pdfs ~/.cache/vlbench/financebench -out /tmp/nav.jsonl
```
