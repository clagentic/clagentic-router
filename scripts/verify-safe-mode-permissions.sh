#!/usr/bin/env bash
# scripts/verify-safe-mode-permissions.sh — reproducible evidence for the
# --setting-sources user / permissions.allow claim documented in README.md's
# "The real exposure, and what closes it" section and in
# internal/backend/claude_cli.go's Invoke doc comment.
#
# RESOLVED: --safe-mode alone left a permissions.allow gap open (a project
# .claude/settings.json allow-rule still granted the tool it named).
# --setting-sources user closes that gap — it excludes the project settings
# source entirely, which covers everything --safe-mode covered (hooks,
# CLAUDE.md) plus the gap it missed. Both adapters now ship
# --setting-sources user alone (--safe-mode dropped). This script's matrix
# still includes the --safe-mode cells as the historical record of why that
# flag was tried first and rejected — see docs/lr-7871bb-verified-run.txt
# for a committed real run's output.
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
# --setting-sources while holding the rule present, and a dedicated
# no-tool-use sentinel-recall cell per fixture (see "Sentinel control" below).
#
# Usage:
#   ./scripts/verify-safe-mode-permissions.sh [--output <file>]
#
# --output <file>  Also write a plain-text copy of the result table to
#                   <file>, so a run's evidence can be committed as an
#                   artifact rather than only printed to a terminal.
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
#
# ---------------------------------------------------------------------------
# PARSER PLUMBING NOTE — read before touching run_cell
# ---------------------------------------------------------------------------
# The structural JSON parse below writes its Python source to a FILE
# (PARSER_SCRIPT, created once, outside the per-cell loop) and invokes it as
# `python3 "$PARSER_SCRIPT" "$marker"` with the captured CLI output fed via a
# single here-string. Do not fold the parser back into an inline
# `python3 - <<EOF ... EOF <<<"$data"` construct: a command cannot carry two
# stdin redirections — bash accepts the syntax but only one of them actually
# feeds the process, silently. `python3 -` needs stdin for the JSON payload,
# so its source must come from somewhere else (a file, here), not from a
# heredoc on the same invocation.
# ---------------------------------------------------------------------------
#
# ---------------------------------------------------------------------------
# SENTINEL CONTROL NOTE
# ---------------------------------------------------------------------------
# The CLAUDE.md sentinel is checked with a DEDICATED prompt
# ("What is the exact sentinel value...") that asks the model to state the
# value directly, with no tool use required to answer it. This is what makes
# it a real control for CLAUDE.md auto-discovery: if the model has the
# sentinel memorized from an auto-loaded CLAUDE.md, it can recite it without
# reading any file; if CLAUDE.md was not auto-discovered, the model has no
# way to know the value (the probe-execution prompt never mentions it) and
# should say so. An earlier version of this script checked sentinel_in_output
# on the *probe-execution* prompt's response — the probe prompt gives the
# model no reason to ever mention the sentinel, so that column was always
# False regardless of whether CLAUDE.md was actually loaded, and looked like
# evidence while establishing nothing.
# ---------------------------------------------------------------------------

set -euo pipefail

## ---- config ----------------------------------------------------------------

CLAUDE_BIN="${CLAUDE_BIN:-claude}"
WORKDIR=""
OUTPUT_FILE=""
PASS=0
FAIL=0
SKIP=0
INCONCLUSIVE=0

# Captured result lines, for the optional --output file. Appended by
# run_cell/run_sentinel_cell as they execute so --output reflects exactly
# what was printed to the terminal.
RESULT_LINES=()

## ---- args --------------------------------------------------------------------

while [[ $# -gt 0 ]]; do
    case "$1" in
        --output)
            OUTPUT_FILE="${2:?--output requires a file path}"
            shift 2
            ;;
        *)
            echo "unknown argument: $1" >&2
            exit 2
            ;;
    esac
done

## ---- helpers ---------------------------------------------------------------

green() { printf '\033[0;32m%s\033[0m\n' "$*"; }
red()   { printf '\033[0;31m%s\033[0m\n' "$*"; }
yellow(){ printf '\033[0;33m%s\033[0m\n' "$*"; }
bold()  { printf '\033[1m%s\033[0m\n' "$*"; }

# record: print a line to the terminal (respecting color helpers already
# having run) AND capture a plain-text copy for --output. Call with the
# already-colorized string as-is; the plain copy strips ANSI codes.
record() {
    local line="$1"
    RESULT_LINES+=("$(printf '%s' "$line" | sed 's/\x1b\[[0-9;]*m//g')")
}

cleanup() {
    if [[ -n "$WORKDIR" && -d "$WORKDIR" ]]; then
        rm -rf "$WORKDIR"
    fi
}
trap cleanup EXIT

## ---- preflight: CLI present and authenticated -------------------------------

bold "=== clagentic-router: --setting-sources user / permissions.allow verification harness ==="
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

## ---- structural JSON parser (file, not inline heredoc — see plumbing note) --

PARSER_SCRIPT="$WORKDIR/parse_permission_denials.py"
cat > "$PARSER_SCRIPT" <<'PYEOF'
# parse_permission_denials.py — reads claude --output-format json output on
# stdin, checks whether permission_denials mentions the given marker.
#
# Prints exactly one of:
#   yes              marker found in permission_denials
#   no               permission_denials present, marker not found in it
#   absent           permission_denials field is not present in the object
#                     (e.g. no denial occurred this turn, or an older CLI
#                     shape) -- distinct from a parse failure; the caller
#                     falls back to the side-effect signal in this case.
#   empty-stdin      stdin.read() returned zero bytes -- the caller never
#                     received CLI output, most likely a plumbing bug in the
#                     invoker, not a CLI/schema problem.
#   parse-error: ... json.loads() raised; the CLI's output was not valid
#                     JSON (or was empty/truncated). The exception text is
#                     appended so a reviewer does not have to reproduce the
#                     failure to see what went wrong.
import json
import sys

marker = sys.argv[1]
raw = sys.stdin.read()

if raw == "":
    print("empty-stdin")
    sys.exit(0)

try:
    obj = json.loads(raw)
except Exception as exc:  # noqa: BLE001 -- deliberately broad: any parse
    # failure must degrade to a labeled diagnostic, never propagate as an
    # uncaught traceback the caller has to scrape from stderr.
    print("parse-error: %s" % exc)
    sys.exit(0)

denials = obj.get("permission_denials")
if denials is None:
    print("absent")
    sys.exit(0)

found = any(marker in json.dumps(d) for d in denials)
print("yes" if found else "no")
PYEOF

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
        record "  SKIP  $label  [claude invocation failed, rc=$rc]"
        (( SKIP++ )) || true
        return
    fi

    # Structural signal: does permission_denials mention our distinctive
    # probe command? This is the primary signal per the methodology note —
    # never derived from matching expected probe OUTPUT text. The parser
    # script is invoked as a FILE argument, never inline heredoc source, so
    # stdin is free to carry $out (see the plumbing note above the parser).
    local parse_result="empty-stdin"
    if command -v python3 >/dev/null 2>&1; then
        parse_result="$(python3 "$PARSER_SCRIPT" "$PROBE_MARKER_CMD" <<<"$out")"
    fi

    local denied
    case "$parse_result" in
        yes)    denied="yes" ;;
        no)     denied="no" ;;
        *)      denied="unknown" ;;  # absent / empty-stdin / parse-error: ...
    esac

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

    local line
    case "$status" in
        PASS)
            line="  PASS  $label  [probe=$observed_probe hook_fired=$hook_fired]"
            green "$line"
            ;;
        FAIL)
            line="  FAIL  $label  [probe=$observed_probe(expected $probe_expect) hook_fired=$hook_fired(expected $hook_expect)]"
            red "$line"
            ;;
        *)
            # Distinguish WHY it's inconclusive (bug 3): an empty read, a
            # JSON parse failure, or a genuinely absent field are different
            # failure modes with different fixes, and conflating them into
            # one "field shape unrecognized" message previously sent readers
            # to update the wrong thing (the parser) when the real fault was
            # elsewhere (e.g. no output ever reached the parser at all).
            local reason
            case "$parse_result" in
                empty-stdin)
                    reason="stdin to the parser was empty -- claude produced no output, or it never reached the parser (plumbing bug)"
                    ;;
                absent)
                    reason="permission_denials field is genuinely absent from the CLI's JSON object -- not a parse failure"
                    ;;
                parse-error:*)
                    reason="claude's output was not valid JSON: ${parse_result#parse-error: }"
                    ;;
                *)
                    reason="unrecognized parser result: $parse_result"
                    ;;
            esac
            line="  INCONCLUSIVE  $label  [$reason]"
            yellow "$line"
            ;;
    esac
    record "$line"
}

# run_sentinel_cell: a DEDICATED no-tool-use prompt asking the model to
# state the CLAUDE.md sentinel value directly. See the "SENTINEL CONTROL
# NOTE" above for why this, and not a check against the probe-execution
# cells' output, is what makes sentinel_in_output a real signal.
#
# Args: label, fixture_dir, sentinel_expect(yes|no), extra_flags...
run_sentinel_cell() {
    local label="$1" fixture_dir="$2" sentinel_expect="$3"
    shift 3
    local extra_flags=("$@")

    local prompt="Without using any tools, state the exact value that follows 'Sentinel:' in this project's CLAUDE.md, if you already know it from context you were given. If you were not given that context, say so plainly and do not guess."
    local out
    local rc=0
    out="$(cd "$fixture_dir" && "$CLAUDE_BIN" -p --output-format json --max-turns 1 \
            --model claude-haiku-4-5 "${extra_flags[@]}" <<<"$prompt" 2>&1)" || rc=$?

    if [[ $rc -ne 0 && -z "$out" ]]; then
        yellow "  SKIP  $label  [claude invocation failed, rc=$rc]"
        record "  SKIP  $label  [claude invocation failed, rc=$rc]"
        (( SKIP++ )) || true
        return
    fi

    local observed="no"
    if printf '%s' "$out" | grep -qF "$SENTINEL"; then
        observed="yes"
    fi

    local status line
    if [[ "$observed" == "$sentinel_expect" ]]; then
        status="PASS"
        (( PASS++ )) || true
        line="  PASS  $label  [sentinel_in_output=$observed]"
        green "$line"
    else
        status="FAIL"
        (( FAIL++ )) || true
        line="  FAIL  $label  [sentinel_in_output=$observed(expected $sentinel_expect)]"
        red "$line"
    fi
    record "$line"
}

echo ""
bold "--- Assert isolation: probe is gated ONLY by the project rule, not a user-level rule ---"
# Run the no-rule fixture with NO restricting flags at all. If the probe
# still runs here, some ambient (user/global) permission grants it
# independently of this harness's fixture, and every other cell's result
# would be confounded by that ambient grant rather than isolating the
# project rule.
run_cell "rule ABSENT, no flags (isolation control)" "$FIXTURE_NO_RULE" "yes" "deny"

echo ""
bold "--- Baseline matrix ---"
run_cell "rule PRESENT, no flags (baseline: probe runs)"                          "$FIXTURE_WITH_RULE" "yes" "run"
run_cell "rule PRESENT, --safe-mode (THE REJECTED GAP: probe still runs)"         "$FIXTURE_WITH_RULE" "no"  "run"  --safe-mode
run_cell "rule PRESENT, --safe-mode --setting-sources user"                        "$FIXTURE_WITH_RULE" "no"  "deny" --safe-mode --setting-sources user
run_cell "rule ABSENT,  --safe-mode --setting-sources user (control)"              "$FIXTURE_NO_RULE"   "no"  "deny" --safe-mode --setting-sources user
# --setting-sources user WITHOUT --safe-mode excludes the project settings
# SOURCE entirely, which is where the PreToolUse hook definition itself
# lives, so the hook has nothing to fire from regardless of --safe-mode
# (hook_expect "no" below). If a fresh run of this harness disagrees, THIS
# HARNESS'S OWN OUTPUT WINS -- do not silently re-paper over a disagreement
# by editing the expectation back.
run_cell "rule PRESENT, --setting-sources user (isolates which flag does the work)" "$FIXTURE_WITH_RULE" "no" "deny" --setting-sources user

echo ""
bold "--- Causality check: rule PRESENT, vary --setting-sources only ---"
run_cell "rule PRESENT, --setting-sources user"          "$FIXTURE_WITH_RULE" "no" "deny" --setting-sources user
run_cell "rule PRESENT, --setting-sources user,project"  "$FIXTURE_WITH_RULE" "yes" "run"  --setting-sources user,project
run_cell "rule PRESENT, --setting-sources project"       "$FIXTURE_WITH_RULE" "yes" "run"  --setting-sources project

echo ""
bold "--- Sentinel control: dedicated no-tool-use recall prompt (see SENTINEL CONTROL NOTE) ---"
run_sentinel_cell "rule PRESENT, no flags (sentinel should be known)"                      "$FIXTURE_WITH_RULE" "yes"
run_sentinel_cell "rule PRESENT, --safe-mode (CLAUDE.md suppressed, sentinel unknown)"     "$FIXTURE_WITH_RULE" "no" --safe-mode
run_sentinel_cell "rule PRESENT, --setting-sources user (project source excluded)"         "$FIXTURE_WITH_RULE" "no" --setting-sources user

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
echo "run's output, never from a pasted/remembered table (see this repo's"
echo "breadth/evidence discipline). docs/lr-7871bb-verified-run.txt is the"
echo "committed evidence for the shipped --setting-sources user configuration."

if [[ -n "$OUTPUT_FILE" ]]; then
    {
        printf 'clagentic-router --setting-sources user / permissions.allow verification run\n'
        printf 'claude CLI: %s\n' "$("$CLAUDE_BIN" --version 2>/dev/null || echo unknown)"
        printf 'run date (UTC): %s\n\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
        for line in "${RESULT_LINES[@]}"; do
            printf '%s\n' "$line"
        done
        printf '\n=== Results ===\n'
        printf '  Pass:         %s\n' "$PASS"
        printf '  Fail:         %s\n' "$FAIL"
        printf '  Skip:         %s\n' "$SKIP"
        printf '  Inconclusive: %s\n' "$INCONCLUSIVE"
    } > "$OUTPUT_FILE"
    echo ""
    echo "Result table written to: $OUTPUT_FILE"
fi

if [[ "$FAIL" -gt 0 || "$INCONCLUSIVE" -gt 0 ]]; then
    exit 1
fi
exit 0
