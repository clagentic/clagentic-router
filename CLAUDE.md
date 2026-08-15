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

## Breadth is the design principle — read this before adding provider-specific behavior

The router's reason to exist is routing across heterogeneous backends, not
serving one provider well. A feature that works for exactly one provider, one
auth mode, or one host is **incomplete by default**. This has cost real
rework: lr-60781e shipped a codex_cli header design generalized from one
host's Bedrock setup (hand-typed provider/project ids); lr-8dd85a had to
replace it with discovery; lr-82e68e then found the replacement's own
proposed model string didn't exist in the actual ChatGPT-Plus catalog, and
that catalog slug *format* differs by provider — a single-path assumption
that would have broken the majority deployment. When you add
provider-specific behavior, name what happens on every other provider in the
PR description, including the no-op case.

**Adapter breadth today:** CLI adapters (`claude_cli`, `codex_cli`,
`codex_subagent`, `gemini_cli`) use OAuth sessions on the host and require no
API keys — the recommended path for local deployments, but they cannot run in
a container. API adapters (`anthropic_api`, `openai_api`, `bedrock_api`)
require keys/credentials and work in containerised or keyless environments.
Neither family is "preferred" in the abstract — pick per deployment
constraint (host with OAuth sessions vs. containerised/keyless), and any new
adapter-level feature should be evaluated against both families, not built
against whichever one is at hand.

**Discover, don't hardcode — but only what you can actually verify by
calling it.** A value identifying a model, provider, project, or endpoint
should be pulled from the source of truth at runtime, not typed into
config, and *never* documented as discoverable against an endpoint that has
not actually been called (lr-698965 reverted a fabricated Bedrock
mantle project-discovery endpoint that was asserted as fact and never once
invoked — see `internal/backend/codex_discovery.go`'s package doc for the
corrected, override-only shape of that field). clagentic-console's
established pattern is the model: it queries `GET /v1/models` and the codex
`model/list` RPC, keeping a static table only as a failure fallback. In this
repo, `internal/backend/codex_discovery.go`'s provider-id discovery
(lr-8dd85a) and `internal/backend/codex_model_discovery.go`'s model slug
discovery via `codex debug models` (lr-82e68e) are the reference
implementations: both discover once at adapter-construction time, never
per-request, against a locally-readable file or a real subprocess call whose
output shape was captured from a live run — not an assumed remote endpoint.
Degrade to feature-off on failure *when omission is safe* (header
injection — an empty pair is simply "no header," see
`codex_discovery.go`'s package doc); but treat discovery failure as a hard
construction error when omission is not safe (model selection — an
unresolved model is not "feature off," it is "codex picks an undocumented
default," which is the outcome discovery exists to remove; see
`codex_model_discovery.go`'s package doc). Bound every discovery call in time
and response size.

**Explicit config always wins.** Discovery is additive. `buildAdapter` in
`cmd/clagentic-router/main.go` only invokes discovery when the corresponding
config field (`CodexProviderID`, `Model`) is empty; an operator-supplied
value is honored byte-identically with discovery never attempted. This is
the compatibility guarantee that makes it safe to add discovery to a working
deployment. `OpenAIProjectID` has no discovery path at all (see above) — it
is either set by the operator or the header is not injected.

**Don't break working paths.** The Claude brand account via `claude_cli` and
ChatGPT-Plus via `codex_cli` are load-bearing production paths. A change that
improves one backend must be a verified no-op for the others — state that
verification explicitly, don't assume it.

**Subprocess cwd contract (`working_dir`) — cwd is not just file context, it
also selects which config executes.** All four subprocess adapters
(`claude_cli`, `codex_cli`, `codex_subagent`, `gemini_cli`) set `cmd.Dir`
from `req.WorkingDir` when the wire request supplies it, else
`backend.DefaultWorkingDir` (`/`). HTTP adapters (`anthropic_api`,
`openai_api`, `bedrock_api`, `ollama_http`) have no subprocess and ignore
the field. The value is never inferred from server-side state (daemon cwd,
HOME, or any other host-local signal) — the router is a shared daemon, and
a server-chosen directory would just be a different flavor of the bug this
field exists to fix for the next caller; it comes only from an explicit,
validated wire field (`backend.ResolveWorkingDir`).

An earlier version of this doc treated `cmd.Dir` as picking only *which
files a subprocess can read/write* — that assumption was wrong for
`claude_cli`/`codex_subagent`. The claude CLI reads `./CLAUDE.md` and
`./.claude/settings.json` (hooks, `permissions.allow`, MCP servers) from its
cwd. `claude --print`/`claude -p` (what these two adapters use) shows no
workspace trust dialog to gate that read — the CLI's own `--print` help
text says the dialog is skipped in non-interactive mode — so `working_dir`
was implicitly choosing *whose Claude Code config executes inside this
daemon's process* for any caller who could reach `/v1/chat/completions`
with that value. This is closed now: both adapters pass
`--setting-sources user`, unconditionally — see README.md's "Working
directory (`working_dir`)" section for the full writeup and the capability
trade-off it costs. `--safe-mode` was tried first and found insufficient
(it suppressed hooks/CLAUDE.md but left project `permissions.allow`
entries in effect — see `docs/lr-7871bb-verified-run.txt` for the
committed evidence); `--setting-sources user` is a strict superset of what
`--safe-mode` closed, so it replaced rather than supplemented it. There is
no `trusted_working_dirs` config surface for this any more; a prior version
had one, gating a trust-dialog pre-acceptance write that turned out to be
pre-accepting a dialog that never fired on this call path in the first
place. A leftover `trusted_working_dirs` key in an existing `router.yaml` is
ignored, not fatal, on load.

Hook suppression is **not uniform across the four adapters** — this is
pre-existing (predates `working_dir`), not something this field changes:
`claude_cli` and `codex_subagent` both set `HOME=claudeSubprocessHome`
(a stub `~/.claude` with no hook-bearing `settings.json`), so they get two
independent layers — the HOME override, cwd-independent, covering
`~/.claude`-scoped hooks/MCP/memory, plus `cmd.Dir`, covering only
project-scoped `./CLAUDE.md` and `./.claude/settings.json`. `codex_cli` and
`gemini_cli` set no HOME override at all — `cmd.Dir` is their *only* layer.
Defaulting `cmd.Dir` to `/` for those two (this field's fix) strengthens
them regardless: it stops them inheriting the daemon's cwd, even without a
HOME override to match. Do not remove `cmd.Dir` from `codex_cli` or
`gemini_cli` on the assumption a HOME override already covers them — it
doesn't. A new adapter that sets a HOME override should still set `cmd.Dir`
too (mirror `claude_cli`/`codex_subagent`); one that doesn't should still
set `cmd.Dir` as its sole layer (mirror `codex_cli`/`gemini_cli`). Adding a
HOME override to `codex_cli`/`gemini_cli` is a separate, real hardening
question — out of scope here, tracked as a follow-up, not assumed done.
`codex_cli` and `gemini_cli` are different binaries with no
`--setting-sources`-equivalent flag verified to exist; if either grows an
analogous config-auto-discovery surface in the future, evaluate it against
this same non-interactive-mode question before assuming a trust/permission
gate protects it.

`backend.ResolveWorkingDir` validates the wire value (absolute, exists, is
a directory) but is not a containment control: it has no path-containment
allowlist restricting *which* directories are acceptable, and there is a
TOCTOU window between its `os.Stat` check and the subprocess's later
`exec.Start` — the directory could be replaced or removed in between. Both
are known, accepted limitations of the wire-boundary validation, not gaps
to close by adding an allowlist here.

**Verify per-provider assumptions against the live source; never generalize
one host's observed shape into a parser assumption.** Data shapes differ
across providers in ways that are not guessable from one example:
- Codex model catalog slugs are bare on ChatGPT-Plus auth; reportedly
  provider-prefixed under Bedrock. `codex_model_discovery.go` takes the slug
  verbatim and never constructs, prefixes, strips, or normalizes it, because
  the format is not confirmed uniform across auth contexts.
- Codex model catalog `priority` values are sparse and non-contiguous
  (1, 7, 16, 23, 43 observed) and are not comparable across auth contexts —
  `codex_model_discovery.go` selects by sorted position, never by matching a
  literal priority value.
- codex hard-rejects an `http_headers` override against a reserved builtin
  `model_providers` id (e.g. `openai`) — `codex_discovery.go` excludes
  reserved ids from automatic provider selection rather than attempting the
  override and handling the rejection.

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
