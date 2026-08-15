// internal/backend/setting_sources_test.go — asserts claude_cli and
// codex_subagent invoke the claude CLI with --setting-sources user, and
// that neither adapter writes a per-project trust file anywhere. Both
// adapters share the same isolated subprocess HOME's .claude.json; the
// trust-file assertion proves the prior trust-pre-acceptance write path is
// fully gone, not merely unused.
//
// --setting-sources user replaces an earlier --safe-mode-only approach:
// --safe-mode suppressed project hooks/CLAUDE.md but left project
// .claude/settings.json permissions.allow entries in effect (verified via
// scripts/verify-safe-mode-permissions.sh, see docs/setting-sources-verification-run.txt
// for the committed evidence). --setting-sources user closes that gap by
// excluding the project settings source entirely.
//
// Uses the fake-binary argv-capture pattern from cli_model_passthrough_test.go.
package backend

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeCLI_SettingSourcesUserFlagPresent(t *testing.T) {
	dir := t.TempDir()
	claudeSuccess := func() []byte {
		out := claudeOutput{Type: "result", Result: "hello", CostUSD: 0.001}
		data, _ := json.Marshal(out)
		return data
	}
	binPath := writeFakeBin(t, dir, "claude", string(claudeSuccess()))

	adapter := NewClaudeCLIAdapter("test", "", binPath, "", ThinkingOff)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	args := readArgs(t, dir, "claude")
	if !argvHasFlagValue(args, "--setting-sources", "user") {
		t.Errorf("--setting-sources user not present in claude_cli argv: %v", args)
	}
}

func TestCodexSubagent_SettingSourcesUserFlagPresent(t *testing.T) {
	dir := t.TempDir()
	claudeSuccess := func() []byte {
		out := claudeOutput{Type: "result", Result: "hello from subagent"}
		data, _ := json.Marshal(out)
		return data
	}
	binPath := writeFakeBin(t, dir, "claude", string(claudeSuccess()))

	adapter := NewCodexSubagentAdapter("test", "flagship", binPath)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	args := readArgs(t, dir, "claude")
	if !argvHasFlagValue(args, "--setting-sources", "user") {
		t.Errorf("--setting-sources user not present in codex_subagent argv: %v", args)
	}
}

// argvHasFlagValue reports whether flag immediately followed by value
// appears as adjacent tokens anywhere in args.
func argvHasFlagValue(args []string, flag, value string) bool {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

// TestClaudeCLI_NoTrustFileWritten proves the isolated subprocess HOME's
// .claude.json is never created or modified by an Invoke call — the prior
// pre-accept-trust write path (removed) is gone, not just dormant.
// claudeSubprocessHome is resolved once at package init (it is a package
// var, not re-read per Invoke), so this test targets that real resolved
// path rather than trying to inject a fresh one via env var — a
// CLAGENTIC_ROUTER_SUBPROCESS_HOME override at test time would have no
// effect on the already-initialized package var.
func TestClaudeCLI_NoTrustFileWritten(t *testing.T) {
	if claudeSubprocessHome == "" {
		t.Skip("claudeSubprocessHome resolved empty at package init; nothing to assert")
	}

	trustFile := filepath.Join(claudeSubprocessHome, ".claude.json")
	var beforeModTime int64
	if info, err := os.Stat(trustFile); err == nil {
		beforeModTime = info.ModTime().UnixNano()
	}

	dir := t.TempDir()
	claudeSuccess := func() []byte {
		out := claudeOutput{Type: "result", Result: "hello", CostUSD: 0.001}
		data, _ := json.Marshal(out)
		return data
	}
	binPath := writeFakeBin(t, dir, "claude", string(claudeSuccess()))

	adapter := NewClaudeCLIAdapter("test", "", binPath, "", ThinkingOff)
	req := &Request{
		Messages:   []Message{{Role: "user", Content: "ping"}},
		WorkingDir: t.TempDir(),
	}
	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	info, err := os.Stat(trustFile)
	if err != nil {
		if os.IsNotExist(err) {
			return // never existed before or after — expected.
		}
		t.Fatalf("unexpected error stating %s: %v", trustFile, err)
	}
	if beforeModTime == 0 {
		t.Errorf("%s was created by Invoke; the trust pre-acceptance path should be fully removed", trustFile)
		return
	}
	if info.ModTime().UnixNano() != beforeModTime {
		t.Errorf("%s was modified by Invoke; the trust pre-acceptance path should be fully removed", trustFile)
	}
}
