// cmd/clagentic-router/auth_gates_test.go — regression tests for lr-7a26e0:
// the boot-time refuse-to-start-unauthenticated gate for both the inference
// token and the admin token.
package main

import "testing"

func TestCheckAuthGates_EmptyInferenceToken_UnsafeNoAuthFalse_Errors(t *testing.T) {
	err := checkAuthGates("", "admin-tok", false)
	if err == nil {
		t.Fatal("expected error for empty inference token without --unsafe-no-auth, got nil")
	}
}

func TestCheckAuthGates_EmptyAdminToken_UnsafeNoAuthFalse_Errors(t *testing.T) {
	err := checkAuthGates("inference-tok", "", false)
	if err == nil {
		t.Fatal("expected error for empty admin token without --unsafe-no-auth, got nil")
	}
}

func TestCheckAuthGates_BothEmpty_UnsafeNoAuthTrue_NoError(t *testing.T) {
	err := checkAuthGates("", "", true)
	if err != nil {
		t.Errorf("expected nil error with --unsafe-no-auth, got: %v", err)
	}
}

func TestCheckAuthGates_BothSet_NoError(t *testing.T) {
	err := checkAuthGates("inference-tok", "admin-tok", false)
	if err != nil {
		t.Errorf("expected nil error when both tokens are set, got: %v", err)
	}
}

func TestCheckAuthGates_EmptyAdminToken_FallsBackToInferenceToken_NoError(t *testing.T) {
	// Mirrors production behavior: ResolvedAdminToken() falls back to
	// ResolvedToken() when admin_token is unset, so a caller passing the
	// already-resolved admin token (equal to the inference token) must not
	// trip the gate.
	err := checkAuthGates("inference-tok", "inference-tok", false)
	if err != nil {
		t.Errorf("expected nil error when adminToken falls back to inference token, got: %v", err)
	}
}
