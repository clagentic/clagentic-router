// internal/config/bedrock_validate_test.go — validation tests for the
// bedrock_api adapter's required region field.
package config

import "testing"

// TestValidate_BedrockAPI_RequiresRegion verifies that a bedrock_api backend
// without a region fails config validation loudly rather than deferring the
// failure to the first call (Bedrock has no SDK default region).
func TestValidate_BedrockAPI_RequiresRegion(t *testing.T) {
	cfg := &Config{
		Backends: map[string]*BackendConfig{
			"bedrock-1": {
				Adapter: AdapterBedrockAPI,
				Model:   "anthropic.claude-sonnet-4-6",
			},
		},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected error for bedrock_api backend with no region, got nil")
	}
}

// TestValidate_BedrockAPI_WithRegionPasses verifies that a bedrock_api backend
// with a region set passes validation.
func TestValidate_BedrockAPI_WithRegionPasses(t *testing.T) {
	cfg := &Config{
		Backends: map[string]*BackendConfig{
			"bedrock-1": {
				Adapter: AdapterBedrockAPI,
				Model:   "anthropic.claude-sonnet-4-6",
				Region:  "us-east-1",
			},
		},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestValidate_UnknownAdapter verifies the adapter allowlist still rejects
// unrecognized adapter types after bedrock_api was added — guards against a
// typo in the switch statement silently widening the allowlist.
func TestValidate_UnknownAdapter(t *testing.T) {
	cfg := &Config{
		Backends: map[string]*BackendConfig{
			"bad": {Adapter: "not_a_real_adapter"},
		},
	}
	if err := cfg.validate(); err == nil {
		t.Fatal("expected error for unknown adapter type, got nil")
	}
}
