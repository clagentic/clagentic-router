# Smoke Test

End-to-end validation procedure for clagentic-router. Covers all HTTP endpoints,
authentication, inference (non-streaming + SSE), webhook CRUD, backend control,
and the call log — against a live daemon backed by a real adapter.

## When to run

- After any merged PR that touches the server, router, or backend packages
- Before tagging a release
- After deploying a new binary to your host
- When onboarding a new environment or host

## Script

```
scripts/smoke-test.sh
```

### Prerequisites

| Requirement | Notes |
|---|---|
| Go toolchain | For `make build`. Binary must be on PATH or in `./bin/`. |
| `curl`, `jq` | Standard POSIX tools |
| Ollama server | Default: `http://localhost:11434` with `phi4-mini` |

The script uses `ollama_http` so no API keys or OAuth sessions are required.
If you have an Ollama server elsewhere, set `OLLAMA_URL`.

### Invocation

```bash
# Default (uses localhost)
CLAGENTIC_ROUTER_TOKEN=test ./scripts/smoke-test.sh

# Custom Ollama
OLLAMA_URL=http://localhost:11434 CLAGENTIC_ROUTER_TOKEN=test ./scripts/smoke-test.sh

# Different port (default 18765, so it doesn't collide with production)
PORT=19999 CLAGENTIC_ROUTER_TOKEN=test ./scripts/smoke-test.sh

# Verbose daemon logs (useful when debugging a failure)
LOG_LEVEL=debug CLAGENTIC_ROUTER_TOKEN=test ./scripts/smoke-test.sh
```

### Environment variables

| Variable | Default | Description |
|---|---|---|
| `CLAGENTIC_ROUTER_TOKEN` | `smoke-test-token` | Bearer token used for all requests |
| `PORT` | `18765` | Port the test daemon binds to |
| `OLLAMA_URL` | `http://localhost:11434` | Ollama server URL |
| `OLLAMA_MODEL` | `phi4-mini` | Model to load from Ollama |
| `LOG_LEVEL` | `warn` | Daemon log level during test run |
| `BINARY` | `./bin/clagentic-router` | Path to the binary (rebuilt by script) |

## What is checked

### 1. Authentication
| Check | Expected |
|---|---|
| `GET /version` (no auth) | 200 — version endpoint is public |
| `GET /health` (no token) | 401 |
| `GET /health` (wrong token) | 401 |

### 2. Observability endpoints
| Check | Expected |
|---|---|
| `GET /version` | JSON with `version` field |
| `GET /health` | JSON with `status` field |
| `GET /doctor` | JSON with `results` array (one entry per backend) |
| `GET /quota` | JSON object keyed by backend ID; each value has `status`, `quota_exhausted`, etc. |
| `GET /v1/models` | OpenAI list object `{"object":"list","data":[...]}` |
| `GET /metrics` | Prometheus text; metrics prefixed `router_backend_*` |
| `GET /logs` | JSON `{"rows":[...]}` — array may be null when empty |
| `GET /stats` | Flat JSON object with `total_calls`, `avg_latency_ms`, etc. |

### 3. Backend control
| Check | Expected |
|---|---|
| `POST /backends/{id}/disable` | 200; backend status becomes `offline` in `/quota` |
| `POST /backends/{id}/enable` | 200 |
| `POST /backends/{id}/reset` | 200 |

### 4. Webhook CRUD
| Check | Expected |
|---|---|
| `POST /webhooks` | 201 with `id` field (lowercase) |
| `GET /webhooks` | `{"webhooks":[...]}` — entries use uppercase `ID` field |
| `DELETE /webhooks/{id}` | 200 |
| `GET /webhooks` (after delete) | Webhook absent from list |

### 5. Inference — non-streaming
| Check | Expected |
|---|---|
| `POST /v1/chat/completions` with `"stream":false` | `choices` array with content |
| Response has `finish_reason` | Non-null |
| `X-Router-Backend` header | Present |
| `X-Router-Latency-Ms` header | Present |

### 6. Inference — SSE streaming
| Check | Expected |
|---|---|
| `POST /v1/chat/completions` with `"stream":true` | Response contains `data:` lines |
| Response ends with `data: [DONE]` | Present |

### 7. Error cases
| Check | Expected |
|---|---|
| Missing `model` field | 400 |
| Malformed JSON body | 400 |
| Unknown route | 404 or 405 |
| `working_dir` not absolute (e.g. `relative/path`) | 400 |

### 9. `working_dir`
| Check | Expected |
|---|---|
| `POST /v1/chat/completions` with `"working_dir":"/tmp"` | 200 — valid absolute, existing directory is accepted |
| `POST /v1/chat/completions` with `"working_dir":"/no/such/path"` | 400 — path does not exist |
| `POST /v1/messages` (routed mode) with `"working_dir":"/tmp"` | 200 — same validation applies to the routed Anthropic surface |

The smoke harness runs against `ollama_http`, an HTTP adapter that ignores
`working_dir` entirely (see README.md's "Working directory" section) — these
checks exercise the wire-boundary validation in `backend.ResolveWorkingDir`,
not subprocess `cmd.Dir`. Subprocess adapters (`claude_cli`, `codex_cli`,
`codex_subagent`, `gemini_cli`) require an OAuth session on the host and are
out of scope for this harness; `internal/backend/working_dir_test.go`
exercises `cmd.Dir` assignment directly per adapter.

### 8. Call log
| Check | Expected |
|---|---|
| `/logs` after inference | At least one call entry |
| Entry has `backend` field | Non-empty string |

## Interpreting failures

**Daemon did not start within 5s** — binary is not built, or the port is already in use.
Check `make build` output and that `PORT` is not taken.

**Section 5/6 failures (inference)** — Ollama is not reachable or the model is not loaded.
Run `curl $OLLAMA_URL/api/tags` to verify connectivity and confirm `phi4-mini` is present.
Load with: `curl -X POST $OLLAMA_URL/api/pull -d '{"name":"phi4-mini"}'`

**Section 3 (disable/enable)** — If `/quota` does not show `offline` immediately after
disable, check that the store round-trip is flushing state synchronously. This is a known
test surface for the state machine.

**Section 4 (webhook)** — The webhook URL does not need to be reachable for CRUD tests.
Delivery is not tested in the smoke test (no listener); delivery is covered by
`internal/webhook` unit tests.

## Adding the suite to CI

The script exits 0 on full pass, 1 on any failure. Wire it as a `make smoke` target or
add it as a CI job that provisions Ollama:

```yaml
smoke:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with: { go-version: '1.25' }
    - name: Start Ollama
      run: |
        curl -fsSL https://ollama.com/install.sh | sh
        ollama pull phi4-mini
    - name: Smoke test
      run: CLAGENTIC_ROUTER_TOKEN=ci-token ./scripts/smoke-test.sh
```

## Recorded runs

| Date | Binary | Ollama | Pass | Fail | Notes |
|---|---|---|---|---|---|
| 2026-05-26 | `e6f41e2` (Phase 2+3 merge) | `phi4-mini` @ localhost | 32 | 0 | First run. Script exposed 6 response-shape mismatches (null vs empty array for `/logs`+`/webhooks`; field name casing `BackendID`, `ID`; DELETE returns 200 not 204; metrics prefix `router_*` not `clagentic_router_*`). All corrected in the script — no daemon bugs. |

Update this table after each manual or CI run. For production deployments, record the
git SHA and the pass/fail count.
