<p align="center">
  <img src="docs/media/banner.png" alt="Vectorless: a retrieval engine with no vectors, citations that land on the exact line of the PDF" width="100%"/>
</p>

<p align="center">
  <strong>A Go retrieval engine with no chunking, no embeddings and no vector database.</strong><br/>
  It finds a document's own contents, picks the pages that answer a question, returns those pages verbatim, and pins each claim in the answer to a line on its page.
</p>

<p align="center">
  <a href="LICENSE"><img alt="License: Apache 2.0" src="https://img.shields.io/badge/license-Apache%202.0-blue.svg"/></a>
  <a href="go.mod"><img alt="Go version" src="https://img.shields.io/badge/go-%3E%3D1.25-00ADD8?logo=go&logoColor=white"/></a>
  <img alt="FinanceBench: 40/40 right section" src="https://img.shields.io/badge/FinanceBench-40%2F40%20right%20section-ff5a00"/>
  <img alt="Query: 1.9 s median" src="https://img.shields.io/badge/query-1.9%20s%20median-18181b"/>
  <img alt="Status: open source, not maintained" src="https://img.shields.io/badge/status-open%20source%2C%20not%20maintained-lightgrey"/>
</p>

<p align="center">
  <a href="#see-it-work">See it work</a> ·
  <a href="#status">Status</a> ·
  <a href="#how-it-works">How it works</a> ·
  <a href="#before-and-after-the-judge">Before and after the judge</a> ·
  <a href="#results">Results</a> ·
  <a href="#dependencies-you-should-know-about">Dependencies</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="#http-api">API</a> ·
  <a href="#configuration">Config</a>
</p>

<p align="center">
  <a href="docs/media/how-it-works.mp4"><img src="docs/media/how-it-works-light.gif" alt="How Vectorless reads, drawn as one machine: a card cabinet of sections, dials showing Jev's probability for each, a crane that pulls the chosen drawer, a light table where a stylus marks the exact line, and a printer for the answer" width="100%"/></a>
  <br/><sub><b>How Vectorless reads, as one machine.</b> The cabinet is the document, one drawer per section. The dials are Jev's probabilities, and the crane only goes where a needle points. On the light table the stylus marks the exact line the answer cites. Ingest → ask → select → read → cite → answer. <a href="docs/media/how-it-works.mp4">As video</a>.</sub>
</p>


---

## See it work

Recorded live on vectorless.store on 12 October 2026, before the hosted service shut down. The 3M 2018 10-K (160 pages) was ready to query 13 seconds after upload. The question below was answered in 5.4 seconds, with 3 of its 4 citations pinned to exact lines. Clicking one opens page 39 with the "Total Company" capital spending row highlighted: $1,577M in 2018 against $1,373M in 2017.

<p align="center">
  <a href="docs/media/live-walkthrough.mp4"><img src="docs/media/live-ask-and-pincite.gif" alt="Asking the 3M 2018 10-K about capital expenditure, then clicking a citation to land on the highlighted row of page 39" width="100%"/></a>
  <br/><sub><a href="docs/media/live-walkthrough.mp4">Watch the full 78-second walkthrough</a>: upload, live ingest, the structure tree, the question and the pincite.</sub>
</p>

## Status

The hosted Vectorless service is closing (decided 2026-10-11). The code is open source under Apache 2.0 and is no longer actively developed. Issues and pull requests may not get a reply. For anything else, write to halleluyaholudele@gmail.com.

## How it works

The engine ships as a single binary and runs on Postgres plus object storage. Ingest builds a table of contents with page numbers. A query picks sections, then pages, and returns the pages as they are. The only generated text anywhere in the pipeline is the final answer. The machine at the top of this page shows the whole loop; here it is stage by stage.

### Ingest (PDF)

1. **Layout.** [pdfgrab](https://github.com/hallelx2/pdfgrab), a pure-Go port of pdfplumber, reads every word and its rectangle on every page. The engine imports it under its earlier module path, `github.com/hallelx2/pdftable` v0.4.0.
2. **Contents found.** Code lists the candidate entries of the document's contents page. Jev, TypeSafe's System One model, confirms which ones are real.
3. **Contents placed.** For each entry, code searches the body for where that heading actually appears. Jev picks the page.
4. **Leaf splitting.** Any section longer than 20 pages is split at its own headings, so no leaf of the tree is huge. Jev decides which sub-headings to keep.
5. **Page images.** After the document is ready, `pdftoppm` renders each page at 144 dpi for the citation view.

Ingest makes no generative call ([2026-09-18 report](docs/evaluations/2026-09-18-toc-extraction-on-a-judge.md)). Markdown, HTML, DOCX and plain text are parsed into a heading tree instead. They have no pages, so they get no page images and no pincites.

### Query: judgewalk

The retrieval strategy is called `judgewalk` in configuration and `navigate` in API responses.

1. Jev ranks the tree's sections against the question.
2. It skims the head of each candidate page, then reads the best pages in full.
3. It follows cross-references such as "see Note 7" to the page they point at.
4. The chosen pages come back **verbatim**, with their page ranges.

Retrieval makes no generative call ([2026-09-18 report](docs/evaluations/2026-09-18-retrieval-navigation-on-a-judge.md)).

### Answer and pincites

A pincite is a citation to the exact page or line, the way a legal brief cites a page of a judgment.

1. A chat model of your choice writes the answer over the evidence pages, marking claims with `[n]`.
2. For each claim, code lists candidate quotes from that page's own sentences and table rows, and keeps a shortlist of 10.
3. Jev picks one quote per claim, in one batched call for the whole answer. It can also pick `none`.
4. If Jev's confidence is below 0.35, the citation falls back to page level.
5. The quote is located on the page exactly, or at 80% or more token overlap. Quotes under 4 tokens must match exactly.
6. The citation is drawn as one highlight rectangle per line, in page coordinates normalised to 0–1.

Citations come at line, page or section precision. There is no table-cell precision. Every quote is taken from the page's own text, so no generated text can reach a citation.

<p align="center">
  <img src="docs/media/pincite.png" alt="One claim becomes a highlighted line: candidate sentences from the page, Jev picks one with a probability, the quote is located and drawn as one rectangle per line" width="100%"/>
</p>

### Replay

Every answer is stored with a `trace_token`. `POST /v1/replay` with that token returns the stored response byte for byte. Replay is a record of what was answered. It does not re-run the query, and re-running would not necessarily give the same result (see [Results](#results)). The store is an in-memory LRU, 1,024 entries for 24 hours by default (`retrieval.replay`), so it does not survive a restart.

### The rule behind it

> If a decision can be written as a closed set of options, it goes to the judge.

Choosing a contents entry, a page, a section or a quote is a selection, not writing. Jev selects. A generative model writes only the answer. The reasoning is in [Judgement, not generation](docs/evaluations/2026-09-25-judgement-not-generation.md) (2026-09-25).

<p align="center">
  <img src="docs/media/select-dont-generate.png" alt="Moving closed decisions onto Jev: the contents stage went from 103 to 840 s and up to 9.5 cents per filing to 12 to 143 s and a fraction of a cent, and page placement from 16/20 to 20/20 gold pages" width="100%"/>
</p>

## Before and after the judge

Vectorless did not start out like this. From May to mid-September 2026 every decision in the pipeline was written by a chat model, and then over three days in September almost all of it moved onto a System One model. That move is the reason the numbers in [Results](#results) look the way they do, so it is worth telling properly.

<p align="center">
  <img src="docs/media/before-after-the-judge.png" alt="Before: a chat model summarises every section, builds the contents tree, walks the tree in up to 8 chat-completion hops, and writes the answer. After: pdfgrab layout, contents found and placed by Jev, judgewalk selecting sections and pages, and a model writes only the answer, with Jev picking each pincite quote." width="100%"/>
</p>

### How it worked before

The first design followed the PageIndex idea closely, and every stage of it generated text:

- **Summaries at ingest.** A chat model read every section and wrote a summary of it, later with structured "multi-axis" summaries and HyDE questions alongside (May 2026).
- **An LLM-built contents tree.** A chat model read the contents pages and wrote the tree, PageIndex style (27 May 2026).
- **Navigation by writing.** `treewalk`, renamed from `pageindex` on 6 June, gave a chat model the tree with its summaries and asked it to write which branch to open next, in up to eight chat-completion hops per question.

It worked, in the sense that answers came back. But each of those calls was slow, cost money, and could write something plausible and wrong. On the long 10-K filings that mattered most, the generative path placed sections on the wrong pages. The contents stage alone took 103 to 840 seconds and $0.017 to $0.095 per filing.

### What changed

Looking at the prompts, almost none of these decisions needed writing at all. "Is this line a contents entry?", "which of these pages does this heading start on?" and "which of these sections could hold the answer?" all have a closed set of answers. As the [25 September note](docs/evaluations/2026-09-25-judgement-not-generation.md) puts it, the question was a judgement when a chat model answered it in eight hundred tokens of prose, and it is the same judgement when a System One model answers it in one letter. The prose was only the medium the decision arrived in.

A System One model such as Jev takes a shared context and a batch of typed questions (yes or no, pick one of these, place this on a scale) and answers all of them in one call with calibrated probabilities. So code builds the list of options, and Jev selects from it.

### The switch, in order

| Date | Change | Where |
|---|---|---|
| 17 Sep | llmgate gains a Judge interface for System One models | [llmgate #16](https://github.com/hallelx2/llmgate/pull/16) |
| 17 Sep | The contents builder's yes/no phases move onto Jev | [#60](https://github.com/hallelx2/vectorless-engine/pull/60) |
| 18 Sep | Every leaf's page is resolved by Jev: select, don't generate | [#61](https://github.com/hallelx2/vectorless-engine/pull/61) |
| 18 Sep | The whole contents stage runs with no generative model | [#63](https://github.com/hallelx2/vectorless-engine/pull/63) |
| 18 Sep | Navigation runs on Jev (`judgewalk`), with real per-page text | [#64](https://github.com/hallelx2/vectorless-engine/pull/64) |
| 18 Sep | Adaptive concurrency through llmgate's limiter | [#65](https://github.com/hallelx2/vectorless-engine/pull/65), [llmgate #17](https://github.com/hallelx2/llmgate/pull/17) |
| 19 Sep | Long sections split at their own headings, on Jev | [#66](https://github.com/hallelx2/vectorless-engine/pull/66) |
| 25 Sep | The rule written down: a closed set of options goes to the judge | [judgement-not-generation](docs/evaluations/2026-09-25-judgement-not-generation.md) |
| 28 Sep | The latency was our own client: median query 50.5 s became 1.9 s | [latency-was-our-cpu](docs/evaluations/2026-09-28-latency-was-our-cpu.md) |
| Oct | Pincites: Jev picks the supporting sentence for every claim | [#72](https://github.com/hallelx2/vectorless-engine/pull/72) |

### What it did to the numbers

| | Generative | On Jev | Report |
|---|---|---|---|
| Contents stage, per filing | 103 to 840 s, $0.017 to $0.095, one generative call | 12 to 143 s, $0.0003 to $0.0015, no generative call, 0.961 title recall against the generative trees | [toc-extraction-on-a-judge](docs/evaluations/2026-09-18-toc-extraction-on-a-judge.md) |
| Placing sections on pages (11 filings) | 16/20 gold pages covered, median leaf 183 pages, $0.246 | 20/20, median leaf 36 pages, $0.0046 | [jev-page-resolver](docs/evaluations/2026-09-18-jev-page-resolver.md) |
| Whole corpus after the per-page text fix | lost pages on long 10-Ks | 47/47 gold pages, 21/21 filings | [retrieval-navigation-on-a-judge](docs/evaluations/2026-09-18-retrieval-navigation-on-a-judge.md) |
| Navigation | up to 8 chat-completion hops | 40/40 right section, 36/40 every gold page, $0.0039 per question | [leaf-granularity](docs/evaluations/2026-09-19-leaf-granularity.md) |

It is unusual for a change to make a system faster, cheaper and more accurate all at once. It happened here because the generative path was never the more careful reader. On structure, it was the weaker judge.

There was a second effect, and it shaped everything after. Once nothing in ingest or retrieval generates prose, no generated text can end up inside a citation. That stops being a rule someone has to remember and becomes something the architecture cannot express. Pincites are built on exactly that: the quote is always selected from the page, never written.

### What it cost

- **Deliberation.** A judge decides one node at a time. It does not plan across sections or combine evidence the way a chat model reasoning step by step might. Multi-hop questions are unmeasured.
- **Determinism.** Jev is not deterministic. Probabilities near a threshold move between calls, and repeated runs returned the same set of pages 0.38 of the time (Jaccard 0.83). Replay reproduces a stored answer; it does not re-run one.
- **A hosted dependency.** Jev is TypeSafe's API, so with a key set, document text leaves your network. Without a key the engine falls back to the generative path described above (see [Dependencies](#dependencies-you-should-know-about)).

## Results

All figures are from FinanceBench 10-K filings unless stated. Each links to the report that measured it. The full list is in [docs/evaluations](docs/evaluations/README.md).

<p align="center">
  <img src="docs/media/numbers.png" alt="40/40 right section; median query 50.5 s to 1.9 s; ingest 66 s to 4.1 s per filing; retrieval 1.70 s against PageIndex's 7.05 s; gold page in the top five 0.925 against 0.375 for embeddings and 0.200 for BM25" width="100%"/>
</p>

| Claim | Number | Date | Report |
|---|---|---|---|
| Building the contents tree on Jev instead of a generative model | 12–143 s and $0.0003–0.0015 per filing, against 103–840 s and $0.017–0.095; title recall 0.961 against the generative trees | 2026-09-18 | [toc-extraction-on-a-judge](docs/evaluations/2026-09-18-toc-extraction-on-a-judge.md) |
| Placing contents entries on their pages (11 filings) | Generative trees with no page resolver: 16/20 gold pages covered, median leaf 183 pages, $0.246. Same trees resolved on Jev: 20/20, median leaf 36 pages, $0.0046 | 2026-09-18 | [jev-page-resolver](docs/evaluations/2026-09-18-jev-page-resolver.md) |
| Navigation, first Jev run (40 questions) | 37/40 right section, 34/40 every gold page, $0.0029 per question | 2026-09-18 | [retrieval-navigation-on-a-judge](docs/evaluations/2026-09-18-retrieval-navigation-on-a-judge.md) |
| Navigation after leaf splitting | 40/40 right section, 36/40 every gold page, $0.0039 per question | 2026-09-19 | [leaf-granularity](docs/evaluations/2026-09-19-leaf-granularity.md) |
| Head-to-head at k=5: Vectorless, BGE-small embeddings, BM25 | hit@5 0.925 / 0.375 / 0.200; F1@5 0.498 / 0.170 / 0.096; answer in top-1 0.750 / 0.225 / 0.075 | 2026-09-21 run | [head-to-head-chunk-and-embed](docs/evaluations/2026-09-19-head-to-head-chunk-and-embed.md) |
| An embedding pre-filter does not help (negative result) | At k=50, BGE finds every gold page for 0.875 of questions, below judgewalk's 0.925 at k=5 | 2026-09-25 | [query-latency-and-the-embedding-prefilter](docs/evaluations/2026-09-25-query-latency-and-the-embedding-prefilter.md) |
| Query latency, navigator called directly | median 50.5 s became 1.9 s, still 36/40; $0.00402 became $0.00388 per question | 2026-09-28 | [latency-was-our-cpu](docs/evaluations/2026-09-28-latency-was-our-cpu.md) |
| Ingest latency per filing | median 66.1 s became 4.1 s, still 47/47 gold pages covered; $0.133 for 21 filings | 2026-09-28 | [latency-was-our-cpu](docs/evaluations/2026-09-28-latency-was-our-cpu.md) |
| Against PageIndex 0.2.22 on the 3M 2018 10-K (160 pages, 6 questions × 3 repeats) | retrieval median 1.70 s vs 7.05 s; full answer median 5.27 s vs 10.51 s; ingest 10.5–19.2 s vs 204–210 s; retrieved pages hold the answer 18/18 vs 17/18 | 2026-10-08 | [pageindex-latency](docs/evaluations/2026-10-08-pageindex-latency.md) |
| Run-to-run agreement across two repeats | The returned set matched exactly 0.38 of the time; Jaccard 0.83 | 2026-09-21 run | [head-to-head-chunk-and-embed](docs/evaluations/2026-09-19-head-to-head-chunk-and-embed.md) |

What these numbers do not show:

- **Retrieval is not answer accuracy.** hit@5 says the evidence was among the returned pages. It says nothing about whether the answer written from them was right. Do not set these figures against answer-accuracy figures such as PageIndex's published 98.7%. A small answer pilot (27/30 correct on 5 filings, graded with GLM-4.6 using the grading prompt from PageIndex's Mafin evaluation) is too small to compare with anything, and is not published as a report.
- **1.9 s and 4.1 s are benchmark-tool figures.** navbench calls the navigator directly, without the HTTP layer, database reads or the answer. The production API answered in about 3.5–6 s.
- **The PageIndex comparison is one filing on one machine.** When both systems use the same generative model for navigation, the answer gap narrows to 7.6 s against 10.5 s at the median and PageIndex wins 2 of 6 questions. The report says what it does not claim.
- **Jev is not deterministic.** See the run-to-run row above. Replay returns a stored answer; it does not make the engine deterministic.
- **Multi-hop questions are unmeasured.**

Pincite figures are from one filing, the 3M 2022 10-K (252 pages): the page layout was built in 3.0 s, the pincite step takes 0.4–0.6 s inside `/v1/answer`, and every claim checked by eye landed at line precision. That last one is a small visual check, not a benchmark.

To reproduce the FinanceBench runs, see `cmd/navbench`, `cmd/tocdump` and the harness in [vectorless-bench](https://github.com/hallelx2/vectorless-bench). Each report lists its harness, corpus and commands.

## Dependencies you should know about

- **Jev is a hosted API.** It is TypeSafe's System One model (`jev-latest`, endpoint `api.typesafe.ai/v1/systemone`). System One models make fast, structured decisions and write no prose. One call carries a shared state and N typed questions: yes/no with a probability, a choice among options with the distribution, or a position on an ordered scale. It costs $0.042 per million input tokens and output is free. Its size is not published. With a key set, document text is sent to TypeSafe, so "nothing leaves your network" is not true.
- **Set the key** with `TYPESAFE_API_KEY` (or `VLE_TYPESAFE_API_KEY` / `VLS_TYPESAFE_API_KEY`). `TYPESAFE_BASE_URL` and `TYPESAFE_MODEL` override the endpoint and model.
- **Without a key** the engine still runs, but not the pipeline measured above:
  - Ingest runs the contents-page decisions on your generative model, one call per page. It is minutes slower on long filings, and leaf splitting is off.
  - `judgewalk` falls back to `treewalk`, where your chat model walks the pages in up to eight generative hops.
  - Pincites choose quotes by word overlap instead of Jev.
- **When Jev refuses** (no credit, a rejected key, an outage), the server binary serves the query with `treewalk` instead and logs `retrieval: primary model unavailable, served by fallback`. This was added after the Judge provider ran out of credit on 2026-10-08 and every production query failed for as long as it lasted.
- **A generative model** writes answers: Anthropic, OpenAI, Gemini, or an Anthropic-compatible gateway such as GLM on z.ai. Callers can also send their own key per request.
- **poppler's `pdftoppm`** renders page images. `Dockerfile.server` installs it.

## Quick start

### The whole product: self-host

The dashboard, control plane, engine and Postgres all start from one command in the [`vectorless`](https://github.com/hallelx2/vectorless) repository. You sign up as the first user, enter your model keys on **Settings → Model providers**, and upload a document. Keys take effect without a restart.

Clone the three repositories side by side, then start the stack:

```bash
mkdir vectorless && cd vectorless
git clone https://github.com/hallelx2/vectorless.git
git clone https://github.com/hallelx2/vectorless-engine.git
git clone https://github.com/hallelx2/vectorless-control-plane.git
cd vectorless/selfhost && docker compose up -d --build
# when every service is healthy: http://localhost:3000
```

There is no `.env` to write. Secrets are generated on first boot.

<p align="center">
  <a href="https://github.com/hallelx2/vectorless/blob/main/selfhost/README.md"><img src="docs/media/selfhost-keys.gif" alt="A fresh self-hosted instance: the first account is created, then model keys are entered as dots on Settings, Model providers, tested and saved without a restart" width="100%"/></a>
  <br/><sub>First boot of a self-hosted instance: the administrator account, then model keys entered on <b>Settings → Model providers</b>. The full guide is in <a href="https://github.com/hallelx2/vectorless/blob/main/selfhost/README.md">selfhost/README.md</a>.</sub>
</p>

### The engine on its own

The engine has two entry points:

| Binary | What it is | Use it for |
|---|---|---|
| `cmd/server` (`Dockerfile.server`) | The multi-tenant HTTP server production ran. It has pincites, page images, live ingest progress and the strategy fallback. | Running the pipeline described above. |
| `cmd/engine` (`Dockerfile`, `Dockerfile.allinone`) | The standalone engine with `--local` zero-config mode and the `/v1/replay` route. | Trying the engine on a laptop. |

### All-in-one container

One container with the engine, a bundled Postgres and a web UI. Build it from this repository with `docker build -f Dockerfile.allinone -t vectorless .`. The `halleluyaholudele/vectorless` image on Docker Hub was last pushed on 2026-07-04, before Jev, judgewalk and pincites existed, so don't use it for the pipeline above. The image defaults to fast `minimal` ingest and GLM through z.ai, so it does not run the Jev pipeline unless you turn it on:

```bash
docker run -p 8080:8080 -p 7654:7654 \
  -e VLE_LLM_ANTHROPIC_API_KEY=<your key> \
  -e TYPESAFE_API_KEY=<your TypeSafe key> \
  -e VLE_INGEST_MODE=toc \
  -e VLE_RETRIEVAL_STRATEGY=judgewalk \
  vectorless
# UI  → http://localhost:8080
# API → http://localhost:7654
```

### From source (Go 1.25+, Postgres)

```bash
git clone https://github.com/hallelx2/vectorless-engine.git && cd vectorless-engine
docker compose up -d postgres
export VLE_LLM_ANTHROPIC_API_KEY=<your key>
export TYPESAFE_API_KEY=<your TypeSafe key>
export VLE_INGEST_MODE=toc VLE_RETRIEVAL_STRATEGY=judgewalk
go run ./cmd/engine --local      # zero-config local mode on :7654
```

For the dashboard and everything around it, use the self-host stack above.

## HTTP API

Routes are versioned under `/v1`. This is the server binary's surface.

| Method | Path | Purpose |
|--------|------|---------|
| `GET`  | `/v1/health` · `/v1/version` | Liveness and build |
| `POST` | `/v1/documents` | Ingest a document (multipart or URL; async, 202) |
| `GET`  | `/v1/documents` · `/v1/documents/{id}` | List, or get status and ingest progress |
| `DELETE` | `/v1/documents/{id}` | Delete a document and its stored files |
| `GET`  | `/v1/documents/{id}/tree` · `/structure` | Section tree; structure with page spans |
| `GET`  | `/v1/documents/{id}/llms.txt` · `/text` · `/source` | Contents as llms.txt; plain text; original bytes |
| `GET`  | `/v1/documents/{id}/pages` · `/pages/{n}/image` | Page sizes for pincites; one page image |
| `GET`  | `/v1/sections/{id}` | One section with its content |
| `POST` | `/v1/query` · `/v1/query/stream` | Retrieve evidence (streamed: one event per navigation stage) |
| `POST` | `/v1/query/multi` · `/v1/query/multi/stream` | Retrieve across several documents |
| `POST` | `/v1/answer` | Retrieve, answer and pincite in one round trip |
| `POST` | `/v1/answer/treewalk` | Answer with the generative page walk |
| `POST` | `/v1/answer/store` | One answer across a store of documents, with per-document citations |

`POST /v1/replay` is served by `cmd/engine`. On this commit `cmd/server` records replay traces but does not register the route.

Per-request model keys (BYOK): `X-LLM-Api-Key`, with optional `X-LLM-Provider`, `X-LLM-Base-Url` and `X-LLM-Model`. Nothing about the key is logged or stored.

## SDKs and MCP

Clients for TypeScript (npm `vectorless`), Python (PyPI `vectorless-sdk`) and Go live in [`vectorless-sdk`](https://github.com/hallelx2/vectorless-sdk). [`vectorless-mcp`](https://github.com/hallelx2/vectorless-mcp) exposes seven tools to agents: list, ingest, get and delete document, get_tree, get_section, and query.

## Architecture

A single Go binary with pluggable boundaries:

| Boundary | Implementations |
|----------|-----------------|
| **Storage** | Local filesystem · S3-compatible (R2, MinIO, B2, Spaces) · Google Cloud Storage |
| **Queue**   | River (Postgres) · Asynq (Redis) · QStash (serverless) |
| **Generative model** | Anthropic · OpenAI · Gemini, and Anthropic-compatible gateways (GLM on z.ai), through [llmgate](https://github.com/hallelx2/llmgate) |
| **Judge** | TypeSafe Jev through llmgate's Judge interface |
| **Retrieval** | `judgewalk` (the pipeline above) · `treewalk` (generative page walk) · `auto` (the config default) · `single-pass` · `chunked-tree` · `agentic` |
| **Ingest mode** | `toc` (the pipeline above) · `minimal` (parse and store, seconds) · `full` (adds the older generative per-section summaries and enrichment) |

`single-pass`, `chunked-tree`, `agentic` and the `full` ingest mode are the original design, in which a chat model summarised every section at ingest and navigated those summaries at query time. They still build and run, but the evaluations above did not use them. Design docs in [docs/](docs/README.md) predate the current pipeline in places and say so at the top.

## Configuration

Config layers, in increasing priority: `--config <yaml>`, then `VLE_*` (engine) or `VLS_*` (server) environment variables, then CLI flags. [`config.example.yaml`](config.example.yaml) is the full reference. The settings production ran with:

```yaml
engine:
  llm:
    driver: anthropic          # the answer model; key from VLE_LLM_ANTHROPIC_API_KEY
    judge:
      typesafe: {}             # key from TYPESAFE_API_KEY
  retrieval:
    strategy: judgewalk
    replay: { enabled: true }
  ingest:
    mode: toc
```

> **Anthropic-compatible gateways (GLM on z.ai):** `base_url` must include `/v1`. The client posts to `${base_url}/messages`.

> **Windows and local storage:** Windows Defender's real-time scan can briefly hide a freshly written file, which under heavy concurrent ingest shows up as transient `object not found` errors. The local backend retries through this, but for bulk loads add a Defender exclusion for your storage root: `Add-MpPreference -ExclusionPath "C:\path\to\data\documents"`.

## Related

- [`pdfgrab`](https://github.com/hallelx2/pdfgrab): the PDF layout parser (MIT)
- [`llmgate`](https://github.com/hallelx2/llmgate): provider clients, router, retries, budgets, and the Judge interface (Apache 2.0)
- [`vectorless-sdk`](https://github.com/hallelx2/vectorless-sdk): TypeScript, Python and Go clients
- [`vectorless-bench`](https://github.com/hallelx2/vectorless-bench): a harness that reports quality, cost, latency and determinism together

## License

[Apache 2.0](LICENSE).
