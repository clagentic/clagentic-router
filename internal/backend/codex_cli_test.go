// internal/backend/codex_cli_test.go — tests for the CodexCLIAdapter's optional
// OpenAI-Project header injection via -c model_providers.<id>.http_headers.
//
// Uses the fake-binary-argv pattern from cli_model_passthrough_test.go: a fake
// codex binary records its argv to a file, and the test inspects it. All ids
// below are fabricated placeholders, not real model or project ids.
package backend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeCodexBinWithEnvDump writes a fake "codex" binary that dumps its
// own subprocess environment (via `env`) to envFile, then emits stdout.
// Exercises CodexCLIAdapter.Invoke's actual cmd.Env at the real
// exec.CommandContext call site — TestBuildCLIEnv_CodexCLIBlocksSecrets in
// env_test.go tests buildCLIEnv in isolation and would not, by itself, have
// caught a call site that built the env but never assigned it to cmd.Env
// (the codex_model_discovery.go leak this task fixed) or never called
// buildCLIEnv at all (the original lr-bd5dc0 defect in this adapter).
func writeFakeCodexBinWithEnvDump(t *testing.T, dir, envFile, stdout string) string {
	t.Helper()
	binPath := filepath.Join(dir, "codex")
	script := "#!/bin/sh\n" +
		"env > " + envFile + "\n" +
		"printf '%s' " + shellQuote(stdout) + "\n"
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("write fake codex bin: %v", err)
	}
	return binPath
}

// TestCodexCLI_Invoke_SubprocessEnvFiltered proves, at the actual Invoke
// call site, that the codex_cli subprocess's real cmd.Env is filtered
// through buildCLIEnv: a daemon secret must not appear in the subprocess
// environment, while HOME/CODEX_HOME survive (lr-bd5dc0).
func TestCodexCLI_Invoke_SubprocessEnvFiltered(t *testing.T) {
	os.Setenv("CLAGENTIC_ROUTER_TOKEN", "super-secret-token")
	os.Setenv("OPENAI_API_KEY", "sk-openai-test")
	defer func() {
		os.Unsetenv("CLAGENTIC_ROUTER_TOKEN")
		os.Unsetenv("OPENAI_API_KEY")
	}()

	origHome := os.Getenv("HOME")
	if origHome == "" {
		os.Setenv("HOME", "/root")
		defer os.Setenv("HOME", origHome)
	}
	os.Setenv("CODEX_HOME", "/home/user/.codex")
	defer os.Unsetenv("CODEX_HOME")

	dir := t.TempDir()
	envFile := filepath.Join(dir, "env.txt")
	bin := writeFakeCodexBinWithEnvDump(t, dir, envFile, "response from codex")

	adapter := NewCodexCLIAdapter("test", "", "", "", "", bin)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read subprocess env dump: %v", err)
	}
	subprocessEnv := string(data)

	for _, secret := range []string{
		"CLAGENTIC_ROUTER_TOKEN=super-secret-token",
		"OPENAI_API_KEY=sk-openai-test",
	} {
		if strings.Contains(subprocessEnv, secret) {
			t.Errorf("secret var leaked into codex_cli subprocess env: %q", secret)
		}
	}

	for _, prefix := range []string{"HOME=", "CODEX_HOME="} {
		if !strings.Contains(subprocessEnv, "\n"+prefix) && !strings.HasPrefix(subprocessEnv, prefix) {
			t.Errorf("%s missing from codex_cli subprocess env — would break auth.json resolution", prefix)
		}
	}
}

// TestCodexCLI_Invoke_SubprocessEnvAWSCredentialsSurvive proves, at the
// actual Invoke call site, that AWS SDK standard credential/config env vars
// survive buildCLIEnv's filter — a live regression this test guards
// against: codex_cli routed through buildCLIEnv for the first time in
// lr-bd5dc0, and the allowlist had never contained an AWS_ entry, so a
// Bedrock-env-authed host (AWS_PROFILE/AWS_REGION set, no ~/.aws/credentials
// file) failed 100% of codex exec invocations with "AWS SDK config did not
// resolve a region". Composed with the same secret-blocking assertions as
// TestCodexCLI_Invoke_SubprocessEnvFiltered to cover both directions in one
// real subprocess env dump.
func TestCodexCLI_Invoke_SubprocessEnvAWSCredentialsSurvive(t *testing.T) {
	os.Setenv("CLAGENTIC_ROUTER_TOKEN", "super-secret-token")
	os.Setenv("AWS_PROFILE", "test-profile")
	os.Setenv("AWS_REGION", "us-test-1")
	os.Setenv("AWS_SECRET_ACCESS_KEY", "fake-secret-access-key")
	os.Setenv("AWS_SESSION_TOKEN", "fake-session-token")
	defer func() {
		os.Unsetenv("CLAGENTIC_ROUTER_TOKEN")
		os.Unsetenv("AWS_PROFILE")
		os.Unsetenv("AWS_REGION")
		os.Unsetenv("AWS_SECRET_ACCESS_KEY")
		os.Unsetenv("AWS_SESSION_TOKEN")
	}()

	origHome := os.Getenv("HOME")
	if origHome == "" {
		os.Setenv("HOME", "/root")
		defer os.Setenv("HOME", origHome)
	}

	dir := t.TempDir()
	envFile := filepath.Join(dir, "env.txt")
	bin := writeFakeCodexBinWithEnvDump(t, dir, envFile, "response from codex")

	adapter := NewCodexCLIAdapter("test", "", "", "", "", bin)
	req := &Request{Messages: []Message{{Role: "user", Content: "ping"}}}

	if _, err := adapter.Invoke(context.Background(), req); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	data, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("read subprocess env dump: %v", err)
	}
	subprocessEnv := string(data)

	if strings.Contains(subprocessEnv, "CLAGENTIC_ROUTER_TOKEN=super-secret-token") {
		t.Error("router token leaked into codex_cli subprocess env")
	}

	for _, want := range []string{
		"AWS_PROFILE=test-profile",
		"AWS_REGION=us-test-1",
		"AWS_SECRET_ACCESS_KEY=fake-secret-access-key",
		"AWS_SESSION_TOKEN=fake-session-token",
	} {
		if !strings.Contains(subprocessEnv, want) {
			t.Errorf("%s missing from codex_cli subprocess env — Bedrock-env auth would fail", want)
		}
	}
}

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
