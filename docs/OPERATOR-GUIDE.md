# Operator Guide

Task-shaped documentation for running clagentic:router: install, configure,
deploy, add a backend, and diagnose a failure. This is the human-facing
manual. For the machine-readable adapter/wire contract, see
[AGENT-REFERENCE.md](AGENT-REFERENCE.md). For AWS Bedrock specifically
(both CLI-adapter and `bedrock_api` auth paths, plus the Bedrock-shaped HTTP
endpoints), see [BEDROCK.md](BEDROCK.md).

## Install and run

```bash
# 1. Build
make build

# 2. Configure
cp router.example.yaml router.yaml
$EDITOR router.yaml

# 3. Run
export CLAGENTIC_ROUTER_TOKEN=mysecret
./bin/clagentic-router serve --config router.yaml

# 4. Call it
export CLAGENTIC_ROUTER_TOKEN=mysecret
./bin/clagentic-router call --model claude-haiku --message "What is 2+2?"

# Or via any OpenAI SDK:
# base_url = "http://localhost:8765/v1"
# api_key  = "mysecret"
```

**Requirements:** Go 1.25+. No CGO — pure Go SQLite via `modernc.org/sqlite`.

```bash
make tidy     # go mod tidy
make build    # produces bin/clagentic-router
make install  # installs to GOBIN
make test     # go test ./...
make docker   # builds Docker image
```

## Which adapter family fits your deployment

Two adapter families exist and neither is "preferred" in the abstract — pick
per deployment constraint:

| Family | Adapters | Auth | Runs in a container? |
|---|---|---|---|
| CLI (subprocess, OAuth-session-backed) | `claude_cli`, `codex_cli`, `codex_subagent`, `gemini_cli` | OAuth session on the host (or AWS Bedrock env vars for `claude_cli`/`codex_subagent` — see [BEDROCK.md](BEDROCK.md)) | No — requires the host's OAuth session |
| API (HTTP, key/credential-backed) | `anthropic_api`, `openai_api`, `bedrock_api` | API key or AWS SDK credential chain | Yes |

`ollama_http` is a third, separate case: no auth at all, local or remote
Ollama server. See [AGENT-REFERENCE.md](AGENT-REFERENCE.md)'s adapter matrix
for the full per-adapter auth/env/cwd/HOME breakdown — this table is the
one-line orientation, not the full contract.

## Configuration

See [`router.example.yaml`](../router.example.yaml) for a fully annotated
example covering every config key. Key concepts:

- **Backends**: one LLM invocation path each (`adapter` + model + auth).
- **Tiers**: named groups of backends at the same capability level — scored,
  best one picked.
- **Chains**: ordered list of tiers tried in sequence on failure/exhaustion.
  Reference with `model: "role:<chain-name>"`.

### Model field syntax

| Syntax | Example | Resolves to |
|---|---|---|
| Tier alias | `claude-haiku` | All backends in the `haiku` tier, scored |
| Explicit chain | `chain:haiku,mini,sonnet` | Three-step fallback |
| Named chain | `role:default` | Chain named `default` in config |
| Direct backend | `backend:claude-haiku` | Exactly one backend, no scoring |

### Adding a backend

1. Pick an adapter type (see the family table above).
2. Add a `backends.<id>` entry in `router.yaml` — copy the closest example
   from `router.example.yaml` and adjust `model`/`cost_weight`/`timeout_seconds`.
3. Add the backend id to a tier in `tiers:` (or reference it directly via
   `backend:<id>`).
4. Restart the daemon (`clagentic-router update`, or a manual restart) and
   check `GET /v1/models` — the backend should appear with a `status`.
5. If it doesn't authenticate, see "Diagnosing a failure" below.

### quota_probe (claude_cli backends)

When the router is idle, quota utilization and reset times for `claude_cli`
backends go stale. The `quota_probe` block activates a background loop that
fires a minimal claude CLI call when no organic `rate_limit_event` has been
received within the configured window.

```yaml
backends:
  claude-low:
    adapter: claude_cli
    model: claude-haiku-4-5
    quota_probe:
      enabled: true       # false by default; must opt in
      interval: 30m       # probe if no organic data in this window (default: 30m)
      model: claude-haiku-4-5  # model to use for the probe ping (default: claude-haiku-4-5)
```

| Field | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Activate the probe loop |
| `interval` | duration string | `30m` | How long to wait without organic data before probing |
| `model` | string | `claude-haiku-4-5` | Model to use for the probe call (use the cheapest available) |

Probe calls are not recorded in `/logs` or `/stats` — they are maintenance
traffic, not routed requests. On a `rejected` status (quota exhausted), the
prober backs off to a 5-minute retry interval until it receives a
non-rejected response.

### Legacy / removed config keys

`trusted_working_dirs` (top-level) was removed — a prior version of this
daemon gated a workspace-trust-dialog pre-acceptance write behind it, but
`claude --print`/`claude -p` (the only invocation mode `claude_cli` and
`codex_subagent` use) never shows that dialog in the first place, so the key
was gating nothing real. A leftover `trusted_working_dirs` key in an
existing `router.yaml` is **ignored, not fatal** — `config.Load` logs a
`Warn` (`internal/config/config.go`'s `unknownTopLevelKeyWarnings` /
`warnRemovedTopLevelKeys`) and startup proceeds normally. Remove the key at
your convenience; there is no replacement config surface for it. See
[AGENT-REFERENCE.md](AGENT-REFERENCE.md)'s working_dir section for what
replaced the exposure this key used to (incompletely) gate.

## Deployment

### Systemd

A fully annotated sample unit is in [`deploy/clagentic-router.service`](../deploy/clagentic-router.service).

**HOME is required for `claude_cli`/`codex_subagent` backends.** The
`claude_cli` adapter resolves OAuth credentials from
`$HOME/.claude/.credentials.json`. Systemd does not set `HOME` for service
units by default. If it is unset, credential sync fails with an ERROR log
and all `claude_cli`/`codex_subagent` backends are permanently
unauthenticated. Set `HOME` to the home directory of the user the service
runs as:

```ini
[Unit]
Description=Clagentic: Router LLM routing daemon
After=network.target

[Service]
User=router
# HOME must match the User above so the claude_cli adapter can locate OAuth credentials.
# Adjust to /root, /home/ubuntu, etc. depending on which user owns the Claude session.
Environment=HOME=/home/router
ExecStart=/usr/local/bin/clagentic-router serve --config /etc/clagentic/router/router.yaml
Restart=on-failure
EnvironmentFile=/etc/clagentic/router/env

[Install]
WantedBy=multi-user.target
```

If you use only API-based adapters (`anthropic_api`, `openai_api`,
`bedrock_api`) and do not configure any `claude_cli`/`codex_subagent`
backends, this requirement does not apply.

### Redeploying: `clagentic-router update`

The router is a long-running daemon — landing a change in git does not make
it live. `clagentic-router update` rebuilds the binary from source, installs
it atomically (stage + rename, never an in-place copy over the running
binary — avoids "text file busy"), and restarts the service. It reuses the
same config file `serve` uses (no second config surface); every setting is
optional and defaults to a stock systemd install:

```yaml
deploy:
  source_dir: .                                   # module root to build from (default: cwd)
  install_path: /usr/local/bin/clagentic-router    # path the running service execs
  service_name: clagentic-router                   # systemd unit name, without .service
  service_manager: systemd                         # systemd | none (install only, no restart)
```

```bash
clagentic-router update                # uses the resolved config (see "Configuration")
clagentic-router update --config PATH   # explicit config path
```

This is the command a project's `.crew/naomi.yaml` `post_merge_steps` should
invoke as a bare, environment-agnostic verb — all host-specific detail lives
in `router.yaml`'s `deploy:` block, not in the committed post-merge step.

### Docker (API-only mode)

```bash
docker run -p 8765:8765 \
  -v /etc/clagentic/router/router.yaml:/etc/clagentic/router/router.yaml:ro \
  -e CLAGENTIC_ROUTER_TOKEN=secret \
  -e ANTHROPIC_API_KEY=sk-... \
  clagentic-router
```

CLI adapters (`claude_cli`, `codex_cli`, `codex_subagent`, `gemini_cli`)
cannot run in a container — they require the OAuth session state that lives
on the host. Configure only API-based adapters for containerized
deployments.

### Verifying a deployment: smoke test

[`docs/smoke-test.md`](smoke-test.md) is an end-to-end validation procedure
(`scripts/smoke-test.sh`) covering every HTTP endpoint, auth, inference
(non-streaming + SSE), webhook CRUD, backend control, and the call log
against a live daemon. Run it after any merged PR touching server/router/
backend, before tagging a release, and after deploying a new binary.

## Diagnosing a failure

| Symptom | Likely cause | Where to look |
|---|---|---|
| `claude_cli`/`codex_subagent` backend permanently `offline`, log shows OAuth session errors | `HOME` unset in the service environment, or stale/expired OAuth credentials | Systemd section above; re-run `claude auth login` on the host |
| `claude_cli` Bedrock-fronted backend hangs indefinitely | Missing `CLAUDE_CODE_USE_BEDROCK` or missing SSO cache mirror | [BEDROCK.md](BEDROCK.md)'s CLI-adapter section |
| `bedrock_api` backend fails at startup | `region` unset — Bedrock has no SDK default region | [BEDROCK.md](BEDROCK.md)'s `bedrock_api` section |
| `422 no_tool_capable_backend` | Request carried `tools` and the resolved chain has no tool-capable backend | [AGENT-REFERENCE.md](AGENT-REFERENCE.md)'s adapter capabilities section — every adapter today declares `supports_tools: false` |
| `GET /doctor` shows a backend `unknown` | No probe or organic traffic yet for that backend | `GET /health` (cached) vs `GET /doctor` (live probe) — see [AGENT-REFERENCE.md](AGENT-REFERENCE.md)'s API table |
| Backend `openai_api` quota shows only soft/header-based limits, never account-level usage | No admin-scoped `openai_api_key` configured, or your key is a standard `sk-proj-...` project key | See "OpenAI usage API" below |
| Webhook not firing | Event not registered for that endpoint, or delivery exhausted retries | `GET /webhooks` to check registration; delivery logs |

### OpenAI usage API note

`backend.UsagePoller` calls `/v1/dashboard/billing/subscription` and
`/v1/dashboard/billing/usage`. These are legacy endpoints that require an
**account-level admin API key** with "View usage" permission — not a
standard `sk-proj-...` project key. Standard project keys return 401; the
poller backs off silently and logs a warning. If you do not have such a key,
omit `openai_api_key` from the backend config and quota will be tracked
only via response headers (soft limits).

## Logging

Every HTTP request is logged at `Info` level with `method`, `path`,
`status`, `latency_ms`, and `request_id`. 5xx responses are logged at
`Warn`. Backend state changes are logged at `Warn`. Verbose adapter traces
are at `Debug`.

Every routed call is persisted to the `call_log` SQLite table with:
`backend_id`, `model`, `outcome`, `prompt_tokens_est`,
`completion_tokens_est`, `latency_ms`, `cost_usd_est`, `score` (router score
at selection time), `request_id` (correlates to HTTP logs),
`rate_limit_type` (active quota bucket), `utilization` (account utilization
at routing time, if known), and `fallback_count` (backends tried before
this hop). Query via `GET /logs`.

Quota events from `claude_cli` are additionally persisted to
`quota_snapshots` with full `rate_limit_info` payload including `status`,
`utilization`, `resets_at`, `surpassed_threshold`, and raw JSON for forward
compatibility.

Configure log level and format in `router.yaml`:

```yaml
log:
  level: info    # debug|info|warn|error
  format: text   # text|json
```

Or at runtime via environment variables (override config):
- `CLAGENTIC_ROUTER_LOG_LEVEL=debug`
- `CLAGENTIC_ROUTER_LOG_FORMAT=json`

Use `format: json` for structured log ingestion (Loki, CloudWatch, Datadog).

## Support

If clagentic:router is useful to you: [ko-fi.com/clagentic](https://ko-fi.com/clagentic)
