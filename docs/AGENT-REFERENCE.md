# Agent Reference

Contract-shaped reference for a caller (human or agent) integrating against
the running clagentic:router daemon: adapter capability matrix, wire-field
semantics, error taxonomy, and invariants a caller must not violate.

## Boundary with CLAUDE.md (read this first)

Two documents carry agent-relevant contract in this repo and they are
**not the same contract**:

- **`CLAUDE.md`** (repo root) is the **build-time contract for whoever is
  editing this repo's Go source** — the breadth principle, the import
  graph, discovery-vs-hardcode rules, the subprocess `cwd`/`HOME` contract
  from the implementer's side. It is read by an agent (or human) about to
  change adapter code.
- **This file** is the **runtime/wire contract for whoever is calling the
  already-built daemon** over HTTP — request/response shape, which
  adapters support what, error codes, auth headers. It is read by an agent
  (or human) integrating a client against `/v1/chat/completions` or
  `/v1/messages`, who never opens a `.go` file.

Where the two would otherwise say the same thing (e.g. the `working_dir`
exposure and the fix that closes it), **this file states the wire-visible
consequence only** (what a caller must send, what happens if they don't)
and **cross-links to CLAUDE.md for the implementation rationale** rather
than re-deriving it — duplicating the reasoning in both places would leave
two copies to keep in sync, and a drifted contract is worse than a missing
one. If you are choosing which file to update for a given change: changing
what the daemon does internally → `CLAUDE.md`; changing what a caller must
send or can expect → this file.

## API surface

| Method | Path | Description |
|---|---|---|
| POST | /v1/chat/completions | OpenAI-compatible inference |
| POST | /v1/messages | Anthropic Messages API — passthrough or `role:*` routed |
| POST | /model/{modelId}/invoke | AWS Bedrock Runtime InvokeModel — passthrough or `role:*` routed |
| POST | /model/{modelId}/invoke-with-response-stream | AWS Bedrock Runtime InvokeModelWithResponseStream — passthrough or `role:*` routed |
| GET | /v1/models | List all backends with status and capabilities |
| GET | /v1/capacity | Per-backend capacity snapshot (utilization, reset time, score) |
| GET | /health | Aggregated health (cached) |
| GET | /doctor | Live probe of all backends |
| GET | /quota | Per-backend quota and rate state |
| GET | /metrics | Prometheus text format |
| GET | `<cache_metrics.path>` | Prometheus text format per-model cache-token aggregates — opt-in, disabled and unregistered by default (`cache_metrics.enabled: false`); default path `/metrics/cache` when enabled. See "Cache token metrics" below. |
| GET | /logs | Recent call log (`?from=RFC3339&to=RFC3339`) |
| GET | /stats | Aggregated call statistics |
| POST | /backends/{id}/reset | Clear error state, re-probe |
| POST | /backends/{id}/disable | Force backend offline |
| POST | /backends/{id}/enable | Re-enable a disabled backend |
| POST | /webhooks | Register a webhook |
| DELETE | /webhooks/{id} | Remove a webhook |
| GET | /webhooks | List webhooks |
| GET | /version | Version (no auth required) |

All endpoints except `/version` require `Authorization: Bearer <token>` —
with two exceptions: `/v1/messages` and
`/model/{modelId}/invoke[-with-response-stream]` in passthrough mode (see
below).

Every `/v1/chat/completions` response includes:

```
X-Router-Backend: claude-haiku
X-Router-Chain-Position: 0
X-Router-Latency-Ms: 1234
X-Router-Fallback-Reason: rate_limit   # only when chain was advanced
```

## Adapter capability matrix

`GET /v1/models` includes a `capabilities` object per backend so a caller
can check tool/streaming/image support **before** sending a request:

```json
{
  "id": "claude-api",
  "capabilities": {
    "supports_tools": false,
    "supports_streaming": false,
    "supports_images": false
  }
}
```

**Every adapter today declares `supports_tools: false` and
`supports_images: false`** — not because the underlying provider APIs lack
tool/vision support, but because none of this router's adapters currently
marshal a `tools` field or a non-text content block on the request, or
parse one back out of the response. `Capabilities()` is the honest,
queryable signal of that state, not a router-level translation flag.

**Tool-bearing requests are refused, never silently degraded.** If a
routed request's `tools` field is present and the resolved chain has no
tool-capable backend, the router returns `422` (`no_tool_capable_backend`
on `/v1/chat/completions` and `/v1/messages`; `{"message": "..."}` Bedrock
error envelope on the Bedrock InvokeModel routes) rather than a `200` with
tools silently dropped. Use passthrough mode (`/v1/messages`,
`/model/{modelId}/invoke*`) for tool-using clients — it forwards `tools`
untouched.

### Per-adapter auth / env / cwd / HOME

| Adapter | Family | Auth | Env into subprocess | `cmd.Dir` (`working_dir`) | HOME override |
|---|---|---|---|---|---|
| `claude_cli` | CLI (subprocess) | OAuth (`~/.claude/.credentials.json`) or AWS Bedrock (`CLAUDE_CODE_USE_BEDROCK=1` + SDK credential chain) — see [BEDROCK.md](BEDROCK.md) | `buildCLIEnv` allowlist (`internal/backend/env.go`) | Yes — defaults to `/` when `working_dir` omitted; validated absolute path otherwise | Yes — `claudeSubprocessHome`, isolated stub `~/.claude` |
| `codex_subagent` | CLI (subprocess) | Same as `claude_cli` — invokes the same `claude` binary via `-p --agent codex` through the same env/HOME construction | `buildCLIEnv` allowlist | Yes — same default/validation as `claude_cli` | Yes — same `claudeSubprocessHome` |
| `codex_cli` | CLI (subprocess) | OAuth (`~/.codex/auth.json`) or AWS Bedrock (`model_provider = amazon-bedrock` in `~/.codex/config.toml`, **not** an env-var switch — see [BEDROCK.md](BEDROCK.md)) | `buildCLIEnv` allowlist | Yes — defaults to `/` when omitted | **No** — reads the daemon's real `HOME` directly |
| `gemini_cli` | CLI (subprocess) | OAuth (`gemini auth login`) or `GEMINI_API_KEY` (set via `router.yaml` extra-env, not daemon-env inheritance — see `env.go`) | `buildCLIEnv` allowlist plus `NO_COLOR=1` | Yes — defaults to `/` when omitted | **No** — reads the daemon's real `HOME` directly |
| `anthropic_api` | API (HTTP) | API key (`api_key` config, `env:VAR` supported) | N/A — no subprocess | Ignored — no subprocess | N/A |
| `openai_api` | API (HTTP) | API key (`api_key` config); optional `openai_api_key` (admin-scoped) enables usage polling | N/A — no subprocess | Ignored — no subprocess | N/A |
| `bedrock_api` | API (HTTP) | AWS SDK credential chain via `config.LoadDefaultConfig` (env vars → web identity → shared files → ECS → IMDS); `region` required | N/A — no subprocess | Ignored — no subprocess | N/A |
| `ollama_http` | HTTP (local) | None | N/A — no subprocess | Ignored — no subprocess | N/A |

**The HOME-override asymmetry is real and deliberate, not an oversight.**
`claude_cli`/`codex_subagent` isolate `HOME` into a stub directory with no
hook-bearing `settings.json` — a second, `cwd`-independent suppression
layer on top of `cmd.Dir`. `codex_cli`/`gemini_cli` set no HOME override at
all, so `cmd.Dir` is their *only* isolation layer. See CLAUDE.md's
"Subprocess cwd contract" for the full per-adapter rationale and why
`cmd.Dir` defaulting to `/` still strengthens `codex_cli`/`gemini_cli` even
without a matching HOME override.

### Cache token metrics

`GET <cache_metrics.path>` (default `/metrics/cache`, opt-in via
`cache_metrics.enabled: true` — see `router.example.yaml`) exposes
per-`(backend, model)` prompt-cache token aggregates in Prometheus text
format, captured at each adapter's `Invoke` boundary:

```
router_cache_input_tokens_total{backend="claude-opus",model="claude-opus-4-8"} 15234
router_cache_read_tokens_total{backend="claude-opus",model="claude-opus-4-8"} 9821
router_cache_write_tokens_total{backend="claude-opus",model="claude-opus-4-8"} 2100
router_cache_calls_reported{backend="claude-opus",model="claude-opus-4-8"} 42
router_cache_calls_unsupported{backend="claude-opus",model="claude-opus-4-8"} 0
```

**Per-adapter-family breadth (checked first, before any storage design) —
one family per row, no silent gaps:**

| Adapter | Reports cache tokens? | Basis |
|---|---|---|
| `anthropic_api` | Yes — input, cache_read, cache_write | Messages API `usage.cache_creation_input_tokens`/`cache_read_input_tokens`, documented public fields |
| `bedrock_api` | Yes — input, cache_read, cache_write | `types.TokenUsage.CacheReadInputTokens`/`CacheWriteInputTokens` — confirmed via a live reflection probe against the exact vendored SDK version pinned in `go.mod`, not inferred from docs |
| `openai_api` | Yes — input, cache_read only | Chat Completions `usage.prompt_tokens_details.cached_tokens`; OpenAI's cache has no write-side concept to report, so `cache_write` is a real, documented `0`, not "unsupported" |
| `codex_cli` | **Not yet wired — verified present, deliberately deferred** | `codex exec --json`'s `turn.completed` event carries `input_tokens`/`cached_input_tokens`/`cache_write_input_tokens` (live-captured, codex-cli 0.147.0). This adapter does not pass `--json` today and changing its invocation/parse/error-classification contract for a production, load-bearing path is out of scope for this addition — see `codex_cli.go`'s package doc for the full reasoning and the `TODO(lr-718af0)` follow-up. |
| `claude_cli` | **Unverified — honest gap, not a guess** | The CLI's stream-json `result` line may carry a `usage`/cache object on some versions, but no permitted live-verification path existed to confirm the shape this adapter actually parses; a speculative field was deliberately not added (see CLAUDE.md's provider-verification rule and `claude_cli.go`'s doc) |
| `codex_subagent` | **Unverified — same gap as `claude_cli`** | Invokes the identical `claude` binary/output shape as `claude_cli`; same disposition, same follow-up |
| `gemini_cli` | **No — verified, documented no-op** | `gemini --output-format json`'s `stats.models.<model>.tokens` block has only `input`/`candidates`/`total` — no cache field of any kind, confirmed live in this package's own prior quota-signal investigation (`gemini_cli.go`) |
| `ollama_http` | **No — documented no-op** | Ollama's `/api/chat` has no prompt-cache concept in its API at all |

**A `nil` field, never a fabricated `0`, marks "cannot report".** A
`(backend, model)` pair only accumulates `calls_unsupported` when its
adapter has no cache data for that call; `calls_reported` and the token
totals track only real reported data, including a genuine zero-token cache
miss. A consumer of this endpoint MUST check `calls_reported > 0` before
treating a `0` hit-rate as a real miss rather than "no data yet" — the same
distinction the underlying Go types (`backend.CacheUsage`,
`store.CacheUsageRow.HitRate`) enforce at the code level.

**Counts/aggregates only.** No prompt content, request body, or response
text is ever captured or persisted for this feature — only integer token
counts and call counters, the same boundary the `call_log.tools_present`
bit draws.

### `working_dir` wire field

Both `/v1/messages` (routed mode only) and `/v1/chat/completions` accept an
optional `working_dir` field: an absolute path the four CLI adapters use as
the subprocess's working directory.

- **Opt-in, never inferred.** Omitted → all four default to `/`
  (`backend.DefaultWorkingDir`). The router never guesses a directory from
  server-side state (daemon cwd, HOME, etc.) — it is a shared daemon
  serving arbitrary callers.
- **Validated at the wire boundary.** A supplied value is rejected with
  `400` (`invalid_request_error` on `/v1/messages`, `invalid_request` on
  `/v1/chat/completions`) unless it is absolute, exists, and is a
  directory. `backend.ResolveWorkingDir` performs this check but is **not**
  a containment control: no path-containment allowlist, and a TOCTOU
  window exists between the existence check and the subprocess actually
  starting. Both are known, accepted limitations — not gaps this
  validation claims to close.
- **`claude_cli`/`codex_subagent` also pass `--setting-sources user`,
  unconditionally, on every invocation.** This is the caller-visible
  consequence: a `working_dir` pointing at a real project will NOT get
  that project's `CLAUDE.md`, hooks, or `.claude/settings.json`
  `permissions.allow` applied in the subprocess — only the daemon's own
  user-scope Claude Code settings apply, for every caller. This closes an
  exposure where an arbitrary caller-chosen directory's config would
  otherwise execute inside the daemon's process (no workspace-trust dialog
  gates it in `claude --print`/`claude -p` mode). Empirically verified
  (not merely mechanism-expected) for exactly three surfaces:
  `permissions.allow`, hooks, and `CLAUDE.md` auto-discovery — see
  `docs/setting-sources-verification-run.txt` for the committed run
  (claude CLI 2.1.232) and `make verify-safe-mode` to reproduce against
  your own CLI version. Project-scope MCP servers and subagent
  definitions are **expected** to be excluded by the same
  source-exclusion mechanism but were **not separately measured** — treat
  that distinction as load-bearing if you cite this claim elsewhere. Full
  rationale, including the `--safe-mode`-was-insufficient finding and the
  capability trade-off (a caller loses project context in exchange for
  this closure), lives in CLAUDE.md's "Subprocess cwd contract" section —
  not duplicated here.
- **`codex_cli`/`gemini_cli` have no equivalent flag.** `--setting-sources`
  is claude-specific. `codex_cli` has no known analogous
  hooks/settings/CLAUDE.md auto-discovery surface. `gemini_cli`'s
  behavior versus GHSA-wpqr-6v78-jr5g depends on the installed `gemini`
  binary version — not pinned or vendored by this daemon.

## `/v1/messages` (Anthropic Messages API)

Selected by the request's `model` field:

| Model field | Mode | Behavior |
|---|---|---|
| Any normal Claude model (`claude-sonnet-4-6`, etc.) | **Passthrough** (default) | Transparent reverse proxy to `anthropic.upstream_url` (default `https://api.anthropic.com`). Request body and streamed SSE forwarded byte-for-byte. |
| `role:<chain>` / `chain:a,b,c` / `backend:<id>` | **Routed** | Translated to the router's internal format, sent through the same fallback-chain machinery as `/v1/chat/completions`, translated back (including Anthropic-grammar SSE when `stream: true`). |

**Auth matrix (deliberately asymmetric):**

- **Passthrough**: the router does **not** check its own inference token.
  The client's own Anthropic credential (`x-api-key` or
  `Authorization: Bearer`) travels through to upstream Anthropic unchanged.
  Set `anthropic.upstream_api_key` to substitute a router-owned key for
  every passthrough request instead.
- **Routed**: requires the router's own inference token, presented as
  `x-api-key: <token>` OR `Authorization: Bearer <token>` — gated exactly
  like `/v1/chat/completions`.

**Known limitation, routed mode only:** no true token streaming (CLI
adapters are request/response); no adapter round-trips
`tool_use`/`tool_result` content on a routed multi-turn call. Suitable for
one-shot review/audit roles only. Passthrough has none of these
limitations.

## Bedrock InvokeModel API (`/model/{modelId}/invoke[-with-response-stream]`)

Full writeup, including the AWS event-stream framing detail and the
routed-vs-passthrough auth matrix, is in [BEDROCK.md](BEDROCK.md) — this is
a caller-facing summary only:

- The model identifier is carried **entirely by the URL path segment**
  (`{modelId}`), never the request body.
- A real Bedrock model/inference-profile ID → SigV4-signed passthrough to
  real AWS Bedrock. `role:*`/`chain:*`/`backend:*` → routed through the
  same fallback machinery as the other endpoints.
- Streaming variant emits AWS event-stream framing
  (`application/vnd.amazon.eventstream`) — distinct from both plain SSE and
  Anthropic-grammar SSE.
- Same tool-refusal behavior as `/v1/messages` routed mode.

## Error taxonomy

All adapter errors are `*backend.InvokeError` with a `Type` field. The
router maps `backend.ErrorType` → `state.ErrorType` for state machine
transitions — **the string values are identical by design**, to avoid
mapping bugs between the two enums.

| ErrorType | Meaning |
|---|---|
| `quota` | Hard limit — daily/monthly credit exhausted |
| `rate_limit` | Soft limit — requests-per-window exceeded |
| `auth` | Authentication/authorization failure (also covers "model not enabled for account" on Bedrock — see below) |
| `network` | Retriable downstream/connectivity failure |
| `timeout` | Call exceeded its configured timeout |
| `not_found` | Binary not found on PATH, or resource not found |
| `schema` | Malformed/empty response the adapter could not parse |
| `unknown` | Unclassified failure |

`bedrock_api`'s Bedrock Runtime SDK exceptions map onto this enum via
`errors.As` (never string matching): `ThrottlingException`/
`ModelNotReadyException` → `rate_limit`; `AccessDeniedException` → `auth`
(this one exception type covers **both** missing/expired credentials
**and** a model not enabled for your account in that region — the SDK does
not distinguish them); `ValidationException`/`ResourceNotFoundException` →
`schema`; `ModelTimeoutException` → `timeout`;
`ModelErrorException`/`InternalServerException`/`ServiceUnavailableException`
→ `network`.

### Chain-exhaustion response (`503 backends_unavailable`)

When every backend in a routed chain fails, `/v1/chat/completions` and
`/v1/messages` (routed mode) both return `503`, never `502` — `502` is
reserved for a distinct, non-exhaustion failure branch. The body carries an
optional `last_error_type` field: the `ErrorType` (table above) of the
**last** backend to fail before the chain gave up, or omitted entirely when
unknown. This reuses the exact same type-only enum `GET /v1/models`
publishes per backend as `last_error_type` — never raw error text, never a
backend id, never any AWS profile/account/token/config-path detail.

OpenAI-compatible shape:

```json
{"error": {"code": "backends_unavailable", "message": "no available backends in chain", "last_error_type": "auth"}}
```

Anthropic-compatible shape:

```json
{"type": "error", "error": {"type": "overloaded_error", "message": "no available backends in chain", "last_error_type": "auth"}}
```

An agent polling for chain health can distinguish "every backend is out of
quota" from "every backend needs re-auth" from this field alone, without a
second call to `/v1/models`.

## Routing invariants an agent must not assume around

- **`router.Score()` is pure and deterministic** — same snapshot/config/
  token-estimate in, same score out, no randomness. Near-tie breaking
  (within 5% of the best score) uses `math/rand/v2` in `selectBest`, a
  separate function — an agent scripting against repeated identical
  requests should expect occasional different-backend selection among
  near-tied backends, not a bug.
- **Latency EMA** (alpha=0.3, ~3-call half-life) is maintained per-backend.
  `Score` applies a soft, inverse-proportional penalty once the EMA exceeds
  `routing.latency_penalty_threshold_ms` (default 15000ms): 2x threshold →
  x0.5, 4x → x0.25.
- **Quota alerts are edge-triggered, not level-triggered.**
  `state.BackendState.TestAndSetQuotaLow(threshold)` fires `quota_low`
  exactly once per threshold crossing, not on every request below
  threshold. An agent polling `/quota` should not expect a fresh webhook
  per poll while a backend stays low — only on the crossing.
- **Webhook events**: `backend_offline`, `backend_degraded`,
  `backend_recovered`, `quota_exhausted`, `quota_low`, `auth_failure`,
  `chain_exhausted`. Delivery is HMAC-signed (`X-Clagentic-Signature:
  sha256=<hmac>` when `secret` configured), with exponential backoff
  (default 5 retries, 500ms initial) and drop-after-exhaustion — a missed
  webhook is not retried forever.
- **Discovery is additive, explicit config always wins, byte-identically.**
  `codex_discovery.go` (provider id) and `codex_model_discovery.go` (model
  slug) only run when the corresponding config field is empty; a
  caller/operator-supplied value is never second-guessed or re-validated
  against discovery. `openai_project_id` has **no discovery path at all** —
  unset means the `OpenAI-Project` header is simply not injected. Do not
  assume a discovery endpoint exists for it; `lr-698965` reverted a
  fabricated one that was never actually invoked.

## See also

- [BEDROCK.md](BEDROCK.md) — every AWS Bedrock auth/config/endpoint path in
  one place (CLI-adapter Bedrock auth, `bedrock_api`, Bedrock InvokeModel
  HTTP endpoints).
- [OPERATOR-GUIDE.md](OPERATOR-GUIDE.md) — install, configure, deploy,
  diagnose.
- `router.example.yaml` — every config key, annotated.
- `CLAUDE.md` — build-time contract for editing this repo's Go source.
