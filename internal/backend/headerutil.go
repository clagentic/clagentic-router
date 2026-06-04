// internal/backend/headerutil.go — shared HTTP response header parsing helpers.
//
// These helpers are used by openai_api.go and anthropic_api.go to extract
// rate-limit window data from provider response headers. All functions are
// best-effort: absent or unparseable headers return zero values rather than
// errors, keeping the hot path clean.
package backend

import (
	"net/http"
	"strconv"
	"time"
)

// parseIntHeader parses a header value as int64.
// Returns 0 if the header is absent or not a valid integer.
func parseIntHeader(h http.Header, key string) int64 {
	v := h.Get(key)
	if v == "" {
		return 0
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// parseDurationResetHeader parses an OpenAI-style duration reset header (e.g. "6m0s",
// "500ms") relative to now. Returns zero time if the header is absent or unparseable.
func parseDurationResetHeader(h http.Header, key string) time.Time {
	v := h.Get(key)
	if v == "" {
		return time.Time{}
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return time.Time{}
	}
	return time.Now().Add(d)
}

// parseRFC3339Header parses an Anthropic-style RFC 3339 timestamp header.
// Returns zero time if the header is absent or unparseable.
func parseRFC3339Header(h http.Header, key string) time.Time {
	v := h.Get(key)
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}
	}
	return t
}
