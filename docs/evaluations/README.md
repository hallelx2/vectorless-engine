# Evaluations

Each report states its date, harness, corpus, the question it set out to answer, and the result. Later reports correct earlier ones; where that happens, the earlier report says so at the top. Read the newest report on a topic first.

Most runs use FinanceBench: 10-K filings from `PatronusAI/financebench`, with the gold evidence pages that dataset provides.

| Date | Report | Result in one line |
|---|---|---|
| 2026-09-18 | [Page resolution on a Judge](2026-09-18-jev-page-resolver.md) | Code finds candidate pages and Jev picks one: on 11 filings, gold pages covered went from 16/20 to 20/20, the median leaf holding one shrank from 183 to 36 pages, and cost fell from $0.246 to $0.0046. |
| 2026-09-18 | [Retrieval navigation on a Judge](2026-09-18-retrieval-navigation-on-a-judge.md) | Jev ranks sections, then pages, with no generative call: 37/40 right section, 34/40 every gold page, $0.0029 per question. Also found and fixed per-page text being built from sections. |
| 2026-09-18 | [TOC extraction on a Judge](2026-09-18-toc-extraction-on-a-judge.md) | The whole contents stage runs with no generative model: 12–143 s and $0.0003–0.0015 per filing, against 103–840 s and $0.017–0.095, with title recall 0.961 against the generative trees. |
| 2026-09-19 | [Leaf granularity](2026-09-19-leaf-granularity.md) | Splitting sections over 20 pages at their own headings: 40/40 right section, 36/40 every gold page, median leaf holding a gold page 6 pages instead of 37, $0.0039 per question. |
| 2026-09-19 (final run 2026-09-21) | [Head-to-head against chunk-and-embed](2026-09-19-head-to-head-chunk-and-embed.md) | At k=5, hit@5 0.925 against 0.375 for BGE-small embeddings and 0.200 for BM25; F1@5 0.498 / 0.170 / 0.096; answer in the top-1 unit 0.750 / 0.225 / 0.075. Not deterministic: 0.38 exact-set agreement across repeats. |
| 2026-09-25 | [Query latency and the embedding pre-filter](2026-09-25-query-latency-and-the-embedding-prefilter.md) | An embedding pre-filter cannot help: at k=50, BGE finds every gold page for 0.875 of questions, below judgewalk's 0.925 at k=5. Its latency figures are superseded by the 2026-09-28 report. |
| 2026-09-25 | [Judgement, not generation](2026-09-25-judgement-not-generation.md) | Architecture note, no new measurement: any decision that can be written as a closed set of options goes to Jev; generation is kept for the final answer. Names what was given up (deliberation; multi-hop is unmeasured). |
| 2026-09-28 | [The 50-second query was our own CPU](2026-09-28-latency-was-our-cpu.md) | llmgate's guard rebuilt its tokenizer on every count. Fixed: median query 50.5 s to 1.9 s (navigator called directly) and median ingest 66.1 s to 4.1 s per filing, with accuracy unchanged (36/40, 47/47). |
| 2026-10-08 | [PageIndex vs Vectorless latency](2026-10-08-pageindex-latency.md) | One filing (3M 2018 10-K), 6 questions × 3 repeats: retrieval median 1.70 s vs 7.05 s, full answer 5.27 s vs 10.51 s, ingest 10.5–19.2 s vs 204–210 s. With the same generative model navigating, the gap narrows to 7.6 s vs 10.5 s and PageIndex wins 2 of 6. |

## Reading these numbers

- Retrieval figures (right section, every gold page, hit@5) measure whether the evidence was found, not whether an answer was right.
- The 1.9 s and 4.1 s figures come from benchmark tools that call the navigator and ingest directly. The production API answered in about 3.5–6 s.
- Jev is not deterministic, so repeated runs can return different page sets.
- Multi-hop questions have not been measured.
