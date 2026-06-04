// internal/backend/errparse.go — error classification for LLM CLI output.
//
// Classifies stderr/stdout from claude and codex CLIs into ErrorType values
// so the router can make the correct state machine transition.
// Also parses reset times when present in error messages.
package backend

import (
	"regexp"
	"strings"
	"time"
)

// errorPattern is a single classification rule.
type errorPattern struct {
	typ     ErrorType
	matches []string // substrings to check (case-insensitive OR logic)
}

var errorPatterns = []errorPattern{
	// Quota / credit exhaustion — hard limits
	{ErrTypeQuota, []string{
		"usage limit reached",
		"you've reached your message limit",
		"you have reached your message limit",
		"credit balance",
		"insufficient_quota",
		"quota exceeded",
		"billing",
		"account has insufficient credits",
	}},
	// Rate limiting — soft window limits
	{ErrTypeRateLimit, []string{
		"rate limit",
		"rate_limit_exceeded",
		"too many requests",
		"overloaded_error",
		"overloaded",
		"529",
		"try again in",
		"please slow down",
	}},
	// Authentication failures — permanent until operator fixes
	{ErrTypeAuth, []string{
		"authentication_error",
		"unauthorized",
		"invalid api key",
		"invalid_api_key",
		"api key",
		"credential",
		"not logged in",
		"login required",
	}},
	// Network failures — transient
	{ErrTypeNetwork, []string{
		"connection refused",
		"no route to host",
		"network unreachable",
		"dial tcp",
		"connection reset",
		"EOF",
		"i/o timeout",
	}},
	// Timeout — from context deadline or OS timeout
	{ErrTypeTimeout, []string{
		"timed out",
		"context deadline exceeded",
		"deadline exceeded",
	}},
}

// ClassifyError returns the ErrorType for the given stderr text and exit code.
// exitCode 127 = binary not found; 124 = killed by timeout command.
func ClassifyError(stderr string, exitCode int) ErrorType {
	if exitCode == 127 {
		return ErrTypeNotFound
	}
	if exitCode == 124 {
		return ErrTypeTimeout
	}

	lower := strings.ToLower(stderr)
	for _, p := range errorPatterns {
		for _, m := range p.matches {
			if strings.Contains(lower, m) {
				return p.typ
			}
		}
	}
	return ErrTypeUnknown
}

// resetPatterns are regexes that extract a reset time from error output.
var resetPatterns = []*regexp.Regexp{
	// "resets at 5:00 AM ET" or "reset at 5:00 AM ET"
	regexp.MustCompile(`(?i)reset(?:s)? at (\d{1,2}:\d{2} [AP]M \w+)`),
	// "quota will reset after 5s" — relative
	regexp.MustCompile(`(?i)reset(?:s)? after (\d+)s`),
	// ISO-8601
	regexp.MustCompile(`(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z)`),
}

// ParseResetTime tries to extract an absolute reset time from error output.
// Returns zero time if no reset time is found or parseable.
func ParseResetTime(stderr string) time.Time {
	for _, re := range resetPatterns {
		m := re.FindStringSubmatch(stderr)
		if m == nil {
			continue
		}
		// Try ISO-8601
		if t, err := time.Parse(time.RFC3339, m[1]); err == nil {
			return t.UTC()
		}
		// "5s" relative offset
		if strings.HasSuffix(m[1], "s") {
			// ParseResetTime is best-effort; skip complex relative parsing
		}
	}
	return time.Time{}
}
