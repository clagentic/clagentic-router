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

// billingWordRe matches "billing" as a whole word (case-insensitive), not as
// a substring of an unrelated identifier (tool name, file path, MCP server
// name) that happens to contain it. See ClassifyError's doc for why this
// pattern is a regex while the others remain plain substrings.
var billingWordRe = regexp.MustCompile(`(?i)\bbilling\b`)

// apiKeyWordRe matches "api key" as a whole phrase (case-insensitive),
// bounded so it does not fire inside a longer token such as a tool or
// parameter name that merely contains the substring "api key".
var apiKeyWordRe = regexp.MustCompile(`(?i)\bapi[ _-]?key\b`)

// credentialWordRe matches "credential"/"credentials" as a whole word
// (case-insensitive). Unbounded substring matching on "credential" fires on
// any tool description, file path, or CLAUDE.md prose that mentions
// credentials without describing an actual auth failure (MILLER,
// lr-c1d353) — ErrTypeAuth drives a hard StatusOffline transition
// (state.go), so a false positive here is not cosmetic.
var credentialWordRe = regexp.MustCompile(`(?i)\bcredentials?\b`)

var errorPatterns = []errorPattern{
	// Quota / credit exhaustion — hard limits
	{ErrTypeQuota, []string{
		"usage limit reached",
		"you've reached your message limit",
		"you have reached your message limit",
		"credit balance",
		"insufficient_quota",
		"quota exceeded",
		"account has insufficient credits",
	}},
	// Rate limiting — soft window limits
	{ErrTypeRateLimit, []string{
		"rate limit",
		"rate_limit_exceeded",
		"too many requests",
		"overloaded_error",
		"overloaded",
		"status 529",
		"http 529",
		"529 overloaded",
		"try again in",
		"please slow down",
	}},
	// Authentication failures — permanent until operator fixes
	{ErrTypeAuth, []string{
		"authentication_error",
		"unauthorized",
		"invalid api key",
		"invalid_api_key",
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
		"eof",
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
//
// Word-boundary patterns (billing, api key, credential) are checked inline,
// interleaved with the plain-substring errorPatterns table, so slice/table
// ordering (Quota before RateLimit before Auth before Network before
// Timeout — first match wins) is preserved exactly as before this change;
// see errparse.go's package doc and lr-c1d353 for why an incidental
// "billing" or "credential" substring must not outrank a real match earlier
// in that priority order, nor a later one for a pattern this input doesn't
// actually contain.
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
		// Word-boundary patterns belonging to this same ErrorType, checked
		// at this point in slice order so first-match-wins priority across
		// types is unaffected by moving them out of the plain-substring list.
		switch p.typ {
		case ErrTypeQuota:
			if billingWordRe.MatchString(stderr) {
				return ErrTypeQuota
			}
		case ErrTypeAuth:
			if apiKeyWordRe.MatchString(stderr) || credentialWordRe.MatchString(stderr) {
				return ErrTypeAuth
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
