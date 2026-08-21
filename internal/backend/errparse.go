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

// exitCodePatternNotFound / exitCodePatternTimeout are the matched_pattern_id
// values ClassifyErrorWithPattern reports for the exitCode 127/124
// fast-path returns below, which classify on exit code alone and never
// consult errorPatterns at all — a bare "" would be indistinguishable from
// the ErrTypeUnknown/no-match case in a journal line, so these name the
// actual (non-table) signal that drove classification.
const (
	exitCodePatternNotFound = "exit_code=127"
	exitCodePatternTimeout  = "exit_code=124"

	// billingPatternID / apiKeyPatternID / credentialPatternID are the
	// matched_pattern_id values for the three word-boundary regex patterns,
	// which match a regex rather than one literal string out of a
	// errorPattern.matches list, so they are named by matcher identity
	// instead of by matched substring.
	billingPatternID    = "billing"
	apiKeyPatternID     = "api_key"
	credentialPatternID = "credential"
)

// ClassifyError returns the ErrorType for the given stderr text and exit
// code. It is a thin wrapper around ClassifyErrorWithPattern for callers
// that don't need the matched pattern id.
func ClassifyError(stderr string, exitCode int) ErrorType {
	typ, _ := ClassifyErrorWithPattern(stderr, exitCode)
	return typ
}

// ClassifyErrorWithPattern returns the ErrorType for the given stderr text
// and exit code, alongside the id of the errorPatterns entry that drove the
// classification (lr-151fa7). exitCode 127 = binary not found; 124 = killed
// by timeout command — both classify on exit code alone, never touching
// errorPatterns, and report a synthetic id naming that fact (see
// exitCodePatternNotFound/exitCodePatternTimeout) rather than "". The
// returned patternID is "" when the result is ErrTypeUnknown, i.e. nothing
// matched.
//
// patternID is the pattern ITSELF (the matched substring, e.g. "rate
// limit", "529 overloaded"), or one of the three word-boundary regex names
// ("billing"/"api_key"/"credential") — never a copy of the surrounding
// classified text. This is the trust-boundary distinction DRUMMER's
// decision draws: matched_pattern_id names WHICH matcher fired and is safe
// to log at Info; the classified text itself may carry model prose, session
// ids, or secrets and is gated separately (see ClassifiedTextExcerpt).
//
// Word-boundary patterns (billing, api key, credential) are checked inline,
// interleaved with the plain-substring errorPatterns table, so slice/table
// ordering (Quota before RateLimit before Auth before Network before
// Timeout — first match wins) is preserved exactly as before this change;
// see errparse.go's package doc and lr-c1d353 for why an incidental
// "billing" or "credential" substring must not outrank a real match earlier
// in that priority order, nor a later one for a pattern this input doesn't
// actually contain.
func ClassifyErrorWithPattern(stderr string, exitCode int) (ErrorType, string) {
	if exitCode == 127 {
		return ErrTypeNotFound, exitCodePatternNotFound
	}
	if exitCode == 124 {
		return ErrTypeTimeout, exitCodePatternTimeout
	}

	lower := strings.ToLower(stderr)
	for _, p := range errorPatterns {
		for _, m := range p.matches {
			if strings.Contains(lower, m) {
				return p.typ, m
			}
		}
		// Word-boundary patterns belonging to this same ErrorType, checked
		// at this point in slice order so first-match-wins priority across
		// types is unaffected by moving them out of the plain-substring list.
		switch p.typ {
		case ErrTypeQuota:
			if billingWordRe.MatchString(stderr) {
				return ErrTypeQuota, billingPatternID
			}
		case ErrTypeAuth:
			if apiKeyWordRe.MatchString(stderr) {
				return ErrTypeAuth, apiKeyPatternID
			}
			if credentialWordRe.MatchString(stderr) {
				return ErrTypeAuth, credentialPatternID
			}
		}
	}
	return ErrTypeUnknown, ""
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
