# Judgement, not generation: what Vectorless became when every stage moved to Jev

**Date:** 2026-09-25
**Status:** architecture note, not a new measurement — it reads the evaluations already in this repo and in `vectorless-bench` and states the principle they support
**Sources:** [`2026-09-19-head-to-head-chunk-and-embed.md`](2026-09-19-head-to-head-chunk-and-embed.md) · [`2026-09-18-retrieval-navigation-on-a-judge.md`](2026-09-18-retrieval-navigation-on-a-judge.md) · [`2026-09-18-toc-extraction-on-a-judge.md`](2026-09-18-toc-extraction-on-a-judge.md) · [`2026-09-25-query-latency-and-the-embedding-prefilter.md`](2026-09-25-query-latency-and-the-embedding-prefilter.md) · `vectorless-bench` run `20260921_184919`

> **Latency superseded 2026-09-28.** The 37 s and 58 s figures below were measured while llmgate's guard rebuilt its tokenizer on every count. See [`2026-09-28-latency-was-our-cpu.md`](2026-09-28-latency-was-our-cpu.md) for the current figures. The quality figures and the argument are unaffected.

## The question this answers

Retrieval now makes no generative call. Jev ranks the TOC's sections, then page
heads, then pages, and returns evidence pages verbatim. A reasonable objection
follows: generation is what made the LLM smart, so a retriever with no
generation is a heuristic, and a heuristic ranking passages is what vector RAG
already was.

The objection is wrong, and the reason it is wrong is worth writing down,
because it decides what gets built next.

## Generation was never the mechanism

"Does this section plausibly contain the answer to this question?" is a
judgement. It was a judgement when a chat model answered it in eight hundred
tokens of prose, and it is the same judgement now that Jev answers it in one
letter. The prose was the medium the decision arrived in. It was never the
decision.

What separates this from vector RAG is not generation. It is **conditioning**.

- **Vector RAG** computes its geometry at ingest, before the question exists.
  A chunk's embedding is fixed; at query time two points are compared, each
  computed in ignorance of the other's purpose. Nothing reads. Distance is
  measured.
- **Jev** receives the question and the candidate together, at query time, and
  decides. Different question, different answer, same document. That is not a
  heuristic — a heuristic is question-independent by construction, which is
  precisely what makes it one.

The gap is measured, not argued. On FinanceBench (19 filings, 40 questions,
k=5):

| system | F1@5 | hit@5 | answer span in top-1 | p50 | $/query |
|---|---|---|---|---|---|
| Vectorless, judgewalk on Jev | 0.498 | 0.925 | **0.750** | 37 s | $0.0040 |
| chunk-and-embed, BGE-small | 0.170 | 0.375 | 0.225 | 49 ms | $0 |
| BM25 | 0.096 | 0.200 | 0.075 | 48 ms | $0 |

Answer text in the first returned unit, **3.3x** BGE-small and **10x** BM25.
Two processes that were the same kind of thing would not separate like that.

And the embedding cannot close it with more candidates. BGE-small puts every
gold page in its **top 50** for 0.875 of questions — below judgewalk's 0.925 at
k=5. Restricted to the sections the tree already chose it still reaches only
0.875 at k=40, where the Judge's head-skim over the same 120 candidates reaches
~0.90+. Similarity computed before the question exists does not become a
judgement conditioned on it at any k.

## The rule

> **Can the decision be written as a closed set of options?**
>
> **Yes** → Jev. Always. Cheaper, faster, and on this corpus more accurate.
> **No** → generation is required, and the first move is to suspect the question
> is badly framed. Most decisions that look open-ended decompose into closed
> ones.

This is the general form of the standing "select, don't generate" direction, and
it applies equally to two things that look different and are the same mistake:

- **LLM-as-judge.** A model optimised to produce plausible text, asked to make a
  decision, will produce a plausible-sounding decision. The output is shaped by
  the prompt and by its own sampling, and neither is the document.
- **Embedding similarity.** A decision made before the question arrived.

Both replace a judgement about *this question* with something else that
correlates with it.

## What was actually given up

Not intelligence. **Deliberation.**

Jev decides one node at a time, locally and greedily. It cannot form a plan,
backtrack, notice that an answer requires §4 combined with §11, or observe that
a question is ambiguous and both branches deserve a look. A chat model walking
the tree could do all of that — slowly, expensively, and, on structure, worse
(see below) — but it could.

That loss is **invisible in every number above**, because FinanceBench is
overwhelmingly single-evidence: find the page, quote it. The six known misses
are three Boeing, one Pfizer, and two off-by-one page boundaries; none is a
deliberation failure. So the capability that was traded away is currently
unmeasured. That is the honest gap in this architecture and it is filed.

## Where deliberation goes — and the shape it must not take

The tempting move is to put a generative pass back at ingest: let a chat model
read whole documents off the critical path and write titles and summaries into
the tree, so the query path stays fast.

**That is what was removed, and removing it is why ingest works.** Ingest went
from minutes and 2–9¢ per filing to **58 s and $0.006**, and the Jev-built tree
matched or beat the generative one: title recall 0.961 against 0.965, page
placement **47/47** where the generative path lost every page on long filings.
The generative path was not the more careful reviewer. On structure it was the
weaker judge.

So deliberation is precomputed as **more typed judgements**, never as prose:
richer per-page Jev questions — page kind, carries-figures, owning item — filed
as HAL-1542. The tree accumulates facets that Jev can condition on at query
time, and the system contains no generation anywhere except the final answer.

This also makes the citation guarantee **structural rather than procedural**. If
nothing generates prose during ingest or retrieval, no generated prose can reach
a citation. Nobody has to remember the rule, because the architecture cannot
express the failure. Human-readable summaries, if wanted, are a product feature
rendered for people reading the UI — outside the retrieval path, outside
citations, and labelled as such.

## Latency is levels, not calls

A related error is worth recording because it was made and corrected here.

It is natural to assume tens of seconds with no generation means many sequential
calls. It does not. Jev's native shape (`POST /v1/systemone`) takes one shared
`state` and a map of N independent typed questions in a single request, and
judgewalk has used it from the start — rank-leaves sends up to 120 Nouls in one
request; the page-head skim ~34; the full read ~4 only because each carries
6,000 characters and the provider caps state-plus-longest-question at 32k
tokens. Measured: **4.3 requests per question**.

The four round-trips are the four **levels** — rank sections, skim heads, read
pages, follow a cross-reference — and they are sequential because each one's
input is the previous one's output. Batching siblings is already the
implementation; there is nothing to win there. What remains:

- **One level is removable.** The head-skim depends on the ranking only because
  the chosen sections are skimmed. Skim every page and the two fire together.
- **Warm per-request latency is 1.5–2.6 s** and request *shape* barely matters.
  The first two calls of a session run 5–6 s — cold start, which dominates p95
  and every live demo.
- **~5 s per question was our own CPU**, not the provider: the tokenizer ran
  over every page while packing batches, then again on the assembled state.
  Fixed in `e9a3313`.

Run `20260921_184919` predates that fix, so its 37 s p50 is not the current
figure and must not be quoted as one until the run is repeated.

## Two things this note does not claim

**Jev is not deterministic.** Across repeats the returned set matches exactly
0.375 of the time (Jaccard 0.834), because probabilities near the threshold move
between calls. What Jev has is a far smaller *variance surface* — the output is
one letter, so drift cannot manifest as rambling, format breakage or invented
structure; it can only move the decision. That is a large reduction and it is
not determinism. `/v1/replay` reproduces an answer from its trace, which is a
different guarantee from re-running and getting the same thing, and the two must
never be blurred in front of a buyer who will check.

**Prompts still matter.** Jev's questions are prompts. Constraining the output
space bounds how badly one can go wrong; it does not remove prompt design, and
the system prompt is load-bearing — including the instruction to treat `state`
as data rather than instructions, which is the injection boundary for untrusted
document text. Question wording is now where retrieval quality lives.

## Consequences

1. Re-run `financebench_jev.yaml` post-`e9a3313` before any latency figure is
   quoted anywhere.
2. `path_correct_top1` reads 0.000 because HAL-1390 made evidence pages the
   leading units (`id: page_<n>`) and the harness derives `title_path` from the
   section tree, where a page has no path. Not unwired — but the GTM pack leads
   the legal segment with this metric and must not do so until page units carry
   a path derived from their owning leaf.
3. HAL-1542 is the enrichment layer, and it is typed judgements, not prose.
4. Multi-hop is unmeasured and is the real open question about this design.
