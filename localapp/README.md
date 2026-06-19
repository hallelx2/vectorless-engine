# Vectorless — local viewer

A tiny, dependency-free local UI for the OSS `vectorless-engine`. Upload a PDF,
watch it ingest into a structured tree, browse the section map, and ask
questions that come back with **cited** answers (page range + verbatim quote) —
answered by whatever model the engine is configured with (here: GLM-4.6 via
z.ai's Anthropic-compatible gateway).

This is the minimal slice of **HAL-188** (local dashboard). It is intentionally
small: a single `index.html` + a stdlib Python proxy. No build step, no Node.

## Why the proxy
The `engine --local` binary emits **no CORS headers**, so a browser page can't
call `http://localhost:7654` cross-origin. `serve.py` serves the page **and**
reverse-proxies `/engine/*` to the engine, so every request is same-origin.

## Run

```bash
# 1. Start the engine (from vectorless-engine/), local mode + your GLM key:
cd ../vectorless-engine
set -a; . ./.env; set +a          # GLM key + base_url (.../api/anthropic/v1) + glm-4.6
export VLE_INGEST_MODE=minimal
./bin/engine.exe --local           # listens on :7654

# 2. Start the viewer (from this folder):
cd ../local-viewer
python serve.py                    # http://localhost:7655
```

Then open **http://localhost:7655** and:
1. Drop a PDF (e.g. a FinanceBench 10-K) onto **Upload**.
2. Watch it move to **ready** in the **Documents** list; click it.
3. Inspect the **Structure map** (section tree + page ranges).
4. Type a question in **Ask** → get a cited answer with confidence, hops, and cost.

## Config
- `ENGINE_URL` (default `http://localhost:7654`) — where the engine listens.
- `VIEWER_PORT` (default `7655`) — the viewer's port.

## Endpoints it uses
`GET /v1/health` · `GET /v1/documents` · `POST /v1/documents` (multipart) ·
`GET /v1/documents/{id}` · `GET /v1/documents/{id}/tree` ·
`POST /v1/answer/treewalk`.
