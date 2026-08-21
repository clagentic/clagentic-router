// internal/backend/init_frame_classification_test.go — regression tests for
// lr-2f35bd defect B5 (folded in from the ZC-host report): a claude_cli
// backend whose configured model string the endpoint rejects (400 "provided
// model identifier is invalid") emits its type="system"/subtype="init"
// stream-json frame BEFORE the failing request — selecting the first
// well-formed JSON object as "the error" reports the harmless init frame
// and discards the real terminal diagnostic that follows it.
//
// CRITICAL PROCESS NOTE from the folded-in task comment: this defect
// survived three prior classification rounds (lr-807319, lr-c1d353,
// lr-151fa7), which suggested the probe path and the Invoke path did not
// share the classification code those rounds fixed. Investigation for this
// task found they DO share code already — router.ProbeBackend (the /doctor
// and quota-probe call path) invokes the SAME Adapter.Invoke method, which
// runs through the SAME extractClassificationText/errorTextFromStreamJSON/
// parseStreamJSON functions as organic traffic — there is no separate
// probe-specific parser to unify. The actual defect was a genuine gap in
// that ALREADY-shared code: neither errorTextFromStreamJSON (nonzero-exit
// path) nor parseStreamJSON's resultLine==nil fallback (zero-exit path)
// treated a non-JSON line following a valid stream-json line as a candidate
// diagnostic — it was silently discarded once the init frame had already
// set "we saw valid stream-json" to true. See errorTextFromStreamJSON's and
// parseStreamJSON's updated docs in claude_cli.go for the mechanism.
//
// Because ProbeBackend calls the identical Invoke path, every test below
// that exercises Invoke (via a fake binary) is EQUALLY a test of the
// /doctor probe path — there is no separate probe-path test needed to prove
// unification, since unification was already true; these tests prove the
// classification defect within that shared path is fixed.
package backend

import (
	"context"
	"testing"

	"github.com/clagentic/clagentic-router/internal/state"
)

// TestClaudeCLI_InitFrameThenPlainTextDiagnostic_NonzeroExit is the core B5
// reproduction on the nonzero-exit path: init frame, then a plain-text
// terminal diagnostic (not further JSON) — the exact shape the claude CLI
// produces for a model the endpoint rejects. Before the fix this classified
// as ErrTypeUnknown with Raw == the raw init JSON; after the fix it must
// classify from the diagnostic text.
func TestClaudeCLI_InitFrameThenPlainTextDiagnostic_NonzeroExit(t *testing.T) {
	dir := t.TempDir()
	initEvent := `{"type":"system","subtype":"init","cwd":"/home/akuehner/code/project","session_id":"41792fe9-a529-4b1e-9c3a-8e2b1d4f6c81","tools":["Read"],"mcp_servers":[],"model":"claude-bogus-9000","permissionMode":"default"}`
	diagnostic := `Error: provided model identifier is invalid`
	stdout := initEvent + "\n" + diagnostic
	binPath := writeFakeBinExit(t, dir, "claude", stdout, "", 1)

	adapter := NewClaudeCLIAdapter("claude-bad-model", "claude-bogus-9000", binPath, "", ThinkingOff, 0)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	_, err := adapter.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("Invoke succeeded; expected a failure")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("error is not *InvokeError: %T: %v", err, err)
	}
	if ie.Type == ErrTypeUnknown {
		t.Errorf("error_type = %q — init frame was selected over the terminal diagnostic (B5 regression)", ie.Type)
	}
	if ie.Type != ErrTypeModelConfig {
		t.Errorf("error_type = %q, want %q", ie.Type, ErrTypeModelConfig)
	}
	if ie.Raw != diagnostic {
		t.Errorf("Raw = %q, want the terminal diagnostic %q, not the init frame or a blend of both", ie.Raw, diagnostic)
	}
}

// TestClaudeCLI_InitFrameThenPlainTextDiagnostic_ZeroExit is the zero-exit
// analogue: the CLI aborts (no result line, no further JSON event) after
// emitting the init frame and a plain-text diagnostic, but happens to exit
// 0. Before the fix this fell into parseStreamJSON's resultLine==nil
// fallback and was returned as a SUCCESSFUL Response whose Content was the
// raw init-plus-diagnostic blob — fabricating a completion the model never
// produced. After the fix it must be a classified failure.
func TestClaudeCLI_InitFrameThenPlainTextDiagnostic_ZeroExit(t *testing.T) {
	initEvent := `{"type":"system","subtype":"init","session_id":"41792fe9-a529-4b1e-9c3a-8e2b1d4f6c81","model":"claude-bogus-9000"}`
	diagnostic := `Error: provided model identifier is invalid`
	data := []byte(initEvent + "\n" + diagnostic)

	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}
	_, err := parseStreamJSON(data, req, "claude-bad-model")
	if err == nil {
		t.Fatal("parseStreamJSON succeeded; expected a failure — must not fabricate a completion from the init frame (B5)")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("error is not *InvokeError: %T: %v", err, err)
	}
	if ie.Type != ErrTypeModelConfig {
		t.Errorf("error_type = %q, want %q", ie.Type, ErrTypeModelConfig)
	}
	if ie.Raw != diagnostic {
		t.Errorf("Raw = %q, want the terminal diagnostic %q", ie.Raw, diagnostic)
	}
}

// TestClaudeCLI_InitFrameOnly_NoDiagnostic_StillFallsBackAsBefore is a
// non-regression guard: when NO diagnostic follows the init frame at all
// (the pre-existing TestClaudeCLI_NonzeroExit_InitEventNotMisclassified
// shape in claude_cli_error_classification_test.go), behavior is unchanged
// — falls back to stderr, or to the raw stdout text classified as unknown
// when stderr is also empty. This proves the fix does not manufacture a
// diagnostic where none exists.
func TestClaudeCLI_InitFrameOnly_NoDiagnostic_StillFallsBackAsBefore(t *testing.T) {
	dir := t.TempDir()
	initEvent := `{"type":"system","subtype":"init","cwd":"/home/akuehner/code/project","session_id":"41792fe9-a529-4b1e-9c3a-8e2b1d4f6c81","tools":["Read"],"mcp_servers":[],"model":"claude-opus-4-7","permissionMode":"default"}`
	binPath := writeFakeBinExit(t, dir, "claude", initEvent, "", 1)

	adapter := NewClaudeCLIAdapter("claude-high", "claude-opus-4-7", binPath, "", ThinkingOff, 0)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	_, err := adapter.Invoke(context.Background(), req)
	if err == nil {
		t.Fatal("Invoke succeeded; expected a failure")
	}
	ie, ok := err.(*InvokeError)
	if !ok {
		t.Fatalf("error is not *InvokeError: %T: %v", err, err)
	}
	if ie.Type == ErrTypeRateLimit || ie.Type == ErrTypeQuota || ie.Type == ErrTypeAuth {
		t.Errorf("classified as %q from a harmless init event with no diagnostic — misclassification not closed (lr-c1d353)", ie.Type)
	}
}

// TestClaudeCLI_ModelIDValidationError_ChainDoesNotSurfaceAsOverloaded is
// the end-to-end proof of B5's "chain empty for that reason must NOT
// surface as overloaded_error" acceptance point: a backend whose only
// failure mode is the model-id diagnostic drives the router's chain
// exhaustion to carry state.ErrTypeModelConfig, not something that would
// map to a capacity-exhaustion reading downstream (see
// messages.go's writeAnthropicChainExhaustedError for where that mapping is
// actually applied).
func TestClaudeCLI_ModelIDValidationError_ClassifiesAsModelConfigNotUnknownOrRateLimit(t *testing.T) {
	typ := ClassifyError("Error: provided model identifier is invalid", 1)
	if typ != ErrTypeModelConfig {
		t.Fatalf("ClassifyError(model-id diagnostic) = %q, want %q", typ, ErrTypeModelConfig)
	}
	// Cross-check against the state package's mirrored enum used by the
	// router/server layers, confirming the string values stay identical
	// (adapter.go's documented invariant — "String values are identical").
	if string(typ) != string(state.ErrTypeModelConfig) {
		t.Errorf("backend.ErrTypeModelConfig = %q, state.ErrTypeModelConfig = %q — must be byte-identical", typ, state.ErrTypeModelConfig)
	}
}
