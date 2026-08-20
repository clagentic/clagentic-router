// internal/backend/errparse_test.go — tests for ClassifyError's pattern
// table (lr-c1d353).
//
// Covers two independently-safe fixes MILLER's diagnosis identified:
//  1. The ErrTypeNetwork "EOF" pattern was unreachable: ClassifyError
//     lowercases its input before matching, but the pattern was the literal
//     uppercase string "EOF", so strings.Contains(lower, "EOF") could never
//     be true. Fixed to "eof".
//  2. Several short/common patterns ("529", "billing", "api key",
//     "credential") were unanchored substrings that fire inside session
//     UUIDs, epoch timestamps, token counts, cost floats, tool names, and
//     file paths once the caller classifies a full stream-json payload
//     rather than short stderr-only text (lr-807319 widened the
//     classification window; this is the pre-existing matcher weakness that
//     widening exposed). "529" is anchored to require an adjacent status
//     word; "billing"/"api key"/"credential" require a word boundary.
package backend

import "testing"

func TestClassifyError_EOFPattern(t *testing.T) {
	got := ClassifyError("unexpected EOF", 1)
	if got != ErrTypeNetwork {
		t.Errorf("ClassifyError(%q, 1) = %q, want %q — EOF pattern is dead (case mismatch after lowercasing)", "unexpected EOF", got, ErrTypeNetwork)
	}
}

func TestClassifyError_EOFPattern_LowercaseInput(t *testing.T) {
	// Guard against a fix that only works for the exact-case reporter string.
	got := ClassifyError("connection closed: eof", 1)
	if got != ErrTypeNetwork {
		t.Errorf("ClassifyError(%q, 1) = %q, want %q", "connection closed: eof", got, ErrTypeNetwork)
	}
}

// TestClassifyError_RealisticStreamPayload_NoFalsePositive is the core
// regression test for the MILLER diagnosis: a realistic claude_cli
// stream-json init event, carrying a UUID, timestamps, and other numeric
// fields, must not misclassify as rate_limit/quota/auth. This is the exact
// artifact shape from lr-c1d353's production journal.
func TestClassifyError_RealisticStreamPayload_NoFalsePositive(t *testing.T) {
	payload := `{"type":"system","subtype":"init","cwd":"/home/akuehner/code/project-coldest-tea/repo","session_id":"41792fe9-a529-4b1e-9c3a-8e2b1d4f6c81","tools":["Read","Write","Bash"],"mcp_servers":[],"model":"claude-opus-4-7","permissionMode":"default","num_turns":2,"cost_usd":0.0529,"resets_at":1755718529}`

	got := ClassifyError(payload, 1)
	if got != ErrTypeUnknown {
		t.Errorf("ClassifyError(realistic init-event payload, 1) = %q, want %q — a UUID/timestamp/cost substring must not drive classification (lr-c1d353)", got, ErrTypeUnknown)
	}
}

// TestClassifyError_BareNumericSubstrings_DoNotMatchRateLimit specifically
// isolates the "529" false-positive MILLER identified as the confirmed
// cause of the observed unknown->rate_limit flip.
func TestClassifyError_BareNumericSubstrings_DoNotMatchRateLimit(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"uuid containing 529", "session_id 41792fe9-a529-4b1e-9c3a-8e2b1d4f6c81"},
		{"epoch resets_at containing 529", "resets_at 1755718529"},
		{"cost float containing 529", "cost_usd 0.00529"},
		{"token count containing 529", "input_tokens 15290"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyError(tc.text, 1)
			if got == ErrTypeRateLimit {
				t.Errorf("ClassifyError(%q, 1) = %q, want anything but rate_limit — bare '529' substring must not match", tc.text, got)
			}
		})
	}
}

// TestClassifyError_AnchoredRateLimitPatterns_StillMatch is the "do not
// weaken genuine detection" half of the anchoring fix: real 529 error text
// must still classify as rate_limit.
func TestClassifyError_AnchoredRateLimitPatterns_StillMatch(t *testing.T) {
	cases := []string{
		"received status 529 from upstream",
		"HTTP 529 overloaded, please retry",
		"error: 529 overloaded",
	}
	for _, text := range cases {
		t.Run(text, func(t *testing.T) {
			got := ClassifyError(text, 1)
			if got != ErrTypeRateLimit {
				t.Errorf("ClassifyError(%q, 1) = %q, want %q", text, got, ErrTypeRateLimit)
			}
		})
	}
}

// TestClassifyError_Billing_WordBoundary covers the ErrTypeQuota "billing"
// pattern: a tool/file path containing "billing" as a substring of a larger
// identifier must not match, but real billing-error prose still does.
func TestClassifyError_Billing_WordBoundary(t *testing.T) {
	if got := ClassifyError(`tool call: mcp__billing-internal-helper__lookup`, 1); got == ErrTypeQuota {
		t.Errorf("ClassifyError(tool name containing 'billing' substring) = %q, want anything but quota", got)
	}
	if got := ClassifyError("your billing account is past due", 1); got != ErrTypeQuota {
		t.Errorf("ClassifyError(real billing error) = %q, want %q", got, ErrTypeQuota)
	}
}

// TestClassifyError_ApiKeyCredential_WordBoundary covers the ErrTypeAuth
// "api key"/"credential" patterns — MILLER flagged these as especially
// dangerous because ErrTypeAuth drives a hard StatusOffline transition.
// The false-positive surface for these two patterns is a longer identifier
// that merely CONTAINS the substring (e.g. "credentialSync", a field/tool
// name) — not a sentence that genuinely uses the word "credential", which
// is inherently closer to a real auth-adjacent signal and is not the
// substring-vs-word-boundary defect this fix targets.
func TestClassifyError_ApiKeyCredential_WordBoundary(t *testing.T) {
	if got := ClassifyError(`"tool":"syncSubprocessCredentialsHelper"`, 1); got == ErrTypeAuth {
		t.Errorf("ClassifyError(identifier containing 'credential' substring) = %q, want anything but auth", got)
	}
	if got := ClassifyError(`"field":"apiKeyRotationScheduleMs"`, 1); got == ErrTypeAuth {
		t.Errorf("ClassifyError(identifier containing 'apikey' substring) = %q, want anything but auth", got)
	}
	if got := ClassifyError("invalid api key provided", 1); got != ErrTypeAuth {
		t.Errorf("ClassifyError(real api key error) = %q, want %q", got, ErrTypeAuth)
	}
	if got := ClassifyError("authentication failed: credential expired", 1); got != ErrTypeAuth {
		t.Errorf("ClassifyError(real credential error) = %q, want %q", got, ErrTypeAuth)
	}
}

// TestClassifyError_QuotaOutranksRateLimit preserves the pre-existing
// first-match-in-slice-order priority (Quota before RateLimit) once the
// word-boundary patterns are checked inline rather than in the flat
// substring list.
func TestClassifyError_QuotaOutranksRateLimit(t *testing.T) {
	got := ClassifyError("quota exceeded and rate limit hit", 1)
	if got != ErrTypeQuota {
		t.Errorf("ClassifyError(quota+rate_limit text) = %q, want %q (quota must win by slice order)", got, ErrTypeQuota)
	}
}
