# AWS Bedrock

Every AWS Bedrock auth path and Bedrock-shaped endpoint this router
supports, in one place. There are **three structurally different** Bedrock
integration points — do not conflate them:

1. [CLI-adapter Bedrock auth](#cli-adapter-bedrock-auth-claude_cli--codex_subagent) — `claude_cli`/`codex_subagent` (and `codex_cli`) authenticating *to* Bedrock as an alternative to OAuth, via subprocess env inheritance.
2. [`bedrock_api` adapter](#bedrock_api-adapter) — the router calling Bedrock directly, in-process, via the AWS SDK's Converse API.
3. [Bedrock InvokeModel HTTP endpoints](#bedrock-invokemodel-api) — the router impersonating a Bedrock Runtime endpoint *for inbound clients* (e.g. Bedrock-mode Claude Code).

## CLI-adapter Bedrock auth (`claude_cli` / `codex_subagent`)

`claude_cli` and `codex_subagent` both invoke the `claude` binary, which
supports authenticating via AWS Bedrock as an alternative to OAuth
(`CLAUDE_CODE_USE_BEDROCK=1`, standard AWS SDK credential chain).
`codex_cli` supports an analogous Bedrock-fronted mode via `codex`'s own
config, not an env var — see its row below. This is a **structurally
different auth path from `bedrock_api`**: it works through subprocess env
inheritance (`buildCLIEnv`'s allowlist), not an in-process
`config.LoadDefaultConfig` call — do not conflate the two sections.

### The switch, per CLI

| CLI | How Bedrock mode is selected | Where it's set |
|---|---|---|
| `claude` (used by `claude_cli`, `codex_subagent`) | `CLAUDE_CODE_USE_BEDROCK=1` — an **environment variable** | Daemon's own service environment (not `router.yaml`) |
| `codex` (used by `codex_cli`) | `model_provider = "amazon-bedrock"` in `~/.codex/config.toml` — **not an env var** | The operator's `~/.codex/config.toml` on the host, before starting the daemon |

### What must reach the subprocess

Two things must both be true for CLI-adapter Bedrock auth to work through
the router's isolated subprocess HOME (`claudeSubprocessHome` — see
CLAUDE.md's "Subprocess cwd contract"):

1. **`CLAUDE_CODE_USE_BEDROCK` must reach the subprocess.** It is a literal
   in `cliEnvAllowlistLiterals` (`internal/backend/env.go`) alongside the
   AWS SDK standard credential/config vars listed there
   (`AWS_PROFILE`, `AWS_REGION`, `AWS_DEFAULT_REGION`,
   `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN`,
   `AWS_ROLE_ARN`, `AWS_WEB_IDENTITY_TOKEN_FILE`, `AWS_SDK_LOAD_CONFIG`,
   `AWS_CONFIG_FILE`, `AWS_SHARED_CREDENTIALS_FILE`). Set it in the
   daemon's own environment the same way any other AWS SDK var is set —
   this list is the single source of truth for what passes the filter;
   this doc cross-references it rather than re-listing values that would
   drift.
2. **An SSO-based Bedrock-fronting AWS profile needs its cached token
   mirrored into the isolated HOME.** `AWS_PROFILE`/`AWS_REGION` only
   *name* a profile; resolving an SSO profile into short-lived credentials
   requires `~/.aws/sso/cache/*.json` — a file, not an env var — which the
   isolated subprocess HOME does not have by default.
   `syncSubprocessAWSSSOCache` (`internal/backend/claude_cli.go`) mirrors
   that one directory from the daemon's real HOME into the isolated HOME
   on every Invoke, the same way `syncSubprocessCreds` keeps OAuth
   credentials current. It syncs **only** `~/.aws/sso/cache/` — never
   `~/.aws/config`, `~/.aws/credentials`, or any other real-HOME state — to
   preserve the isolation property. A **static-credential** Bedrock profile
   (`AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`/`AWS_SESSION_TOKEN`,
   already allowlisted) needs only item 1; item 2 is SSO-specific.

This matters specifically for `claude_cli`/`codex_subagent` because they
run with an **isolated HOME** (`claudeSubprocessHome`, a stub `~/.claude`
with no hook-bearing `settings.json`). `codex_cli` and `gemini_cli` set no
HOME override at all — they read the daemon's real `~/.aws` directly, so
this SSO cache mirror is not needed and does not run for them.

### Additive mirroring is a deliberate, documented property — not a bug (lr-dbbcd3)

`syncSubprocessAWSSSOCache` and the pre-existing `syncSubprocessCreds`
(`.claude/.credentials.json`) both mirror **additively**: neither has a
delete/prune step on the destination. If a source token file is removed
from the daemon's real HOME — the ordinary consequence of an `aws sso
logout` clearing the cache entry, or a `claude auth logout` clearing the
credentials file — the previously-synced copy **remains in the isolated
subprocess HOME indefinitely**.

**This is decided as intentional, not a defect to fix,** for both
functions identically:

- The residual is a stale, **short-lived, already-expired** token by the
  time it matters (SSO cache tokens and OAuth tokens both rotate on a
  timescale much shorter than "operator forgot this exists"). Written
  `0600`, inside an isolated HOME that exists specifically to not be the
  operator's real one.
- There is no known path by which a stale expired token grants access —
  the daemon's own subprocess is the only reader of `claudeSubprocessHome`,
  and an expired token fails auth the same way an absent one would.
- The realistic failure mode is **confusing, not dangerous**: a revoked
  profile appears to still be present in the subprocess directory, and a
  future debugging session could waste time inspecting a token the
  operator believes they revoked. That is a documentation problem (this
  section exists to close it), not a security or correctness problem
  requiring code.
- A prune was evaluated and rejected as the wrong shape for the actual
  risk: it would add a delete path to functions whose entire job, as of
  PR #51 (`lr-6572d5`), is reliably getting a valid token into place before
  a Bedrock-fronted call — a prune that runs before a failed write, or
  that removes a file the sync is about to refresh, would reintroduce the
  exact missing-token hang that PR fixed. The `len(entries) == 0` early
  return in `syncSubprocessAWSSSOCache` (an entirely emptied `srcDir`
  skips the sync loop) would also need prune-specific handling to cover
  the full-logout case at all — extra surface for a residual whose actual
  blast radius is "confusing," not "unsafe."

If this changes (e.g. a future requirement that revoked credentials must
not persist anywhere, for compliance reasons stronger than "isolated,
short-lived, unreadable by anything else"), a prune must be added to
**both** `syncSubprocessCreds` and `syncSubprocessAWSSSOCache` in the same
change — never one without the other, since an inconsistency between the
two would just become the next surprise — with the same test rigor already
in `claude_cli_test.go` (assert on observable destination state via a temp
HOME) and explicit handling of the `len(entries) == 0` early-return path.

**Verified no-op for every other deployment shape.** A host with no
`~/.aws/sso/cache` directory (OAuth hosts, static-credential Bedrock hosts —
the majority deployment) hits `syncSubprocessAWSSSOCache`'s absent-source
path: it logs at `Debug` and returns without creating anything in the
subprocess HOME. `codex_cli` and `gemini_cli` set no HOME override at all,
so they already read the daemon's real `~/.aws` directly — no code change
touches either. `bedrock_api` is an in-process AWS SDK call with no
subprocess — structurally unaffected. `anthropic_api`/`openai_api`/
`ollama_http` have no subprocess and no AWS involvement at all.

**Operator-dependent verification.** This was built and tested with unit
coverage against a temp-directory fake HOME
(`internal/backend/claude_cli_test.go`) and is not verified end-to-end
against a live Bedrock-fronted SSO profile from the build environment —
the same caveat `bedrock_api` below carries for its own Bedrock path. If
you enable this and the router still cannot authenticate on your host,
capture the CLI's stderr/exit code and file it.

## `bedrock_api` adapter

Calls the [Bedrock Runtime Converse API](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_Converse.html),
which is uniform across model families for text-only, non-streaming
invocation — the same adapter covers both the Anthropic and OpenAI families
hosted on Bedrock with no model-family-specific config. Image input,
streaming, and tool-use content blocks are out of scope for this adapter;
text-only requests/responses only. Non-text response content blocks (tool
use, reasoning, etc.) are skipped rather than causing an error.

```yaml
backends:
  bedrock-claude:
    adapter: bedrock_api
    region: us-east-1                      # required — Bedrock has no SDK default region
    model: anthropic.claude-sonnet-4-6-v1:0  # confirm the exact ID in the Bedrock console
    # profile: my-aws-profile              # optional named AWS profile
    timeout_seconds: 180
```

**Credentials.** Resolved exclusively via the standard AWS SDK credential
chain (env vars → web identity → shared credentials file → shared config
file → ECS → IMDS) — the same chain `aws-cli` and every other AWS SDK use.
There is no `api_key` field for this adapter and `router.yaml` must never
carry AWS credentials; set `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` (or
better, a role) in the environment, or use `profile` to select a named
profile from `~/.aws/config` / `~/.aws/credentials`.

**Region.** Required — Bedrock has no default region in the SDK. An empty
`region` fails config validation at startup rather than failing on the
first call.

**Model ID semantics.** Two ID forms exist and they are *not*
interchangeable:
- **Bare model IDs** (e.g. `anthropic.claude-sonnet-4-6-v1:0`) are
  region-specific — the model must be enabled for that exact region in your
  account.
- **Region-prefixed inference profile IDs** (e.g. `us.anthropic.claude-...`,
  `eu.anthropic.claude-...`) enable cross-region routing with failover, and
  some models are reachable *only* via an inference profile.

Check the Bedrock console for your target model to determine which form it
requires. The Anthropic family's Bedrock IDs are well-documented; **the
exact ID strings for the OpenAI family hosted on Bedrock were not confirmed
during this adapter's development** — do not copy an ID from this doc for
an OpenAI-family model without first verifying it in the console for your
account/region.

**Error classification.** Bedrock Runtime SDK exceptions are mapped onto
the router's existing `ErrorType` enum via `errors.As` (never string
matching): `ThrottlingException`/`ModelNotReadyException` → rate limit,
`AccessDeniedException` → auth (covers both missing/expired credentials
*and* a model not enabled for your account — both surface as the same SDK
exception type), `ValidationException`/`ResourceNotFoundException` →
schema, `ModelTimeoutException` → timeout, and
`ModelErrorException`/`InternalServerException`/`ServiceUnavailableException`
→ network (retriable downstream failure).

**Operator-verifiable, not verified here.** This adapter was built and
tested entirely against a mocked Bedrock client — no live AWS account was
available in the build environment. Live end-to-end verification against a
real Bedrock endpoint (credential resolution, an actual Converse call
succeeding, and the OpenAI-family model ID question above) is left to the
operator enabling this backend against their own AWS account.

## Bedrock InvokeModel API

`POST /model/{modelId}/invoke` and
`POST /model/{modelId}/invoke-with-response-stream` let Bedrock-mode Claude
Code (`CLAUDE_CODE_USE_BEDROCK=1`, `ANTHROPIC_BEDROCK_BASE_URL` pointed at
the router) work the same way `/v1/messages` works for direct-API mode.
The key difference from `/v1/messages`: **the model identifier is carried
entirely by the URL path segment, never the request body** — there is no
`model` field in a Bedrock InvokeModel request. That path segment
(`{modelId}`) is the routing key, exactly as with `/v1/messages`'s body
`model` field:

| `{modelId}` | Mode | Behavior |
|---|---|---|
| A real Bedrock model/inference-profile ID (`anthropic.claude-...`, `us.anthropic.claude-...`) | **Passthrough** (default) | SigV4-signed reverse proxy to the real AWS Bedrock Runtime endpoint for `bedrock.region`. Request body and response (including AWS event-stream-framed responses) are forwarded byte-for-byte. |
| `role:<chain>` / `chain:a,b,c` / `backend:<id>` | **Routed** | Translated to the router's internal request format, sent through the same fallback-chain machinery as `/v1/chat/completions` and `/v1/messages`, translated back into the Bedrock InvokeModel response envelope — byte-identical in shape to the direct Anthropic Messages response for Anthropic models on Bedrock. |

### Auth matrix

Same asymmetric shape as `/v1/messages`:

- **Passthrough mode**: the router does **not** check its own inference
  token — Bedrock passthrough is authenticated by SigV4 signing with AWS
  credentials the router itself resolves (see Config below), not by any
  credential the client presents.
- **Routed mode**: requires the router's own inference token, presented as
  `x-api-key: <token>` OR `Authorization: Bearer <token>`.

### Tools

Same refusal behavior as `/v1/messages` routed mode: if the request body's
`tools` field is present and the resolved chain has no tool-capable
backend, the router returns `422` (Bedrock error envelope:
`{"message": "..."}`) rather than a `200` with tools silently dropped.
Passthrough forwards `tools` untouched — this check only applies to
`role:*`/`chain:*`/`backend:*` model IDs.

### Config

```yaml
bedrock:
  region: us-east-1        # required for passthrough; routed-mode-only deployments may omit
  # profile: my-aws-profile  # optional named AWS profile for passthrough credential resolution
```

Credentials for passthrough are resolved via the same standard AWS SDK
credential chain the `bedrock_api` adapter uses (env vars → web identity →
shared credentials file → shared config file → ECS → IMDS) — see
[`bedrock_api` adapter](#bedrock_api-adapter) above for the full
credential/region discussion. A passthrough request with no
`bedrock.region` configured fails with `503` rather than attempting to
sign with an empty region.

### Streaming framing

The streaming variant emits **AWS event-stream framing**
(`Content-Type: application/vnd.amazon.eventstream`) — a third
response-framing scheme distinct from both plain SSE
(`/v1/chat/completions`) and the Anthropic Messages SSE grammar
(`/v1/messages` routed mode). Passthrough forwards the upstream
event-stream bytes unmodified; routed mode constructs event-stream frames
itself (via the AWS SDK's own `aws/protocol/eventstream` codec, not
hand-rolled), one frame per Anthropic-grammar event (`message_start`,
`content_block_delta`, etc.) — same known routed-mode limitation as
`/v1/messages`: the full response arrives in one `content_block_delta`
frame rather than true token-by-token streaming.

### Verification status

The routed-mode path (path extraction, request/response translation,
event-stream encode/decode) is unit-tested deterministically and requires
no AWS account. The passthrough path's request-building and SigV4 signing
are unit-tested against an `httptest` stand-in with injected stub
credentials — this verifies the signing call shape and header production,
but **does not** verify AWS itself accepts the signed request end-to-end.
No live AWS Bedrock account was available during this endpoint's
development (same caveat as the `bedrock_api` adapter above); live
verification against a real Bedrock endpoint is left to the operator
enabling Bedrock passthrough.

## See also

- [AGENT-REFERENCE.md](AGENT-REFERENCE.md) — full adapter capability matrix
  and error taxonomy, of which the Bedrock rows above are one part.
- [OPERATOR-GUIDE.md](OPERATOR-GUIDE.md) — install/configure/deploy.
- `router.example.yaml` — annotated `bedrock-claude`/`bedrock-openai`
  backend examples and the top-level `bedrock:` passthrough block.
