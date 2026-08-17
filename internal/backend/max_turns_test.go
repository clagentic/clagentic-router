// internal/backend/max_turns_test.go — tests for the --max-turns ceiling
// and max_turns termination classification shared by claude_cli and
// codex_subagent (lr-39ed6b).
//
// Covers, per the task's TESTING requirements:
//   - the chosen ceiling (DefaultMaxTurns) appears in argv for both adapters
//     when no per-backend override is configured;
//   - a per-backend override is honored, and explicit config wins
//     byte-identically over the default;
//   - a fake claude emitting a max_turns terminal_reason is classified as
//     ErrTypeMaxTurns, not ErrTypeUnknown, for both adapters;
//   - the tool-free path (a normal "result" line with no is_error/
//     terminal_reason) still succeeds — the MUST NOT REGRESS requirement.
//
// Uses the fake-binary argv-capture pattern from cli_model_passthrough_test.go
// (writeFakeBin/readArgs/findFlag).
package backend

import (
	"context"
	"encoding/json"
	"testing"
)

// --- argv: ceiling present for both adapters ---

// TestClaudeCLI_MaxTurnsDefaultInArgv verifies that with no per-backend
// override (maxTurns=0), ClaudeCLIAdapter passes DefaultMaxTurns to
// --max-turns — not the old hardcoded "1".
func TestClaudeCLI_MaxTurnsDefaultInArgv(t *testing.T) {
	dir := t.TempDir()
	claudeSuccess := func() []byte {
		out := claudeOutput{Type: "result", Result: "hello", CostUSD: 0.001}
		data, _ := json.Marshal(out)
		return data
	}
	binPath := writeFakeBin(t, dir, "claude", string(claudeSuccess()))

	adapter := NewClaudeCLIAdapter("test", "", binPath, "", ThinkingOff, 0)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	args := readArgs(t, dir, "claude")
	got := findFlag(args, "--max-turns")
	want := "5"
	if DefaultMaxTurns != 5 {
		t.Fatalf("test assumes DefaultMaxTurns==5; got %d — update this test's expectation", DefaultMaxTurns)
	}
	if got != want {
		t.Errorf("--max-turns = %q, want %q (DefaultMaxTurns)", got, want)
	}
	if got == "1" {
		t.Error("--max-turns is still hardcoded to 1 — the turn-ceiling defect is not fixed")
	}
}

// TestCodexSubagent_MaxTurnsDefaultInArgv is the codex_subagent analogue of
// TestClaudeCLI_MaxTurnsDefaultInArgv — HOLDEN verified this adapter has the
// identical hardcoded --max-turns 1 defect (same claude binary, same flag).
func TestCodexSubagent_MaxTurnsDefaultInArgv(t *testing.T) {
	dir := t.TempDir()
	claudeSuccess := func() []byte {
		out := claudeOutput{Type: "result", Result: "hello from subagent"}
		data, _ := json.Marshal(out)
		return data
	}
	binPath := writeFakeBin(t, dir, "claude", string(claudeSuccess()))

	adapter := NewCodexSubagentAdapter("test", "flagship", binPath, 0)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	args := readArgs(t, dir, "claude")
	got := findFlag(args, "--max-turns")
	want := "5"
	if got != want {
		t.Errorf("--max-turns = %q, want %q (DefaultMaxTurns)", got, want)
	}
	if got == "1" {
		t.Error("--max-turns is still hardcoded to 1 — the turn-ceiling defect is not fixed (codex_subagent)")
	}
}

// --- argv: per-backend override honored, explicit config wins ---

// TestClaudeCLI_MaxTurnsOverrideHonored verifies a positive per-backend
// maxTurns value is passed to --max-turns verbatim, not replaced by
// DefaultMaxTurns — CLAUDE.md's "explicit config always wins" guarantee.
func TestClaudeCLI_MaxTurnsOverrideHonored(t *testing.T) {
	dir := t.TempDir()
	claudeSuccess := func() []byte {
		out := claudeOutput{Type: "result", Result: "hello", CostUSD: 0.001}
		data, _ := json.Marshal(out)
		return data
	}
	binPath := writeFakeBin(t, dir, "claude", string(claudeSuccess()))

	adapter := NewClaudeCLIAdapter("test", "", binPath, "", ThinkingOff, 12)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	args := readArgs(t, dir, "claude")
	got := findFlag(args, "--max-turns")
	if got != "12" {
		t.Errorf("--max-turns = %q, want %q (explicit override must win byte-identically over DefaultMaxTurns)", got, "12")
	}
}

// TestCodexSubagent_MaxTurnsOverrideHonored is the codex_subagent analogue.
func TestCodexSubagent_MaxTurnsOverrideHonored(t *testing.T) {
	dir := t.TempDir()
	claudeSuccess := func() []byte {
		out := claudeOutput{Type: "result", Result: "hello from subagent"}
		data, _ := json.Marshal(out)
		return data
	}
	binPath := writeFakeBin(t, dir, "claude", string(claudeSuccess()))

	adapter := NewCodexSubagentAdapter("test", "flagship", binPath, 8)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	args := readArgs(t, dir, "claude")
	got := findFlag(args, "--max-turns")
	if got != "8" {
		t.Errorf("--max-turns = %q, want %q (explicit override must win byte-identically over DefaultMaxTurns)", got, "8")
	}
}

// TestResolveMaxTurns_ExplicitConfigWins unit-tests the shared resolver
// directly: a positive configured value is returned unchanged; <= 0 (unset
// or an invalid negative) resolves to DefaultMaxTurns.
func TestResolveMaxTurns_ExplicitConfigWins(t *testing.T) {
	cases := []struct {
		name       string
		configured int
		want       int
	}{
		{"unset (zero) resolves to default", 0, DefaultMaxTurns},
		{"negative resolves to default", -1, DefaultMaxTurns},
		{"positive override honored verbatim", 3, 3},
		{"positive override honored verbatim, larger", 20, 20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveMaxTurns(tc.configured); got != tc.want {
				t.Errorf("resolveMaxTurns(%d) = %d, want %d", tc.configured, got, tc.want)
			}
		})
	}
}

// --- classification: max_turns termination is ErrTypeMaxTurns, not unknown ---

// TestClaudeCLI_MaxTurnsTermination_ClassifiedDistinctly verifies that a
// fake claude emitting is_error:true / terminal_reason:max_turns (the exact
// shape from the reporter's live capture) is classified as ErrTypeMaxTurns —
// not ErrTypeUnknown, and not silently converted into a success despite the
// tool call itself having succeeded upstream.
func TestClaudeCLI_MaxTurnsTermination_ClassifiedDistinctly(t *testing.T) {
	dir := t.TempDir()
	maxTurnsOutput := func() []byte {
		out := claudeOutput{
			Type:           "result",
			Subtype:        "error_max_turns",
			IsError:        true,
			TerminalReason: "max_turns",
			NumTurns:       1,
			Errors:         []string{"Reached maximum number of turns (1)"},
		}
		data, _ := json.Marshal(out)
		return data
	}
	binPath := writeFakeBin(t, dir, "claude", string(maxTurnsOutput()))

	adapter := NewClaudeCLIAdapter("test", "", binPath, "", ThinkingOff, 1)
	req := &Request{Messages: []Message{{Role: "user", Content: "what directory are you in?"}}}

	resp, err := adapter.Invoke(context.Background(), req)
	if err == nil {
		t.Fatalf("Invoke succeeded with resp=%+v; expected a max_turns failure, not a silently converted success", resp)
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("error is not *InvokeError: %T: %v", err, err)
	}
	if ie.Type != ErrTypeMaxTurns {
		t.Errorf("classified as %q, want %q (must not fall through to ErrTypeUnknown — that is the exact silent-misattribution defect lr-39ed6b closes)", ie.Type, ErrTypeMaxTurns)
	}
	if ie.Type == ErrTypeUnknown {
		t.Error("max_turns termination classified as unknown — indistinguishable from auth/network/crash, the defect this task fixes")
	}
}

// TestCodexSubagent_MaxTurnsTermination_ClassifiedDistinctly is the
// codex_subagent analogue — same claude binary, same JSON shape (single
// object, not stream-json, per --output-format json).
func TestCodexSubagent_MaxTurnsTermination_ClassifiedDistinctly(t *testing.T) {
	dir := t.TempDir()
	maxTurnsOutput := func() []byte {
		out := claudeOutput{
			Type:           "result",
			Subtype:        "error_max_turns",
			IsError:        true,
			TerminalReason: "max_turns",
			NumTurns:       1,
			Errors:         []string{"Reached maximum number of turns (1)"},
		}
		data, _ := json.Marshal(out)
		return data
	}
	binPath := writeFakeBin(t, dir, "claude", string(maxTurnsOutput()))

	adapter := NewCodexSubagentAdapter("test", "flagship", binPath, 1)
	req := &Request{Messages: []Message{{Role: "user", Content: "what directory are you in?"}}}

	resp, err := adapter.Invoke(context.Background(), req)
	if err == nil {
		t.Fatalf("Invoke succeeded with resp=%+v; expected a max_turns failure, not a silently converted success", resp)
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("error is not *InvokeError: %T: %v", err, err)
	}
	if ie.Type != ErrTypeMaxTurns {
		t.Errorf("classified as %q, want %q", ie.Type, ErrTypeMaxTurns)
	}
}

// TestIsMaxTurnsTermination_FallbackViaErrorsMessage verifies the Errors
// fallback path (case-insensitive "maximum number of turns" substring) is
// used when TerminalReason is absent but the human-readable message is
// present — a defensive fallback for CLI shapes where the structured field
// might not be populated identically to the reporter's captured version.
func TestIsMaxTurnsTermination_FallbackViaErrorsMessage(t *testing.T) {
	out := &claudeOutput{
		Errors: []string{"Reached MAXIMUM NUMBER OF TURNS (3)"},
	}
	if !isMaxTurnsTermination(out) {
		t.Error("expected isMaxTurnsTermination to match via Errors fallback when TerminalReason is absent")
	}

	notMaxTurns := &claudeOutput{
		Errors: []string{"authentication_error: invalid api key"},
	}
	if isMaxTurnsTermination(notMaxTurns) {
		t.Error("unrelated error message incorrectly classified as max_turns")
	}
}

// --- MUST NOT REGRESS: tool-free path still succeeds ---

// TestClaudeCLI_ToolFreePath_StillSucceeds is the regression guard for the
// task's MUST NOT REGRESS requirement: a normal "result" line with no
// is_error/terminal_reason must parse as a success exactly as before this
// change, for both the default ceiling and an explicit override.
func TestClaudeCLI_ToolFreePath_StillSucceeds(t *testing.T) {
	cases := []struct {
		name     string
		maxTurns int
	}{
		{"default ceiling", 0},
		{"explicit override", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			claudeSuccess := func() []byte {
				out := claudeOutput{Type: "result", Result: "clean tool-free answer", CostUSD: 0.002}
				data, _ := json.Marshal(out)
				return data
			}
			binPath := writeFakeBin(t, dir, "claude", string(claudeSuccess()))

			adapter := NewClaudeCLIAdapter("test", "", binPath, "", ThinkingOff, tc.maxTurns)
			req := &Request{Messages: []Message{{Role: "user", Content: "say hello"}}}

			resp, err := adapter.Invoke(context.Background(), req)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if resp.Content != "clean tool-free answer" {
				t.Errorf("Content = %q, want %q", resp.Content, "clean tool-free answer")
			}
		})
	}
}

// TestCodexSubagent_ToolFreePath_StillSucceeds is the codex_subagent
// analogue of TestClaudeCLI_ToolFreePath_StillSucceeds.
func TestCodexSubagent_ToolFreePath_StillSucceeds(t *testing.T) {
	dir := t.TempDir()
	claudeSuccess := func() []byte {
		out := claudeOutput{Type: "result", Result: "clean tool-free answer from subagent"}
		data, _ := json.Marshal(out)
		return data
	}
	binPath := writeFakeBin(t, dir, "claude", string(claudeSuccess()))

	adapter := NewCodexSubagentAdapter("test", "flagship", binPath, 0)
	req := &Request{Messages: []Message{{Role: "user", Content: "say hello"}}}

	resp, err := adapter.Invoke(context.Background(), req)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Content != "clean tool-free answer from subagent" {
		t.Errorf("Content = %q, want %q", resp.Content, "clean tool-free answer from subagent")
	}
}
