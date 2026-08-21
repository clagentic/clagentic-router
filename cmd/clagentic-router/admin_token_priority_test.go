// cmd/clagentic-router/admin_token_priority_test.go — regression tests for
// lr-92ee18 PEACHES re-review (comment 5371343493, finding 1): in a
// split-token deployment where both CLAGENTIC_ROUTER_TOKEN and
// CLAGENTIC_ROUTER_ADMIN_TOKEN are set, admin subcommands (health, doctor,
// quota, logs, metrics, backend reset/disable/enable) must resolve and send
// the admin token, not the inference token — the server's adminAuth
// middleware only accepts the admin token on those routes.
package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestTokenEnvVarPriority_Admin verifies ADMIN_TOKEN is checked before
// TOKEN for admin subcommands.
func TestTokenEnvVarPriority_Admin(t *testing.T) {
	got := tokenEnvVarPriority(true)
	want := []string{"CLAGENTIC_ROUTER_ADMIN_TOKEN", "CLAGENTIC_ROUTER_TOKEN"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("tokenEnvVarPriority(true) = %v, want %v", got, want)
	}
}

// TestTokenEnvVarPriority_Inference verifies TOKEN is checked before
// ADMIN_TOKEN for inference subcommands (unchanged, pre-existing priority).
func TestTokenEnvVarPriority_Inference(t *testing.T) {
	got := tokenEnvVarPriority(false)
	want := []string{"CLAGENTIC_ROUTER_TOKEN", "CLAGENTIC_ROUTER_ADMIN_TOKEN"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("tokenEnvVarPriority(false) = %v, want %v", got, want)
	}
}

// TestParseAdminClientFlags_PrefersAdminTokenEnvVar verifies that with both
// CLAGENTIC_ROUTER_TOKEN and CLAGENTIC_ROUTER_ADMIN_TOKEN set in the
// caller's shell, parseAdminClientFlags resolves CLAGENTIC_ROUTER_ADMIN_TOKEN
// — the exact split-token scenario cited in the PEACHES finding.
func TestParseAdminClientFlags_PrefersAdminTokenEnvVar(t *testing.T) {
	t.Setenv("CLAGENTIC_ROUTER_TOKEN", "inference-tok")
	t.Setenv("CLAGENTIC_ROUTER_ADMIN_TOKEN", "admin-tok")

	f, _, err := parseAdminClientFlags(nil)
	if err != nil {
		t.Fatalf("parseAdminClientFlags: %v", err)
	}
	if f.token != "admin-tok" {
		t.Errorf("token = %q, want %q (admin token must win for an admin subcommand)", f.token, "admin-tok")
	}
	if f.tokenSource != "env:CLAGENTIC_ROUTER_ADMIN_TOKEN" {
		t.Errorf("tokenSource = %q, want %q", f.tokenSource, "env:CLAGENTIC_ROUTER_ADMIN_TOKEN")
	}
}

// TestParseClientFlags_StillPrefersTokenEnvVar verifies the inference-token
// path (parseClientFlags, used by "call") keeps its original TOKEN-first
// priority — this fix must not flip the order for the one subcommand that
// actually authenticates with the inference token.
func TestParseClientFlags_StillPrefersTokenEnvVar(t *testing.T) {
	t.Setenv("CLAGENTIC_ROUTER_TOKEN", "inference-tok")
	t.Setenv("CLAGENTIC_ROUTER_ADMIN_TOKEN", "admin-tok")

	f, _, err := parseClientFlags(nil)
	if err != nil {
		t.Fatalf("parseClientFlags: %v", err)
	}
	if f.token != "inference-tok" {
		t.Errorf("token = %q, want %q (inference token must win for an inference subcommand)", f.token, "inference-tok")
	}
	if f.tokenSource != "env:CLAGENTIC_ROUTER_TOKEN" {
		t.Errorf("tokenSource = %q, want %q", f.tokenSource, "env:CLAGENTIC_ROUTER_TOKEN")
	}
}

// TestCmdGet_SplitTokenDeployment_BothEnvVarsSet_AdminTokenWins is an
// end-to-end regression test for the PEACHES finding: with a real HTTP
// server that only accepts a specific "admin" bearer token on /health, and
// both CLAGENTIC_ROUTER_TOKEN (a different, wrong-for-this-route value) and
// CLAGENTIC_ROUTER_ADMIN_TOKEN set in the environment, cmdGet("health") must
// succeed — before this fix it 401'd because CLAGENTIC_ROUTER_TOKEN was
// checked (and sent) first.
func TestCmdGet_SplitTokenDeployment_BothEnvVarsSet_AdminTokenWins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin-tok" {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":"unauthorized"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	t.Setenv("CLAGENTIC_ROUTER_TOKEN", "inference-tok")
	t.Setenv("CLAGENTIC_ROUTER_ADMIN_TOKEN", "admin-tok")

	if err := cmdGet([]string{"--server", srv.URL}, "/health"); err != nil {
		t.Fatalf("cmdGet(/health) with split tokens set: %v (admin token was not preferred)", err)
	}
}
