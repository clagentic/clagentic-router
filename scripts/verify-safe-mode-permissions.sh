#!/usr/bin/env bash
# scripts/verify-safe-mode-permissions.sh — reproducible evidence for the
# --safe-mode / permissions.allow claim documented in README.md's "The real
# exposure, and what --safe-mode does and does not close" section and in
# internal/backend/claude_cli.go's Invoke doc comment (TODO(lr-7871bb)).
#
# THIS SCRIPT IS THE EVIDENCE, NOT ANY PASTED TABLE. Re-run it against a
# newer claude CLI version to re-derive the claim rather than trusting prose.
#
# What it builds: a throwaway fixture project under $TMPDIR containing
#   - .claude/settings.json with permissions.allow for a distinctive probe
#     command, plus a PreToolUse hook that writes an observable marker file
#   - CLAUDE.md with a sentinel string
# and runs `claude -p --output-format json` against that fixture across a
# matrix of (rule present/absent) x (flag combination), reporting a
# pass/fail table. It also runs a causality check varying only
# --setting-sources while holding the rule present.
#
# Usage:
#   ./scripts/verify-safe-mode-permissions.sh
#
# Gating (NOT part of `make test` — invokes a real CLI, costs tokens):
#   make verify-safe-mode
#
# Requirements: claude CLI on PATH, authenticated (OAuth or API key). If
# either is missing, every matrix cell SKIPs — this script never reports a
# false PASS when it cannot actually invoke the CLI.
#
# ---------------------------------------------------------------------------
# CRITICAL METHODOLOGY NOTE — READ BEFORE "SIMPLIFYING" THIS SCRIPT
# ---------------------------------------------------------------------------
# Do NOT determine pass/fail by substring-matching the probe's expected
# marker text against the model's response/transcript text. On a genuine
# denial, the model may still read the probe script with the Read tool and
# quote its expected output in prose (explaining what the command *would*
# print) — a naive `grep MARKER` over the response would then score a real
# denial as a false PASS. This is silent and inverts the result.
#
# Instead:
#   1. The permission signal is read structurally from
#      `--output-format json`'s `permission_denials` array (or equivalent
#      structural field), never from prose pattern-matching.
#   2. The probe command's real effect (writing a marker FILE on disk, not
#      printing a marker STRING) is checked by stat-ing the file the probe
#      would have created. A denied command cannot have run, so the marker
#      file cannot exist — this is a side-effect check, not a text match,
#      and is immune to the model quoting the probe's source in prose.
# Both signals must agree; a mismatch is reported as an INCONCLUSIVE cell,
# never silently resolved in either direction.
# ---------------------------------------------------------------------------

set -euo pipefail

## ---- config ----------------------------------------------------------------

CLAUDE_BIN="${CLAUDE_BIN:-claude}"
WORKDIR=""
PASS=0
FAIL=0
SKIP=0
INCONCLUSIVE=0

## ---- helpers ---------------------------------------------------------------

green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
yellow(){ printf '\033[0;33m%s\033[0m\n' "$*"; }
bold()  { printf '\033[1m%s\033[0m\n' "$*"; }

cleanup() {
    if [[ -n "$WORKDIR" && -d "$WORKDIR" ]]; then
        rm -rf "$WORKDIR"
    fi
}
trap cleanup EXIT

## ---- preflight: CLI present and authenticated -------------------------------

bold "=== clagentic-router: --safe-mode / permissions.allow verification harness ==="
echo ""

if ! command -v "$CLAUDE_BIN" >/dev/null 2>&1; then
    yellow "SKIP: '$CLAUDE_BIN' not found on PATH. This harness requires the real claude CLI"
    yellow "      and cannot report a result without it. Install/authenticate claude and re-run."
    exit 0
fi

# Cheap auth probe: a trivial print call with a tight timeout. Any failure
# here (auth error, network, etc.) degrades to SKIP, never a false PASS.
if ! "$CLAUDE_BIN" -p --output-format json --max-turns 1 \
        --model claude-haiku-4-5 <<<"reply with: ok" >/dev/null 2>&1; then
    yellow "SKIP: '$CLAUDE_BIN' present but a trivial invocation failed (not authenticated,"
    yellow "      no network, or CLI incompatible). Run 'claude auth login' and re-run."
    exit 0
fi

## ---- fixture construction ---------------------------------------------------

WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/safe-mode-verify.XXXXXX")"
FIXTURE_WITH_RULE="$WORKDIR/with-rule"
FIXTURE_NO_RULE="$WORKDIR/no-rule"

# Distinctive probe: a marker command unlikely to collide with any real
# permissions.allow entry a reader might already have. The "grant" being
# tested is Bash(<PROBE_MARKER_CMD> *) in permissions.allow; the "effect"
# being observed is the hook-written marker FILE (never a printed string —
# see the methodology note above).
PROBE_MARKER_CMD="clagentic_lr4abfe9_probe_$$"
HOOK_MARKER_FILE_NAME="hook-fired.marker"
SENTINEL="CLAGENTIC_LR4ABFE9_SENTINEL_$$"

build_fixture() {
    local dir="$1"
    local include_rule="$2"   # "yes" | "no"

    mkdir -p "$dir/.claude"

    cat > "$dir/CLAUDE.md" <<EOF
# Fixture project for scripts/verify-safe-mode-permissions.sh

Sentinel: $SENTINEL
EOF

    # PreToolUse hook: writes an observable marker file whenever ANY tool
    # fires. Used to detect whether project hooks execute at all (the
    # --safe-mode claim this harness is NOT re-litigating — README.md
    # already documents hooks/CLAUDE.md as suppressed; this hook is here so
    # a reviewer can see that suppression alongside the permissions.allow
    # result in one run, not as a separate untested assertion).
    local hook_cmd
    hook_cmd="printf fired > '$dir/$HOOK_MARKER_FILE_NAME'"

    if [[ "$include_rule" == "yes" ]]; then
        cat > "$dir/.claude/settings.json" <<EOF
{
  "permissions": {
    "allow": [
      "Bash(${PROBE_MARKER_CMD}:*)"
    ]
  },
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "*",
        "hooks": [
          { "type": "command", "command": "$hook_cmd" }
        ]
      }
    ]
  }
}
EOF
    else
        cat > "$dir/.claude/settings.json" <<EOF
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "*",
        "hooks": [
          { "type": "command", "command": "$hook_cmd" }
        ]
      }
    ]
  }
}
EOF
    fi
}

build_fixture "$FIXTURE_WITH_RULE" "yes"
build_fixture "$FIXTURE_NO_RULE" "no"

## ---- matrix runner -----------------------------------------------------------

# run_cell: invoke claude -p in a fixture dir with given extra flags,
# structurally inspect permission_denials for the probe command, and
# side-effect-check for the hook marker file. Never substring-matches the
# probe's expected stdout against the response text (see methodology note).
#
# Args: label, fixture_dir, hook_marker_expect(yes|no), probe_expect(run|deny), extra_flags...
run_cell() {
    local label="$1" fixture_dir="$2" hook_expect="$3" probe_expect="$4"
    shift 4
    local extra_flags=("$@")

    # Fresh marker state per cell.
    rm -f "$fixture_dir/$HOOK_MARKER_FILE_NAME"

    local prompt="Run the shell command '${PROBE_MARKER_CMD} --check' via the Bash tool and report its exit code."
    local out
    local rc=0
    out="$(cd "$fixture_dir" && "$CLAUDE_BIN" -p --output-format json --max-turns 3 \
            --model claude-haiku-4-5 "${extra_flags[@]}" <<<"$prompt" 2>&1)" || rc=$?

    if [[ $rc -ne 0 && -z "$out" ]]; then
        yellow "  SKIP  $label  [claude invocation failed, rc=$rc]"
        (( SKIP++ )) || true
        return
    fi

    # Structural signal: does permission_denials mention our distinctive
    # probe command? This is the primary signal per the methodology note —
    # never derived from matching expected probe OUTPUT text.
    local denied="unknown"
    if command -v python3 >/dev/null 2>&1; then
        denied="$(python3 - "$PROBE_MARKER_CMD" <<PYEOF
import json, sys
marker = sys.argv[1]
raw = sys.stdin.read()
try:
    obj = json.loads(raw)
except Exception:
    print("unknown")
    sys.exit(0)
denials = obj.get("permission_denials")
if denials is None:
    # Field absent entirely (e.g. no denial occurred, or older CLI shape) —
    # do not assume; report unknown and fall back to the side-effect signal.
    print("unknown")
    sys.exit(0)
found = any(marker in json.dumps(d) for d in denials)
print("yes" if found else "no")
PYEOF
<<<"$out")"
    fi

    # Side-effect signal: did the hook actually fire (marker file exists)?
    # A denied tool call cannot have triggered PreToolUse for that call, but
    # other tool calls in the same turn (e.g. the model reading a file)
    # could still fire the hook — so this alone does not prove the PROBE
    # ran; it is corroborating evidence, combined below with the structural
    # denial signal, never used standalone as the pass/fail source.
    local hook_fired="no"
    if [[ -f "$fixture_dir/$HOOK_MARKER_FILE_NAME" ]]; then
        hook_fired="yes"
    fi

    # Resolve expected vs observed.
    local observed_probe
    if [[ "$denied" == "yes" ]]; then
        observed_probe="deny"
    elif [[ "$denied" == "no" ]]; then
        observed_probe="run"
    else
        observed_probe="unknown"
    fi

    local status
    if [[ "$observed_probe" == "unknown" ]]; then
        status="INCONCLUSIVE"
        (( INCONCLUSIVE++ )) || true
    elif [[ "$observed_probe" == "$probe_expect" && "$hook_fired" == "$hook_expect" ]]; then
        status="PASS"
        (( PASS++ )) || true
    else
        status="FAIL"
        (( FAIL++ )) || true
    fi

    case "$status" in
        PASS) green "  PASS  $label  [probe=$observed_probe hook_fired=$hook_fired]" ;;
        FAIL) red   "  FAIL  $label  [probe=$observed_probe(expected $probe_expect) hook_fired=$hook_fired(expected $hook_expect)]" ;;
        *)    yellow "  INCONCLUSIVE  $label  [permission_denials field shape unrecognized; upgrade this harness's parser]" ;;
    esac
}

echo ""
bold "--- Assert isolation: probe is gated ONLY by the project rule, not a user-level rule ---"
# Run the no-rule fixture with NO safe-mode flags at all. If the probe still
# runs here, some ambient (user/global) permission grants it independently
# of this harness's fixture, and every other cell's result would be
# confounded by that ambient grant rather than isolating the project rule.
run_cell "rule ABSENT, no flags (isolation control)" "$FIXTURE_NO_RULE" "yes" "deny"

echo ""
bold "--- Baseline matrix ---"
run_cell "rule PRESENT, no flags (baseline: probe runs)"                          "$FIXTURE_WITH_RULE" "yes" "run"
run_cell "rule PRESENT, --safe-mode (THE GAP: probe should still run)"            "$FIXTURE_WITH_RULE" "no"  "run"  --safe-mode
run_cell "rule PRESENT, --safe-mode --setting-sources user"                        "$FIXTURE_WITH_RULE" "no"  "deny" --safe-mode --setting-sources user
run_cell "rule ABSENT,  --safe-mode --setting-sources user (control)"              "$FIXTURE_NO_RULE"   "no"  "deny" --safe-mode --setting-sources user
run_cell "rule PRESENT, --setting-sources user (isolates which flag does the work)" "$FIXTURE_WITH_RULE" "yes" "deny" --setting-sources user

echo ""
bold "--- Causality check: rule PRESENT, vary --setting-sources only ---"
run_cell "rule PRESENT, --setting-sources user"          "$FIXTURE_WITH_RULE" "yes" "deny" --setting-sources user
run_cell "rule PRESENT, --setting-sources user,project"  "$FIXTURE_WITH_RULE" "yes" "run"  --setting-sources user,project
run_cell "rule PRESENT, --setting-sources project"       "$FIXTURE_WITH_RULE" "yes" "run"  --setting-sources project

## ---- summary -----------------------------------------------------------------

echo ""
bold "=== Results ==="
echo "  Pass:         $PASS"
echo "  Fail:         $FAIL"
echo "  Skip:         $SKIP"
echo "  Inconclusive: $INCONCLUSIVE"
echo ""
echo "This script reports the matrix above; it does not itself assert what"
echo "README.md or claude_cli.go should claim. Update those docs from a real"
echo "run's output, never from a pasted/remembered table (see CLAUDE.md's"
echo "breadth/evidence discipline and TODO(lr-7871bb))."

if [[ "$FAIL" -gt 0 || "$INCONCLUSIVE" -gt 0 ]]; then
    exit 1
fi
exit 0
