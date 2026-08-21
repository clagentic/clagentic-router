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

### Systemd, user scope

A single-operator workstation — where the OAuth session `claude_cli` needs
(`$HOME/.claude/.credentials.json`) and, for Bedrock, `~/.aws/sso/cache`
both belong to the one human running the router — is a natural fit for a
`systemd --user` unit instead of a system-scope service. A fully annotated
sample is in
[`deploy/clagentic-router.user.service`](../deploy/clagentic-router.user.service).
It uses `%h`/`%S` systemd specifiers throughout (your home directory / your
state directory root) — copy it unmodified, nothing in it needs editing for
your username or host.

```bash
# 1. Build and install the binary for your own user
make build
mkdir -p ~/.local/bin
cp bin/clagentic-router ~/.local/bin/clagentic-router

# 2. Configure
mkdir -p ~/.config/clagentic/router
cp router.example.yaml ~/.config/clagentic/router/router.yaml
$EDITOR ~/.config/clagentic/router/router.yaml
# Set deploy.service_manager: systemd-user (see "Redeploying" below)

# 3. Secrets file referenced by the unit's EnvironmentFile=
mkdir -p ~/.config/clagentic/router
printf 'CLAGENTIC_ROUTER_TOKEN=mysecret\n' > ~/.config/clagentic/router/env
chmod 0600 ~/.config/clagentic/router/env

# 4. Install the unit
mkdir -p ~/.config/systemd/user
cp deploy/clagentic-router.user.service ~/.config/systemd/user/clagentic-router.service
systemctl --user daemon-reload
systemctl --user enable --now clagentic-router

# 5. Keep it running after you log out (optional but usually wanted)
loginctl enable-linger "$USER"

# 6. Call it
export CLAGENTIC_ROUTER_TOKEN=mysecret
clagentic-router call --model claude-haiku --message "What is 2+2?"
```

Every path in this sequence is either a fixed relative layout under `~`
(`~/.local/bin`, `~/.config/clagentic/router/`,
`~/.config/systemd/user/`) or a value you already set in step 2/3 — nothing
here is invented by the operator that this guide does not name.

Two adaptations from the system-scope unit, both explained in comments in
the shipped user template itself:

- **No `Environment=HOME=...` line is needed.** The system-scope unit above
  requires it because a system-scope systemd unit does not set `HOME` by
  default; a `systemd --user` unit is started by the per-user systemd
  manager instance, which does set `HOME` from your own passwd entry.
- **`CLAGENTIC_ROUTER_STATE_DIR` is set explicitly**, even though the unit
  also declares `StateDirectory=clagentic-router`. `StateDirectory=` only
  creates the directory and exports `$STATE_DIRECTORY` to the unit — the
  router never reads that variable (it reads `CLAGENTIC_ROUTER_STATE_DIR`
  and `$XDG_STATE_HOME`/`storage.db_path` instead). The compiled fallback
  default for part of that state (the `claude_cli` subprocess-home root) is
  `/var/lib/clagentic-router`, which is wrong and unwritable at user scope,
  so the template sets `CLAGENTIC_ROUTER_STATE_DIR=%S/clagentic-router`
  explicitly rather than relying on it being inferred.
- **`PATH` includes `%h/.local/bin`.** A `systemd --user` unit inherits a
  minimal `PATH` (typically `/usr/bin:/bin`) that does not include
  `~/.local/bin`, where the `claude` CLI (and other user-installed tools)
  commonly live. Without this, the daemon starts and reports `active
  (running)` and `GET /health` `ok` while every `claude_cli`-backed chain is
  silently unusable — see "Diagnosing a failure" below.

### Redeploying: `clagentic-router update`

The router is a long-running daemon — landing a change in git does not make
it live. `clagentic-router update` maintains a git checkout, rebuilds the
binary from it, installs the result atomically (stage + rename, never an
in-place copy over the running binary — avoids "text file busy"), and
restarts the service. It reuses the same config file `serve` uses (no
second config surface); every setting is optional and defaults to a stock
systemd install:

```yaml
deploy:
  source_dir: ""                                   # default: a managed checkout (see below); set explicitly to opt out
  repo_url: ""                                      # git remote to clone the managed checkout from, if it doesn't already exist
  install_path: /usr/local/bin/clagentic-router    # path the running service execs
  service_name: clagentic-router                   # systemd unit name, without .service
  service_manager: systemd                         # systemd | systemd-user | none (install only, no restart)
```

For the user-scope deployment above, set `install_path` to
`%h/.local/bin/clagentic-router` expanded for your own home (e.g.
`/home/you/.local/bin/clagentic-router` — `deploy.install_path` is plain
Go/YAML config, not a systemd unit file, so it does not expand `%h`
itself) and `service_manager: systemd-user`; `update` then restarts via
`systemctl --user restart` instead of the system-scope `systemctl restart`
the default `systemd` value uses. `systemd` and `none` behavior is
unchanged by the addition of `systemd-user`.

```bash
clagentic-router update                             # uses the resolved config (see "Configuration")
clagentic-router update --config PATH                # explicit config path
clagentic-router update --source-dir /path/to/checkout   # override deploy.source_dir for this run only
```

#### Where does the source come from? (lr-720e91)

`update` never builds from its own working directory by default — a
deployed host (systemd unit, cron, or an operator running `update` from an
arbitrary shell) has no reason to have a source tree in cwd, and building
from whatever happened to be there was the actual bug this section used to
paper over.

- **Default: a managed checkout.** `deploy.source_dir` unset resolves to
  `$XDG_DATA_HOME/clagentic-router/src` (falls back to
  `~/.local/share/clagentic-router/src`). `update` owns this checkout's git
  state:
  - **Missing** — cloned from `deploy.repo_url` if set. If `repo_url` is
    unset, `update` refuses with an error naming exactly this: set
    `deploy.repo_url`, or pre-create the checkout yourself at that path
    (`git clone <remote> <path>`), or point `source_dir` elsewhere.
  - **Present** — identity-checked, then `git pull --ff-only`. Before
    pulling, `update` reads the checkout's own `origin` remote and compares
    it against `deploy.repo_url` (tolerating equivalent forms: `.git`
    suffix, trailing slash, scp-style SSH vs. an explicit-scheme URL). A
    mismatch is a hard error naming both URLs — a managed checkout is never
    silently pulled and built if it turns out to be a checkout of a
    different repository (or a fork, or a stale/pre-seeded directory). No
    `origin` remote at all is also a hard error when `repo_url` is set
    (identity cannot be verified, so it is not assumed fine); when
    `repo_url` is also unset there is nothing to check against, so `update`
    warns and proceeds rather than failing — see
    `ensureSourceCheckout`/`verifyOriginMatchesRepoURL` in
    `cmd/clagentic-router/update.go` for the full decision rationale. Once
    identity-checked, non-fast-forwardable pull state (local commits,
    diverged history) still fails loudly; `update` never merges or resets a
    checkout out from under you.
  - **Present but not a git repo** (no `.git`) — hard error naming the path,
    rather than attempting to build whatever happens to be sitting there.
- **Explicit `source_dir`/`--source-dir` always wins**, byte-identically,
  and `update` never touches that directory's git state — it is assumed to
  already reflect the desired revision, matching the pre-lr-720e91
  contract exactly. This is the mechanism a post-merge automation step
  uses: it passes `--source-dir .` so it keeps building from the
  already-merged tree at its own cwd (see `.clagentic/loadout/config.yaml`'s
  `post_merge_steps` in this repo for the concrete example) — no
  repo-committed `router.yaml` override needed.
- **Missing Go toolchain** on the host is detected before the build is
  attempted and reported with an actionable message (install Go, or run
  `update` from a host that has it), rather than surfacing as an opaque
  `exec: "go": executable file not found in $PATH`.

This is the command a project's `.crew/naomi.yaml` `post_merge_steps` should
invoke — with an explicit `--source-dir` for the merged-tree case above —
so all *other* host-specific detail (install path, unit name, checkout
location for the deployed-host case) lives in `router.yaml`'s `deploy:`
block, not in the committed post-merge step.

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
| `GET /health` reports `status` other than `"ok"` and lists a backend in `unresolved_binaries` | That backend's CLI binary (claude/codex/gemini) could not be found on `PATH`/`extraBinDirs` at startup — an ERROR-level `binary not found at startup` log line was emitted when the daemon started | Install the binary, or set the adapter's `bin_path` / the matching `*_BIN` env var; restart the daemon |
| `clagentic-router doctor`/`health`/`quota`/etc. (run from an operator's own shell) returns `401` | The daemon's token lives only in its systemd `EnvironmentFile` (not sourced into an interactive shell) and none of `--token`/`--token-file`/`CLAGENTIC_ROUTER_TOKEN` resolved a value either | The 401 error itself names every source checked, including the exact env-file path; see "Client token resolution" below |
| Backend `openai_api` quota shows only soft/header-based limits, never account-level usage | No admin-scoped `openai_api_key` configured, or your key is a standard `sk-proj-...` project key | See "OpenAI usage API" below |
| Webhook not firing | Event not registered for that endpoint, or delivery exhausted retries | `GET /webhooks` to check registration; delivery logs |
| `systemd --user` deployment: service is `active (running)`, `GET /health` is `ok`, but a `claude_cli` backend never responds and logs `binary not found at startup name=claude` at `WARN` | A `systemd --user` unit inherits a minimal `PATH` lacking `~/.local/bin`, where `claude` installs | "Systemd, user scope" above — the shipped user template sets `PATH` including `%h/.local/bin`; if you wrote your own unit instead of copying the template, add that |
| `update` reports `restart: systemctl restart ... failed` (note: NO `--user` in the command text) on a host that is actually running the unit at user scope | `deploy.service_manager` is still `systemd` (system scope) even though the unit was installed under `~/.config/systemd/user/` — `update` is issuing a system-scope `systemctl restart`, which cannot see a user-scope unit at all | Set `deploy.service_manager: systemd-user` in `router.yaml` |
| `update` reports `restart: systemctl --user restart ... failed` (command text DOES include `--user`) | `deploy.service_manager` is correctly `systemd-user`, but the per-user systemd manager instance isn't reachable (e.g. no active login session and `loginctl enable-linger` not set) | Run `loginctl enable-linger "$USER"`; confirm with `systemctl --user status` |

### Client token resolution (lr-92ee18 B3)

`health`, `doctor`, `quota`, `metrics`, `logs`, `call`, and `backend`
subcommands are thin HTTP clients — they need the same bearer token the
daemon itself checks incoming requests against. That token normally lives
**only** in the daemon's `EnvironmentFile` (`/etc/clagentic/router/env` by
default — see the systemd unit above), which systemd loads into the
service's environment but which is never sourced into an operator's own
interactive shell. Before this was fixed, that meant the single best
diagnostic surface for a broken deployment — `clagentic-router doctor` — was
effectively unusable from a normal shell: it always 401ed.

Resolution order, first non-empty value wins:

1. `--token TOKEN` / `-t TOKEN`
2. `--token-file PATH` (file contents, trimmed)
3. `CLAGENTIC_ROUTER_TOKEN` or `CLAGENTIC_ROUTER_ADMIN_TOKEN` in the
   caller's own shell environment
4. The deployment's `EnvironmentFile` — default
   `/etc/clagentic/router/env`, matching
   [`deploy/clagentic-router.service`](../deploy/clagentic-router.service);
   override the path with `CLAGENTIC_ROUTER_ENV_FILE`

Step 4 is what makes a bare `clagentic-router doctor` work out of the box on
a stock systemd install: it reads the same file the daemon was configured to
load its own token from. If every step fails, the resulting `401` names
exactly which sources were checked and which env-file path was tried — never
a bare `HTTP 401`, and never the token value itself.

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
