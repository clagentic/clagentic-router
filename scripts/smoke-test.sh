#!/usr/bin/env bash
# scripts/smoke-test.sh — end-to-end smoke test for clagentic-router.
#
# Starts the daemon against a minimal stub config that uses ollama_http
# (no auth, no API keys required), exercises every documented HTTP endpoint,
# and verifies response shape and headers.
#
# Requirements:
#   - Go toolchain (for `make build`)
#   - curl, jq
#   - An Ollama server reachable at $OLLAMA_URL (default http://localhost:11434)
#
# Usage:
#   CLAGENTIC_ROUTER_TOKEN=test ./scripts/smoke-test.sh
#   OLLAMA_URL=http://localhost:11434 CLAGENTIC_ROUTER_TOKEN=test ./scripts/smoke-test.sh
#
# Exit codes:
#   0  all checks passed
#   1  one or more checks failed

set -euo pipefail

## ---- config ----------------------------------------------------------------

BINARY="${BINARY:-./bin/clagentic-router}"
PORT="${PORT:-18765}"
BASE="http://127.0.0.1:${PORT}"
TOKEN="${CLAGENTIC_ROUTER_TOKEN:-smoke-test-token}"
OLLAMA_URL="${OLLAMA_URL:-http://localhost:11434}"
OLLAMA_MODEL="${OLLAMA_MODEL:-phi4-mini}"
LOG_LEVEL="${LOG_LEVEL:-warn}"   # suppress info noise during test run

PASS=0
FAIL=0
DAEMON_PID=""

## ---- helpers ---------------------------------------------------------------

green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
bold()  { printf '\033[1m%s\033[0m\n' "$*"; }

check() {
    local label="$1"; shift
    if "$@" >/dev/null 2>&1; then
        green "  PASS  $label"
        (( PASS++ )) || true
    else
        red   "  FAIL  $label"
        (( FAIL++ )) || true
    fi
}

# check_body: run curl, assert jq expression is truthy
check_body() {
    local label="$1"
    local jq_expr="$2"
    local response
    response="$3"
    if echo "$response" | jq -e "$jq_expr" >/dev/null 2>&1; then
        green "  PASS  $label"
        (( PASS++ )) || true
    else
        red   "  FAIL  $label  [jq: $jq_expr]"
        echo  "         response: $(echo "$response" | head -3)"
        (( FAIL++ )) || true
    fi
}

auth_get() {
    curl -sf -H "Authorization: Bearer $TOKEN" "$BASE$1"
}

auth_post() {
    local path="$1"; shift
    curl -sf -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
         -X POST -d "$1" "$BASE$path"
}

cleanup() {
    if [[ -n "$DAEMON_PID" ]]; then
        kill "$DAEMON_PID" 2>/dev/null || true
        wait "$DAEMON_PID" 2>/dev/null || true
    fi
    rm -f "$CONFIG_FILE" "$DB_FILE"
}
trap cleanup EXIT

## ---- build -----------------------------------------------------------------

bold "=== clagentic-router smoke test ==="
echo ""
bold "Building binary..."
make build 2>&1 | tail -2

## ---- config file -----------------------------------------------------------

CONFIG_FILE="$(mktemp /tmp/smoke-router-XXXXXX.yaml)"
DB_FILE="$(mktemp /tmp/smoke-router-XXXXXX.db)"

cat > "$CONFIG_FILE" <<EOF
backends:
  ollama-smoke:
    adapter: ollama_http
    url: ${OLLAMA_URL}
    model: ${OLLAMA_MODEL}
    cost_weight: 1.0
    timeout_seconds: 60

tiers:
  local: [ollama-smoke]

chains:
  default: [local]

routing:
  strategy: scored
  degraded_failure_threshold: 3
  offline_failure_threshold: 6
  health_probe_interval_seconds: 3600
  quota_warning_threshold: 0.20
  latency_penalty_threshold_ms: 15000

proxy:
  host: 127.0.0.1
  port: ${PORT}
  token: ${TOKEN}

storage:
  db_path: ${DB_FILE}

log:
  level: ${LOG_LEVEL}
  format: text
EOF

## ---- start daemon ----------------------------------------------------------

bold "Starting daemon (port $PORT)..."
CLAGENTIC_ROUTER_TOKEN="$TOKEN" "$BINARY" serve --config "$CONFIG_FILE" &
DAEMON_PID=$!

# Wait for the daemon to accept connections (up to 5s)
for i in $(seq 1 10); do
    if curl -sf "$BASE/version" >/dev/null 2>&1; then
        break
    fi
    sleep 0.5
done

if ! curl -sf "$BASE/version" >/dev/null 2>&1; then
    red "FATAL: daemon did not start within 5s"
    exit 1
fi
echo "  daemon up (pid $DAEMON_PID)"
echo ""

## ---- 1. unauthenticated access ---------------------------------------------

bold "--- 1. Authentication"

check "GET /version (no auth — expect 200)" \
    curl -sf "$BASE/version"

check "GET /health (no token — expect 401)" bash -c \
    "[[ \$(curl -s -o /dev/null -w '%{http_code}' $BASE/health) == 401 ]]"

check "GET /health (wrong token — expect 401)" bash -c \
    "[[ \$(curl -s -o /dev/null -w '%{http_code}' -H 'Authorization: Bearer wrong' $BASE/health) == 401 ]]"

echo ""
bold "--- 2. Observability endpoints"

VERSION_RESP="$(curl -sf "$BASE/version")"
check_body "GET /version returns version field" '.version' "$VERSION_RESP"

HEALTH_RESP="$(auth_get /health)"
check_body "GET /health returns status field"   '.status'   "$HEALTH_RESP"

DOCTOR_RESP="$(auth_get /doctor)"
check_body "GET /doctor returns results array" '.results | type == "array"' "$DOCTOR_RESP"

QUOTA_RESP="$(auth_get /quota)"
# /quota is a map keyed by backend ID — verify the key exists
check_body "GET /quota returns backend entry"  ".[\"ollama-smoke\"] | type == \"object\"" "$QUOTA_RESP"

MODELS_RESP="$(auth_get /v1/models)"
check_body "GET /v1/models returns object list" '.object == "list"' "$MODELS_RESP"
check_body "GET /v1/models data is array"       '.data | type == "array"' "$MODELS_RESP"

METRICS_RESP="$(auth_get /metrics)"
check "GET /metrics returns Prometheus text" \
    bash -c "echo '$METRICS_RESP' | grep -q 'router_backend_status'"

LOGS_RESP="$(auth_get /logs)"
check_body "GET /logs returns rows key"  '.rows | (type == "array" or . == null)' "$LOGS_RESP"

STATS_RESP="$(auth_get /stats)"
check_body "GET /stats returns total_calls field" '.total_calls | type == "number"' "$STATS_RESP"

echo ""
bold "--- 3. Backend control"

# Register, disable, enable, reset a backend
check "POST /backends/ollama-smoke/disable returns 200" \
    auth_post /backends/ollama-smoke/disable '{}'

QUOTA_AFTER="$(auth_get /quota)"
# /quota is keyed by backend ID; check the status field directly
check_body "Backend is disabled in /quota after disable" \
    '.["ollama-smoke"].status == "offline"' "$QUOTA_AFTER"

check "POST /backends/ollama-smoke/enable returns 200" \
    auth_post /backends/ollama-smoke/enable '{}'

check "POST /backends/ollama-smoke/reset returns 200" \
    auth_post /backends/ollama-smoke/reset '{}'

echo ""
bold "--- 4. Webhook CRUD"

WH_RESP="$(auth_post /webhooks '{"url":"http://127.0.0.1:19999/hook","events":["backend_offline"]}')"
check_body "POST /webhooks returns id"  '.id' "$WH_RESP"
WH_ID="$(echo "$WH_RESP" | jq -r '.id')"

WH_LIST="$(auth_get /webhooks)"
# Webhook list is {"webhooks":[...]}, entries use uppercase "ID" field
check_body "GET /webhooks shows registered webhook" \
    ".webhooks | [.[] | select(.ID==\"$WH_ID\")] | length == 1" "$WH_LIST"

check "DELETE /webhooks/{id} returns 200" bash -c \
    "[[ \$(curl -s -o /dev/null -w '%{http_code}' -X DELETE \
        -H 'Authorization: Bearer $TOKEN' $BASE/webhooks/$WH_ID) == 200 ]]"

WH_LIST_AFTER="$(auth_get /webhooks)"
# After last webhook deleted, .webhooks may be null (empty store); either is acceptable
check_body "Webhook gone after delete" \
    ".webhooks == null or ([.webhooks[] | select(.ID==\"$WH_ID\")] | length == 0)" "$WH_LIST_AFTER"

echo ""
bold "--- 5. Inference — non-streaming"

INFER_RESP="$(auth_post /v1/chat/completions \
    '{"model":"backend:ollama-smoke","messages":[{"role":"user","content":"Reply with the single word: pong"}],"max_tokens":16}')"

check_body "POST /v1/chat/completions returns choices array" \
    '.choices | length > 0' "$INFER_RESP"
check_body "Response has finish_reason" \
    '.choices[0].finish_reason != null' "$INFER_RESP"
check_body "Response message has content" \
    '.choices[0].message.content | length > 0' "$INFER_RESP"

# Verify router headers — use -D to dump headers alongside the body, avoid -I with POST
INFER_HEADERS="$(curl -s -D - \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -X POST -d '{"model":"backend:ollama-smoke","messages":[{"role":"user","content":"hi"}],"max_tokens":8}' \
    "$BASE/v1/chat/completions" || true)"
check "X-Router-Backend header present" \
    bash -c "echo '$INFER_HEADERS' | grep -qi 'x-router-backend'"
check "X-Router-Latency-Ms header present" \
    bash -c "echo '$INFER_HEADERS' | grep -qi 'x-router-latency-ms'"

echo ""
bold "--- 6. Inference — SSE streaming"

STREAM_OUT="$(curl -sf \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -X POST -d '{"model":"backend:ollama-smoke","messages":[{"role":"user","content":"Reply with: ok"}],"max_tokens":8,"stream":true}' \
    "$BASE/v1/chat/completions")"

check "SSE response contains data: lines" \
    bash -c "echo '$STREAM_OUT' | grep -q 'data: '"
check "SSE response ends with [DONE]" \
    bash -c "echo '$STREAM_OUT' | grep -q 'data: \[DONE\]'"

echo ""
bold "--- 7. Error cases"

check "POST /v1/chat/completions missing model — expect 400" bash -c \
    "[[ \$(curl -s -o /dev/null -w '%{http_code}' \
        -H 'Authorization: Bearer $TOKEN' \
        -H 'Content-Type: application/json' \
        -X POST -d '{\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}' \
        $BASE/v1/chat/completions) == 400 ]]"

check "POST /v1/chat/completions bad JSON — expect 400" bash -c \
    "[[ \$(curl -s -o /dev/null -w '%{http_code}' \
        -H 'Authorization: Bearer $TOKEN' \
        -H 'Content-Type: application/json' \
        -X POST -d 'not-json' \
        $BASE/v1/chat/completions) == 400 ]]"

check "GET /nonexistent — expect 404 or 405" bash -c \
    "[[ \$(curl -s -o /dev/null -w '%{http_code}' \
        -H 'Authorization: Bearer $TOKEN' \
        $BASE/nonexistent) =~ ^(404|405)\$ ]]"

echo ""
bold "--- 8. Call log populated after inference"

LOGS_AFTER="$(auth_get /logs)"
check_body "Call log has at least one entry after inference" \
    '.rows | length > 0' "$LOGS_AFTER"
check_body "Call log entry has BackendID field" \
    '.rows[0].BackendID | length > 0' "$LOGS_AFTER"

## ---- summary ---------------------------------------------------------------

echo ""
bold "=== Results ==="
echo "  Passed: $PASS"
echo "  Failed: $FAIL"
echo ""

if [[ "$FAIL" -gt 0 ]]; then
    red "SMOKE TEST FAILED ($FAIL failures)"
    exit 1
else
    green "SMOKE TEST PASSED ($PASS checks)"
    exit 0
fi
