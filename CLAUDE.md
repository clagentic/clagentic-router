# clagentic-router

LLM routing daemon for the clagentic brand. Routes requests across multiple LLM backends
with fallback chains, quota tracking, and an OpenAI-compatible HTTP API.

## Goal

A single HTTP daemon that:
- Accepts OpenAI-compatible `/v1/chat/completions` requests from any client
- Routes each request across a scored, configurable set of backends
- Walks a fallback chain when a backend is degraded, rate-limited, or quota-exhausted
- Tracks per-backend health state persistently in SQLite
- Emits webhook alerts on state changes (offline, quota_low, auth_failure, etc.)
- Runs on any Linux host; CLI adapters require OAuth sessions on that host; API adapters work anywhere

## Build

```bash
make tidy      # go mod tidy
make build     # produces bin/clagentic-router
make test      # go test ./...
```

## Adapter preference

CLI adapters (`claude_cli`, `codex_cli`, `codex_subagent`, `gemini_cli`) use OAuth
sessions on the host and require no API keys. They are the recommended path for
local deployments. API adapters (`anthropic_api`, `openai_api`) require keys and
work in containerised or keyless environments.

## Import graph (no cycles allowed)

```
config  → (stdlib)
state   → (stdlib)
store   → state
backend → config
webhook → state, store
router  → backend, config, state, store, webhook
server  → router, state, store
cmd/clagentic-router → config, backend, router, server, store, webhook
```

Adding an import that creates a cycle is a hard error.
`webhook` must never import `router`. `backend` must never import `store` or `state`.

## Error classification

All adapter errors are `*backend.InvokeError` with a `Type` field.
The router maps `backend.ErrorType` → `state.ErrorType` for state machine transitions.
The string values are identical to avoid mapping bugs.

## Scoring

`router.Score()` is **pure and deterministic** — no randomness. It returns a float64 for
one backend given its snapshot, config, and the estimated request token count.

Near-tie breaking (within 5% of the best score) uses `math/rand/v2` in `selectBest`, not
in `Score`. This keeps tests deterministic while avoiding thundering-herd on identical backends.

Latency EMA (alpha=0.3) is maintained per-backend in state. Score applies a soft penalty
when EMA exceeds `routing.latency_penalty_threshold_ms` (default 15 000 ms).

## Quota alerts (edge-triggered)

`state.BackendState.TestAndSetQuotaLow(threshold)` fires `quota_low` only once per
threshold crossing, not on every request. `QuotaLowAlerted bool` in state tracks the edge.
Clear fires when quota recovers above threshold.

## Testing

Unit tests go in `_test.go` files next to the code they test.
Integration tests that invoke real LLM backends go in `internal/backend/integration_test.go`
and are guarded with `testing.Short()` skips.

## OpenAI usage API note

`backend.UsagePoller` calls `/v1/dashboard/billing/subscription` and
`/v1/dashboard/billing/usage`. These are legacy endpoints that require an **account-level
admin API key** with "View usage" permission — not a standard `sk-proj-...` project key.
Standard project keys return 401; the poller backs off silently and logs a warning.
If you do not have such a key, omit `openai_api_key` from the backend config and quota
will be tracked only via response headers (soft limits).

## CLI Naming

Binary names, env vars, syslog identifiers, and config paths follow the
clagentic CLI Naming Standard. Violations are a review blocker.
