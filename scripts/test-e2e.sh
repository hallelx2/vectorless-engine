#!/usr/bin/env bash
# End-to-end test for the vectorless server.
#
# Prerequisites:
#   - Server running at http://localhost:8080
#   - Postgres running with migrations applied
#
# Usage:
#   ./scripts/test-e2e.sh [base_url]

set -euo pipefail

BASE="${1:-http://localhost:8080}"
PASS=0
FAIL=0

green() { printf "\033[32m%s\033[0m\n" "$*"; }
red()   { printf "\033[31m%s\033[0m\n" "$*"; }

check() {
  local name="$1" expected="$2" actual="$3"
  if [[ "$actual" == *"$expected"* ]]; then
    green "  ✓ $name"
    ((PASS++))
  else
    red "  ✗ $name (expected '$expected', got '$actual')"
    ((FAIL++))
  fi
}

echo "=== Vectorless Server E2E Tests ==="
echo "    Base URL: $BASE"
echo ""

# ── 1. Health ──────────────────────────────────────────────────────
echo "── Health ──"
RESP=$(curl -s "$BASE/v1/health")
check "GET /v1/health returns ok" '"status":"ok"' "$RESP"

RESP=$(curl -s "$BASE/v1/version")
check "GET /v1/version returns version" '"version"' "$RESP"

# ── 2. Metrics ─────────────────────────────────────────────────────
echo "── Metrics ──"
RESP=$(curl -s "$BASE/metrics" | head -5)
check "GET /metrics returns prometheus" "http_requests_total" "$RESP"

# ── 3. Ingest a document ──────────────────────────────────────────
echo "── Ingest ──"
RESP=$(curl -s -X POST "$BASE/v1/documents" \
  -H "Content-Type: application/json" \
  -d '{
    "filename": "test.md",
    "content": "# Hello World\n\nThis is a test document for the vectorless engine.\n\n## Section One\n\nFirst section content with some detail.\n\n## Section Two\n\nSecond section with different content."
  }')
check "POST /v1/documents returns 202 with document_id" '"document_id"' "$RESP"
DOC_ID=$(echo "$RESP" | grep -o '"document_id":"[^"]*"' | cut -d'"' -f4)
echo "    Document ID: $DOC_ID"

# ── 4. List documents ─────────────────────────────────────────────
echo "── List ──"
RESP=$(curl -s "$BASE/v1/documents")
check "GET /v1/documents returns items array" '"items"' "$RESP"

# ── 5. Get document ───────────────────────────────────────────────
echo "── Get Document ──"
if [[ -n "$DOC_ID" ]]; then
  RESP=$(curl -s "$BASE/v1/documents/$DOC_ID")
  check "GET /v1/documents/{id} returns document" "$DOC_ID" "$RESP"
fi

# ── 6. Wait for ingest to complete ────────────────────────────────
echo "── Waiting for ingest (up to 30s) ──"
for i in $(seq 1 30); do
  RESP=$(curl -s "$BASE/v1/documents/$DOC_ID")
  STATUS=$(echo "$RESP" | grep -o '"status":"[^"]*"' | cut -d'"' -f4)
  if [[ "$STATUS" == "ready" ]]; then
    green "  ✓ Document reached 'ready' state after ${i}s"
    ((PASS++))
    break
  elif [[ "$STATUS" == "failed" ]]; then
    red "  ✗ Document failed: $RESP"
    ((FAIL++))
    break
  fi
  sleep 1
done
if [[ "$STATUS" != "ready" && "$STATUS" != "failed" ]]; then
  red "  ✗ Document still in '$STATUS' after 30s"
  ((FAIL++))
fi

# ── 7. Get tree ───────────────────────────────────────────────────
echo "── Tree ──"
if [[ "$STATUS" == "ready" ]]; then
  RESP=$(curl -s "$BASE/v1/documents/$DOC_ID/tree")
  check "GET /v1/documents/{id}/tree returns sections" '"sections"' "$RESP"
fi

# ── 8. Query ──────────────────────────────────────────────────────
echo "── Query ──"
if [[ "$STATUS" == "ready" ]]; then
  RESP=$(curl -s -X POST "$BASE/v1/query" \
    -H "Content-Type: application/json" \
    -d "{\"document_id\": \"$DOC_ID\", \"query\": \"What is in section one?\"}")
  check "POST /v1/query returns sections" '"sections"' "$RESP"
  check "POST /v1/query returns strategy name" '"strategy"' "$RESP"
fi

# ── 9. Connect-RPC (HTTP/JSON mode) ──────────────────────────────
echo "── Connect-RPC ──"
RESP=$(curl -s -X POST "$BASE/vectorless.v1.HealthService/Check" \
  -H "Content-Type: application/json" \
  -d '{}')
check "Connect-RPC Health/Check returns ok" '"status":"ok"' "$RESP"

RESP=$(curl -s -X POST "$BASE/vectorless.v1.HealthService/Version" \
  -H "Content-Type: application/json" \
  -d '{}')
check "Connect-RPC Health/Version returns version" '"version"' "$RESP"

# ── 10. Delete document ───────────────────────────────────────────
echo "── Cleanup ──"
if [[ -n "$DOC_ID" ]]; then
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X DELETE "$BASE/v1/documents/$DOC_ID")
  check "DELETE /v1/documents/{id} returns 204" "204" "$HTTP_CODE"
fi

# ── Summary ───────────────────────────────────────────────────────
echo ""
echo "=== Results: $PASS passed, $FAIL failed ==="
[[ $FAIL -eq 0 ]] && green "All tests passed!" || red "Some tests failed."
exit $FAIL
