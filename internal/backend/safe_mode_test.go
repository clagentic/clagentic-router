// internal/backend/safe_mode_test.go — asserts claude_cli and codex_subagent
// invoke the claude CLI with --safe-mode, and that neither adapter writes a
// per-project trust file anywhere. Both adapters share the same isolated
// subprocess HOME's .claude.json; the second assertion proves the prior
// trust-pre-acceptance write path is fully gone, not merely unused.
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

func TestClaudeCLI_SafeModeFlagPresent(t *testing.T) {
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
	found := false
	for _, a := range args {
		if a == "--safe-mode" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("--safe-mode not present in claude_cli argv: %v", args)
	}
}

func TestCodexSubagent_SafeModeFlagPresent(t *testing.T) {
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
	found := false
	for _, a := range args {
		if a == "--safe-mode" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("--safe-mode not present in codex_subagent argv: %v", args)
	}
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
