// internal/backend/codex_cli_test.go — tests for the CodexCLIAdapter's optional
// OpenAI-Project header injection via -c model_providers.<id>.http_headers.
//
// Uses the fake-binary-argv pattern from cli_model_passthrough_test.go: a fake
// codex binary records its argv to a file, and the test inspects it. All ids
// below are fabricated placeholders, not real model or project ids.
package backend

import (
	"context"
	"testing"
)

// findFlagAll returns every value immediately following an occurrence of flag
// in args, preserving order. Used because -c may appear more than once.
func findFlagAll(args []string, flag string) []string {
	var out []string
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// TestCodexCLI_ProjectHeaderInjection verifies that the -c
// model_providers.<id>.http_headers override is appended only when both
// CodexProviderID and OpenAIProjectID (providerID/projectID constructor args)
// are non-empty, is absent when either is empty (default/backward-compat
// path), and composes cleanly alongside an existing -c model_reasoning_effort
// override.
func TestCodexCLI_ProjectHeaderInjection(t *testing.T) {
	const (
		fakeProviderID = "acme-custom-provider"
		fakeProjectID  = "proj_fake_0000000000"
		fakeEffort     = "medium"
	)
	wantHeaderFlag := `model_providers.` + fakeProviderID + `.http_headers={"OpenAI-Project"="` + fakeProjectID + `"}`

	cases := []struct {
		name            string
		providerID      string
		projectID       string
		reasoningEffort string
		wantHeader      bool
	}{
		{"both set", fakeProviderID, fakeProjectID, "", true},
		{"provider empty", "", fakeProjectID, "", false},
		{"project empty", fakeProviderID, "", "", false},
		{"both empty default backward compat", "", "", "", false},
		{"both set, composed with model_reasoning_effort", fakeProviderID, fakeProjectID, fakeEffort, true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			binPath := writeFakeBin(t, dir, "codex", "response from codex")

			adapter := NewCodexCLIAdapter("test", "", tc.reasoningEffort, tc.providerID, tc.projectID, binPath)
			req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

			resp, err := adapter.Invoke(context.Background(), req)
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if resp.Content != "response from codex" {
				t.Errorf("unexpected content: %q", resp.Content)
			}

			args := readArgs(t, dir, "codex")
			gotHeaders := findFlagAll(args, "-c")

			hasHeaderFlag := false
			for _, v := range gotHeaders {
				if v == wantHeaderFlag {
					hasHeaderFlag = true
				}
			}
			if tc.wantHeader && !hasHeaderFlag {
				t.Errorf("expected -c %q in args, got -c values: %v", wantHeaderFlag, gotHeaders)
			}
			if !tc.wantHeader && hasHeaderFlag {
				t.Errorf("did not expect -c %q in args (providerID=%q projectID=%q), got: %v",
					wantHeaderFlag, tc.providerID, tc.projectID, gotHeaders)
			}

			if tc.reasoningEffort != "" {
				wantEffortFlag := `model_reasoning_effort="` + tc.reasoningEffort + `"`
				hasEffortFlag := false
				for _, v := range gotHeaders {
					if v == wantEffortFlag {
						hasEffortFlag = true
					}
				}
				if !hasEffortFlag {
					t.Errorf("expected -c %q in args (composition check), got -c values: %v", wantEffortFlag, gotHeaders)
				}
			}
		})
	}
}
