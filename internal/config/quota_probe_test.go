// internal/config/quota_probe_test.go — tests for QuotaProbeConfig yaml unmarshaling.
package config

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestDuration_UnmarshalYAML_ValidStrings verifies that duration strings round-trip.
func TestDuration_UnmarshalYAML_ValidStrings(t *testing.T) {
	cases := []struct {
		input string
		want  time.Duration
	}{
		{"30m", 30 * time.Minute},
		{"1h", time.Hour},
		{"15s", 15 * time.Second},
		{"2h30m", 2*time.Hour + 30*time.Minute},
		{"", 0},
	}
	for _, tc := range cases {
		raw := "interval: \"" + tc.input + "\""
		if tc.input == "" {
			raw = "interval: \"\""
		}
		var cfg QuotaProbeConfig
		if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
			t.Errorf("input %q: unexpected error: %v", tc.input, err)
			continue
		}
		got := time.Duration(cfg.Interval)
		if got != tc.want {
			t.Errorf("input %q: got %v, want %v", tc.input, got, tc.want)
		}
	}
}

// TestDuration_UnmarshalYAML_InvalidString verifies that an invalid duration string
// causes an unmarshal error.
func TestDuration_UnmarshalYAML_InvalidString(t *testing.T) {
	raw := `interval: "notaduration"`
	var cfg QuotaProbeConfig
	if err := yaml.Unmarshal([]byte(raw), &cfg); err == nil {
		t.Error("expected error for invalid duration string, got nil")
	}
}

// TestQuotaProbeConfig_FullUnmarshal verifies that a full quota_probe block
// round-trips correctly from YAML.
func TestQuotaProbeConfig_FullUnmarshal(t *testing.T) {
	raw := `
enabled: true
interval: "45m"
model: claude-haiku-4-5
`
	var cfg QuotaProbeConfig
	if err := yaml.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Enabled {
		t.Error("Enabled = false, want true")
	}
	if got := time.Duration(cfg.Interval); got != 45*time.Minute {
		t.Errorf("Interval = %v, want 45m", got)
	}
	if cfg.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q, want %q", cfg.Model, "claude-haiku-4-5")
	}
}

// TestQuotaProbeConfig_Defaults verifies that zero-value fields report zero,
// and callers should apply defaults (interval=30m, model=haiku).
func TestQuotaProbeConfig_Defaults(t *testing.T) {
	var cfg QuotaProbeConfig
	if cfg.Enabled {
		t.Error("default Enabled should be false")
	}
	if time.Duration(cfg.Interval) != 0 {
		t.Errorf("default Interval should be 0, got %v", time.Duration(cfg.Interval))
	}
	if cfg.Model != "" {
		t.Errorf("default Model should be empty, got %q", cfg.Model)
	}
}
