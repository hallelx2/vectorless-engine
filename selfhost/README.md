# Self-hosting the Vectorless engine

This directory runs `cmd/server`, the binary production ran, with Postgres beside it. You get the pipeline the [main README](../README.md#how-it-works) describes:

- Jev finds and places the table of contents.
- Retrieval uses judgewalk.
- Answers come with line-level pincites and page images, and ingest reports live progress.

Documents are stored on a Docker volume, and the job queue runs inside Postgres, so there's nothing else to operate. It's an HTTP API with no web interface. Call it with `curl`, the [SDKs](https://github.com/hallelx2/vectorless-sdk) or the [MCP server](https://github.com/hallelx2/vectorless-mcp).

## Start

```bash
cp .env.example .env
# edit .env: VECTORLESS_API_KEY, one answer-model key, VECTORLESS_TYPESAFE_API_KEY
docker compose up -d
docker compose ps        # engine is ready when it reports (healthy)
```

`VECTORLESS_API_KEY` is the bearer token every request carries. Use a long random string, for example from `openssl rand -hex 32`. Compose refuses to start without one.

## Use

```bash
K=<your VECTORLESS_API_KEY>
B=http://localhost:8080

# ingest (async): returns a document_id
curl -s -H "Authorization: Bearer $K" -F file=@report.pdf $B/v1/documents

# status and live progress, until "status":"ready"
curl -s -H "Authorization: Bearer $K" $B/v1/documents/doc_…

# answer with pincites
curl -s -H "Authorization: Bearer $K" -H 'Content-Type: application/json' \
  -d '{"document_id":"doc_…","query":"What was capital expenditure in 2018?"}' $B/v1/answer
```

Each pincite in the response carries a `page`, a `precision` (`line` or `page`), the `quote` taken from the page, and `regions`. Regions are rectangles in 0–1 page coordinates. Draw them over `GET /v1/documents/{id}/pages/{n}/image` to show the line. The [HTTP API table](../README.md#http-api) lists every route.

Behind a control plane, each request names its tenant in `X-Vectorless-Org`. Here `auth.default_org` in `engine.yaml` puts every request in one tenant, so you send only the bearer token. A request that does send the header still gets its own tenant.

## Models

| Variable | What it does |
|---|---|
| `VECTORLESS_LLM_PROVIDER` | `anthropic`, `openai`, `gemini` or `vertex`: the model that writes answers. |
| `VECTORLESS_ANTHROPIC_API_KEY`, `VECTORLESS_ANTHROPIC_BASE_URL`, `VECTORLESS_ANTHROPIC_MODEL` | Anthropic, or an Anthropic-compatible gateway. For GLM on z.ai the base URL is `https://api.z.ai/api/anthropic/v1`, and the `/v1` is required. |
| `VECTORLESS_OPENAI_API_KEY`, `VECTORLESS_OPENAI_BASE_URL`, `VECTORLESS_OPENAI_MODEL` | OpenAI, or any OpenAI-compatible endpoint. |
| `VECTORLESS_GEMINI_API_KEY`, `VECTORLESS_GEMINI_MODEL` | Gemini. |
| `VECTORLESS_VERTEX_PROJECT`, `VECTORLESS_VERTEX_LOCATION`, `VECTORLESS_VERTEX_MODEL`, `VECTORLESS_VERTEX_THINKING_BUDGET` | Gemini on Google Cloud Vertex AI. No key: a GCP VM's service account (or a credentials file at `VECTORLESS_GOOGLE_CREDENTIALS_PATH`) authenticates, and the project is billed. Set a thinking budget (`0` turns thinking off on models that allow it) or a level, not both. Off GCP, put a service-account key in `selfhost/gcp/key.json` (gitignored) and set `VECTORLESS_GOOGLE_CREDENTIALS_PATH=/etc/vectorless/gcp/key.json`. |
| `VECTORLESS_ANSWER_MODEL` | Optional: a different model for answers only. |
| `VECTORLESS_TYPESAFE_API_KEY` | Jev, TypeSafe's System One model, which makes the contents, page selection and pincite decisions. It's a hosted API, so document text goes to TypeSafe when it's set. |

Without a TypeSafe key the engine still answers, but it reads the contents page with your generative model. That takes minutes longer on long filings. Retrieval falls back to the generative page walk, and pincites pick quotes by word overlap.

**Pick a non-reasoning model for answers.** The answer budget is 1,200 tokens (`engine.retrieval.answer.max_answer_tokens`). A reasoning model such as GLM-4.6 can spend all of it thinking and return no text. The engine then fails the request with "the answer model returned no text", and the server log names the cause. GLM-4.5-Air answered the 3M test question in about 3 seconds. GLM-4.6 took 37 seconds and returned nothing.

Every variable has a `VECTORLESS_` prefix on purpose. Compose lets your shell override `.env`, and a shell set up for Claude Code or an SDK often exports `ANTHROPIC_BASE_URL` or `OPENAI_API_KEY`, which would otherwise win without telling you.

## Operating it

| Variable | Default | |
|---|---|---|
| `VECTORLESS_PORT` | `8080` | Host port. |
| `VECTORLESS_BIND` | `127.0.0.1` | Set `0.0.0.0` to serve beyond this machine. Put TLS in front: the server speaks plain HTTP. |
| `VECTORLESS_VERSION` | `latest` | Or a 12-character commit tag from GHCR, to pin one build. |
| `VECTORLESS_POSTGRES_PASSWORD` | `vectorless` | Postgres isn't published to the host, so this only matters if you change that. |
| `VECTORLESS_LOG_LEVEL` | `info` | `debug` logs each navigation step. |

The rest of the configuration is in `engine.yaml`, and [`config.server.example.yaml`](../config.server.example.yaml) documents every key.

**Data.** Documents live in the `documents` volume and everything else in `pgdata`. To back up, stop the engine, then dump Postgres and copy the volume:

```bash
docker compose stop engine
docker compose exec postgres pg_dump -U vectorless -Fc engine > engine.dump
docker run --rm -v "$(basename "$PWD")_documents:/data" -v "$PWD:/out" alpine tar czf /out/documents.tgz -C /data .
docker compose start engine
```

**Upgrading.** Run `docker compose pull && docker compose up -d`. Migrations run when the server starts.

## Building from source

To run a change you made to the engine, build the image and point Compose at it:

```bash
docker build -f ../Dockerfile.server -t vectorless-engine:local ..
VECTORLESS_IMAGE=vectorless-engine VECTORLESS_VERSION=local docker compose up -d
```
