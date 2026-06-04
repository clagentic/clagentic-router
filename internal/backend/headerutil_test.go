// internal/backend/headerutil_test.go — unit tests for header parse helpers.
package backend

import (
	"net/http"
	"testing"
	"time"
)

func TestParseIntHeader(t *testing.T) {
	cases := []struct {
		name   string
		header string
		value  string
		want   int64
	}{
		{"happy path positive", "x-ratelimit-remaining-tokens", "5000", 5000},
		{"zero value", "x-ratelimit-remaining-tokens", "0", 0},
		{"absent header", "x-ratelimit-remaining-tokens", "", 0},
		{"not a number", "x-ratelimit-remaining-tokens", "abc", 0},
		{"float string", "x-ratelimit-remaining-tokens", "3.14", 0},
		{"negative value", "x-ratelimit-remaining-tokens", "-1", -1},
		{"large value", "x-ratelimit-remaining-tokens", "1000000", 1_000_000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := http.Header{}
			if tc.value != "" {
				h.Set(tc.header, tc.value)
			}
			got := parseIntHeader(h, tc.header)
			if got != tc.want {
				t.Errorf("parseIntHeader(%q) = %d, want %d", tc.value, got, tc.want)
			}
		})
	}
}

func TestParseDurationResetHeader(t *testing.T) {
	now := time.Now()

	t.Run("absent header returns zero time", func(t *testing.T) {
		h := http.Header{}
		got := parseDurationResetHeader(h, "x-ratelimit-reset-tokens")
		if !got.IsZero() {
			t.Errorf("expected zero time, got %v", got)
		}
	})

	t.Run("empty string returns zero time", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-ratelimit-reset-tokens", "")
		got := parseDurationResetHeader(h, "x-ratelimit-reset-tokens")
		if !got.IsZero() {
			t.Errorf("expected zero time, got %v", got)
		}
	})

	t.Run("unparseable value returns zero time", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-ratelimit-reset-tokens", "not-a-duration")
		got := parseDurationResetHeader(h, "x-ratelimit-reset-tokens")
		if !got.IsZero() {
			t.Errorf("expected zero time, got %v", got)
		}
	})

	t.Run("6m0s parses to ~6 minutes from now", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-ratelimit-reset-tokens", "6m0s")
		got := parseDurationResetHeader(h, "x-ratelimit-reset-tokens")
		want := now.Add(6 * time.Minute)
		// Allow 2 second tolerance for test execution time
		diff := got.Sub(want)
		if diff < -2*time.Second || diff > 2*time.Second {
			t.Errorf("expected ~%v, got %v (diff %v)", want, got, diff)
		}
	})

	t.Run("500ms parses to ~500ms from now", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-ratelimit-reset-tokens", "500ms")
		got := parseDurationResetHeader(h, "x-ratelimit-reset-tokens")
		want := now.Add(500 * time.Millisecond)
		diff := got.Sub(want)
		if diff < -2*time.Second || diff > 2*time.Second {
			t.Errorf("expected ~%v, got %v (diff %v)", want, got, diff)
		}
	})

	t.Run("0s parses to ~now", func(t *testing.T) {
		h := http.Header{}
		h.Set("x-ratelimit-reset-tokens", "0s")
		got := parseDurationResetHeader(h, "x-ratelimit-reset-tokens")
		diff := got.Sub(now)
		if diff < -2*time.Second || diff > 2*time.Second {
			t.Errorf("expected ~now, got %v (diff %v)", got, diff)
		}
	})
}

func TestParseRFC3339Header(t *testing.T) {
	t.Run("absent header returns zero time", func(t *testing.T) {
		h := http.Header{}
		got := parseRFC3339Header(h, "anthropic-ratelimit-tokens-reset")
		if !got.IsZero() {
			t.Errorf("expected zero time, got %v", got)
		}
	})

	t.Run("empty string returns zero time", func(t *testing.T) {
		h := http.Header{}
		h.Set("anthropic-ratelimit-tokens-reset", "")
		got := parseRFC3339Header(h, "anthropic-ratelimit-tokens-reset")
		if !got.IsZero() {
			t.Errorf("expected zero time, got %v", got)
		}
	})

	t.Run("unparseable value returns zero time", func(t *testing.T) {
		h := http.Header{}
		h.Set("anthropic-ratelimit-tokens-reset", "not-a-timestamp")
		got := parseRFC3339Header(h, "anthropic-ratelimit-tokens-reset")
		if !got.IsZero() {
			t.Errorf("expected zero time, got %v", got)
		}
	})

	t.Run("valid RFC3339 timestamp", func(t *testing.T) {
		ts := "2026-05-28T15:04:05Z"
		want, _ := time.Parse(time.RFC3339, ts)
		h := http.Header{}
		h.Set("anthropic-ratelimit-tokens-reset", ts)
		got := parseRFC3339Header(h, "anthropic-ratelimit-tokens-reset")
		if !got.Equal(want) {
			t.Errorf("expected %v, got %v", want, got)
		}
	})

	t.Run("RFC3339 with timezone offset", func(t *testing.T) {
		ts := "2026-05-28T10:04:05-05:00"
		want, _ := time.Parse(time.RFC3339, ts)
		h := http.Header{}
		h.Set("anthropic-ratelimit-tokens-reset", ts)
		got := parseRFC3339Header(h, "anthropic-ratelimit-tokens-reset")
		if !got.Equal(want) {
			t.Errorf("expected %v, got %v", want, got)
		}
	})
}
