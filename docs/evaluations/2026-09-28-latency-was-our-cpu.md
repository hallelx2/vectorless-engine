# The 50-second query was our own CPU

**Date:** 2026-09-28
**Harness:** [`cmd/navbench`](../../cmd/navbench/main.go), [`cmd/tocdump`](../../cmd/tocdump/main.go) with `coverage.py` and `titles.py`, llmgate `judge/typesafe` benchmarks
**Corpus:** FinanceBench, 21 filings with trees (the 19 with questions plus two), 40 questions, trees `trees-split-20c`
**Issues:** HAL-1708 (llmgate), HAL-1545 (ingest fan-out), HAL-1566 (retrieval skim), HAL-1563 (post-fix baseline)
**Supersedes:** every latency figure in [`2026-09-25-query-latency-and-the-embedding-prefilter.md`](2026-09-25-query-latency-and-the-embedding-prefilter.md) and the 37 s / 58 s figures in [`2026-09-25-judgement-not-generation.md`](2026-09-25-judgement-not-generation.md)

## Result

| | before | after | accuracy | cost |
|---|---|---|---|---|
| query, median | 50.5 s | **1.9 s** | 36/40 every gold page, unchanged | $0.00402 → **$0.00388** |
| ingest, median per filing | 66.1 s | **4.1 s** | 47/47 gold pages covered, unchanged | $0.133 → $0.133 for 21 filings |

Accuracy and cost did not move because the provider was never the bottleneck. Jev answered in about 0.6 s at the median throughout. The time went to our client preparing each request.

## 1. The guard rebuilt its tokenizer on every count

llmgate's TypeSafe client checks each request against the provider's context limits before sending it. It counted tokens with `tiktoken.GetEncoding("cl100k_base")`, and tiktoken-go v0.1.6 caches the rank table but builds a new BPE on every call: a 100k-entry decoder map and a regex compile. That cost 256 ms. The guard counted the state and then each question separately, so the 120-question section ranking spent around 30 s of CPU before it went on the wire. It did so inside the concurrency limiter's slot, where it looked like a slow provider.

This was the factor of three the 2026-09-25 evaluation could not account for (predicted ~9 s, measured 28.6 s). It hit ingest too: `batchByTokens` and the resolver count every page to pack batches.

The fix (llmgate PR #19):

- build the encoder once per process;
- decide on byte length first, because every cl100k token covers at least one byte — when the bytes fit, the tokens fit, and nothing is counted;
- past that bound, count the state once, cut into pieces the pre-tokenizer cannot span and counted in parallel. The count is exact, and a property test holds it equal to the serial count.

| llmgate benchmark | before | after |
|---|---|---|
| count one short question | 256 ms | 0.1 ms |
| 16-page request, end to end against a stub | 289 ms | 63 ms |
| 120-head skim request, end to end against a stub | 163 ms | 45 ms |

Same engine code, only llmgate swapped, first 12 questions, run back to back:

| llmgate | hit | requests/q | input tokens/q | $/q | median |
|---|---|---|---|---|---|
| v0.5.0 | 12/12 | 4.2 | 87,711 | 0.00368 | 50.5 s |
| 010e07c | 12/12 | 4.2 | 87,771 | 0.00369 | **2.2 s** |

Identical requests, tokens and cost. The 28.6 s quoted on 2026-09-25 came from the same bug on a less loaded machine. It should not be cited again.

Every Judge request now reports prepare time, connect, time to first byte and total (`typesafe.Config.OnRequest`). The engine warns when preparation passes 250 ms, and both benches print the split. Across the runs below, preparation was 1–35 ms at the median and the provider's first byte 450–640 ms.

## 2. Ingest sent its batches one at a time

Six Judge loops in the TOC stage — detection fan-out, detection, verification, contents confirmation, page resolution, heading split — built a batch, sent it, waited, then built the next. The batches were independent. They are now built first and sent together, and the leaf splitter handles every leaf of a generation at once. The 24k request budget stays; HAL-1545's proposal to cut it to 6k rested on a probe the earlier evaluation retracted.

All 21 filings, one at a time, Judge-only, minimal context:

| build | median / filing | p90 | max | total build | requests | cost | coverage |
|---|---|---|---|---|---|---|---|
| original | 66.1 s | 126.0 s | 456.5 s | 1,673 s | 288 | $0.1331 | 47/47 |
| + tokenizer fix | 8.8 s | 17.2 s | 49.4 s | 234 s | 287 | $0.1316 | 47/47 |
| + batch fan-out | **4.1 s** | **6.8 s** | **15.9 s** | **105 s** | 282 | $0.1330 | 47/47 |

The tokenizer fix is most of it. Fan-out halves what is left.

Title recall against the original build is 0.944 for the fan-out and 0.949 for the tokenizer fix alone. The tokenizer fix does not change which questions are asked, so 0.949 measures Jev's run-to-run variation in the trees, and the fan-out sits inside it. HAL-1545's acceptance of "≥ 0.96 against the current trees" cannot be met by re-running unchanged code and should be read against this floor.

## 3. Retrieval: one level removed, fewer pages read

With the guard fixed, a query is its four sequential levels at ~0.6 s each: rank sections, skim page heads, read the best pages, maybe follow a cross-reference. The skim waited on the ranking only to learn which pages to skim. Skimming every page's head in the same round as the ranking removes that level (HAL-1566). The full read is chosen from the same candidates by the same scores, and a unit test holds the two paths equal.

40 questions, repeated runs:

| mode | hit (every gold page) | median | requests/q | $/q |
|---|---|---|---|---|
| sequential skim, 40 pages | 36, 35, 36 | 2.5, 2.7, 3.1 s | 4.3 | 0.00402 |
| skim-all, 40 pages | 35, 36, 35 | 1.7, 1.7, 1.8 s | 4.8 | 0.00431 |
| skim-all, 30 pages | 36, 36, **36** | 1.8, 2.0, **1.9** s | 4.4 | **0.00388** |
| skim-all, 20 pages | 35, 36 | 1.9, 1.7 s | 3.9 | 0.00345 |

The questions that change between runs are the same two in every mode (Pfizer 0030, Verizon 0215). No mode differs in accuracy beyond that noise. Skimming every page costs ~7% at 40 pages. Reading 30 pages in full instead of 40 recovers that and more: full pages are ~60% of a query's tokens. The shipped default is skim-all at 30 pages. 20 matched on this corpus, but FinanceBench rarely spreads an answer across pages, and a smaller read would hurt those questions first.

Skim-all is on only in the persisted-pages path, where every page is already in memory, and only for documents up to 400 pages.

## What this does not claim

- The 1.9 s is `navbench`, which calls the navigator directly. It excludes the HTTP API, the database reads for the TOC and pages, and answer generation. `/v1/query` over the deployed engine has not been re-measured.
- Runs from 03:40 onward shared the machine with a headless Chromium, an Android emulator and the original-code ingest run. Load average peaked at 68 on 8 cores. Accuracy comparisons are unaffected. Absolute latencies from those runs are, if anything, pessimistic.
- The misses are the known ones — three Boeing, one Pfizer, one Verizon multi-page — and none was touched.
- Multi-hop is still unmeasured.

## Reproduce

```bash
# llmgate
go test -run XXX -bench . ./judge/typesafe/

# retrieval, shipped defaults
go run ./cmd/navbench -questions ~/.cache/vlbench/financebench-questions.jsonl \
  -trees ~/.cache/vlbench/trees-split-20c -pdfs ~/.cache/vlbench/financebench -skim-all

# ingest
go run ./cmd/tocdump -docs ~/.cache/vlbench/financebench -out /tmp/t -judge-only -minimal -parallel 1
uv run --project ../vectorless-bench python cmd/tocdump/coverage.py /tmp/t
```

Both benches end with a `judge requests` block separating local preparation from provider time. A latency figure quoted without it does not say whose latency it is.
