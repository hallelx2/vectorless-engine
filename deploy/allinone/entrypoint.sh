#!/usr/bin/env bash
# All-in-one entrypoint: Postgres + Vectorless engine + the local viewer UI,
# all in one container. Postgres is bundled so `docker run` needs no external
# services — the user only supplies an LLM provider key.
set -euo pipefail

PGUSER_="${POSTGRES_USER:-vectorless}"
PGDB_="${POSTGRES_DB:-vectorless}"

echo "[vectorless] starting bundled Postgres…"
# The official postgres entrypoint handles first-run initdb (using the
# POSTGRES_* env vars) and then execs postgres. Run it in the background so we
# can start the engine + UI alongside it in the same container.
docker-entrypoint.sh postgres &

echo "[vectorless] waiting for Postgres to accept connections…"
until pg_isready -h localhost -U "$PGUSER_" -d "$PGDB_" >/dev/null 2>&1; do
  sleep 1
done
echo "[vectorless] Postgres ready."

# Start the viewer UI (serves the single-page app + same-origin proxy to the
# engine). Backgrounded; the engine is the container's main process.
if [ -f /opt/vectorless-app/serve.py ]; then
  echo "[vectorless] starting viewer UI on :${VIEWER_PORT:-8080} → ${ENGINE_URL:-http://localhost:7654}"
  PYTHONIOENCODING=utf-8 python3 /opt/vectorless-app/serve.py &
fi

if [ -z "${VLE_LLM_ANTHROPIC_API_KEY:-}" ] && [ -z "${VLE_LLM_OPENAI_API_KEY:-}" ] && [ -z "${VLE_LLM_GEMINI_API_KEY:-}" ]; then
  echo "[vectorless] WARNING: no LLM provider key set. Ingestion will work, but"
  echo "[vectorless]          queries need e.g. -e VLE_LLM_ANTHROPIC_API_KEY=<your GLM key>"
fi

echo "[vectorless] starting engine (local mode) on :7654 …"
# exec so the engine becomes PID 1's foreground process and receives signals.
exec engine --local
