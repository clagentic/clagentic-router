// internal/backend/failure_log_test.go — tests for ClassifiedTextExcerpt
// (lr-151fa7).
package backend

import (
	"strings"
	"testing"
)

// TestClassifiedTextExcerpt_WindowOnMatch covers the core case: a pattern
// match produces a window of up to 30 chars before + the match + up to 30
// chars after.
func TestClassifiedTextExcerpt_WindowOnMatch(t *testing.T) {
	text := "some preamble text here then rate limit exceeded and more trailing context follows after that"
	patternID := "rate limit"

	got := ClassifiedTextExcerpt(text, patternID)

	if !strings.Contains(got, patternID) {
		t.Fatalf("excerpt %q does not contain the matched pattern %q", got, patternID)
	}
	idx := strings.Index(text, patternID)
	wantStart := idx - 30
	if wantStart < 0 {
		wantStart = 0
	}
	wantEnd := idx + len(patternID) + 30
	if wantEnd > len(text) {
		wantEnd = len(text)
	}
	want := text[wantStart:wantEnd]
	if got != want {
		t.Errorf("excerpt = %q, want %q", got, want)
	}
}

// TestClassifiedTextExcerpt_NoMatch_FirstHundredChars covers
// ErrTypeUnknown's empty patternID case: first 100 chars of text.
func TestClassifiedTextExcerpt_NoMatch_FirstHundredChars(t *testing.T) {
	text := strings.Repeat("x", 150)
	got := ClassifiedTextExcerpt(text, "")
	want := text[:100]
	if got != want {
		t.Errorf("len(got)=%d, want 100", len(got))
	}
	if got != want {
		t.Errorf("excerpt = %q, want first 100 chars", got)
	}
}

// TestClassifiedTextExcerpt_NoMatch_ShortText covers text shorter than the
// 100-char no-match window — must return the whole string, not pad or error.
func TestClassifiedTextExcerpt_NoMatch_ShortText(t *testing.T) {
	text := "short"
	got := ClassifiedTextExcerpt(text, "")
	if got != text {
		t.Errorf("excerpt = %q, want %q", got, text)
	}
}

// TestClassifiedTextExcerpt_EmptyText covers the boundary case of an empty
// classification string — must not panic and must return "".
func TestClassifiedTextExcerpt_EmptyText(t *testing.T) {
	if got := ClassifiedTextExcerpt("", ""); got != "" {
		t.Errorf("excerpt = %q, want empty", got)
	}
	if got := ClassifiedTextExcerpt("", "rate limit"); got != "" {
		t.Errorf("excerpt = %q, want empty", got)
	}
}

// TestClassifiedTextExcerpt_HardCap200Chars covers DRUMMER's hard cap: even
// a maximal window (30 + long pattern + 30) must never exceed 200 total.
func TestClassifiedTextExcerpt_HardCap200Chars(t *testing.T) {
	longPattern := strings.Repeat("p", 300)
	text := strings.Repeat("a", 50) + longPattern + strings.Repeat("b", 50)

	got := ClassifiedTextExcerpt(text, longPattern)
	if len(got) > 200 {
		t.Errorf("excerpt len = %d, want <= 200", len(got))
	}
}

// TestClassifiedTextExcerpt_HardCap200Chars_NoMatchPath covers the hard cap
// on the no-match branch too — belt and suspenders against the 100-char
// no-match constant ever being raised above 200 without updating the cap.
func TestClassifiedTextExcerpt_HardCap200Chars_NoMatchPath(t *testing.T) {
	text := strings.Repeat("z", 5000)
	got := ClassifiedTextExcerpt(text, "")
	if len(got) > 200 {
		t.Errorf("excerpt len = %d, want <= 200", len(got))
	}
}

// TestClassifiedTextExcerpt_BinaryDataAndNullBytes covers DRUMMER's
// explicit boundary case: text containing binary/null-byte data must not
// panic and must still return a bounded result.
func TestClassifiedTextExcerpt_BinaryDataAndNullBytes(t *testing.T) {
	text := "prefix\x00\x01\x02binary\xffdata\x00rate limit\x00more\x00binary\xfe\xfd"
	got := ClassifiedTextExcerpt(text, "rate limit")
	if len(got) > 200 {
		t.Errorf("excerpt len = %d, want <= 200", len(got))
	}
	if !strings.Contains(got, "rate limit") {
		t.Errorf("excerpt %q lost the matched pattern amid binary data", got)
	}

	// No-match path with pure binary/null-byte input must not panic either.
	binaryOnly := "\x00\x01\x02\xff\xfe\xfd\x00\x00"
	if got := ClassifiedTextExcerpt(binaryOnly, ""); len(got) > 200 {
		t.Errorf("excerpt len = %d, want <= 200", len(got))
	}
}

// TestClassifiedTextExcerpt_RegexPatternIDCaseInsensitiveMatch covers a
// word-boundary regex pattern (id "billing", lowercase) found in mixed-case
// text — the excerpt must still center on the real occurrence rather than
// silently falling back to the no-match window.
func TestClassifiedTextExcerpt_RegexPatternIDCaseInsensitiveMatch(t *testing.T) {
	text := "Your Billing account requires attention immediately"
	got := ClassifiedTextExcerpt(text, billingPatternID)
	if !strings.Contains(strings.ToLower(got), "billing") {
		t.Errorf("excerpt %q did not center on the mixed-case Billing occurrence", got)
	}
}

// TestClassifiedTextExcerpt_PatternIDNotInText covers a patternID that does
// not literally occur in text (e.g. a synthetic exit-code id like
// "exit_code=127", or a mismatched caller) — must fall back to the
// no-match window rather than panicking or returning a nonsense slice.
func TestClassifiedTextExcerpt_PatternIDNotInText(t *testing.T) {
	text := "claude binary not found on PATH"
	got := ClassifiedTextExcerpt(text, exitCodePatternNotFound)
	want := text // shorter than 100 chars, so whole string
	if got != want {
		t.Errorf("excerpt = %q, want %q (no-match fallback)", got, want)
	}
}

// TestClassifiedTextExcerpt_MatchAtVeryStartAndEnd covers the start/end
// clamping logic: a match with fewer than 30 chars of context available on
// one side must not underflow/overflow the slice bounds.
func TestClassifiedTextExcerpt_MatchAtVeryStartAndEnd(t *testing.T) {
	// Match at the very start: no "before" context available.
	textStart := "rate limit exceeded, please retry after a short backoff period ends"
	got := ClassifiedTextExcerpt(textStart, "rate limit")
	if !strings.HasPrefix(got, "rate limit") {
		t.Errorf("excerpt = %q, want to start with the match (no chars available before it)", got)
	}

	// Match at the very end: no "after" context available.
	textEnd := "an unexpected failure occurred: rate limit"
	got2 := ClassifiedTextExcerpt(textEnd, "rate limit")
	if !strings.HasSuffix(got2, "rate limit") {
		t.Errorf("excerpt = %q, want to end with the match (no chars available after it)", got2)
	}
}

// ---- Raw-equals-classified-text window invariant (PR #61/lr-c1d353,
// carried forward per DRUMMER's decision item 3) ----

// TestClassifiedTextExcerpt_WindowOnSameStringAsRaw is the explicit test
// DRUMMER's decision requires: the excerpt must be a WINDOW on the exact
// same string that becomes InvokeError.Raw, never a separately-sourced
// copy. This constructs the same text->Raw pipeline claude_cli.go/
// codex_cli.go/codex_subagent.go/gemini_cli.go all use (truncate(text,
// 500)) and asserts every byte of the excerpt appears, contiguously, inside
// Raw.
func TestClassifiedTextExcerpt_WindowOnSameStringAsRaw(t *testing.T) {
	text := "stream preamble noise session_id=41792fe9 then the real signal: rate limit exceeded, please retry, followed by trailing telemetry fields cost_usd=0.05"

	errType, patternID := ClassifyErrorWithPattern(text, 1)
	if errType != ErrTypeRateLimit {
		t.Fatalf("setup: errType = %q, want %q", errType, ErrTypeRateLimit)
	}

	raw := truncate(text, 500) // identical construction to every adapter's Raw field
	excerpt := ClassifiedTextExcerpt(text, patternID)

	if raw != text {
		t.Fatalf("setup: text shorter than 500 chars must truncate to itself, got %q", raw)
	}
	if !strings.Contains(raw, excerpt) {
		t.Errorf("excerpt %q is not a contiguous substring of Raw %q — window invariant violated", excerpt, raw)
	}
}

// TestClassifiedTextExcerpt_WindowOnSameStringAsRaw_LongTextTruncatedRaw
// covers the case where Raw itself is truncated (text > 500 chars) — the
// excerpt (<=200 chars, taken from the match position) must still be a
// substring of the ORIGINAL text (the same string ClassifyErrorWithPattern
// saw), and — because the match falls within Raw's first 500 chars in this
// test — also a substring of Raw.
func TestClassifiedTextExcerpt_WindowOnSameStringAsRaw_LongTextTruncatedRaw(t *testing.T) {
	preamble := strings.Repeat("x", 100)
	text := preamble + "rate limit exceeded here" + strings.Repeat("y", 600)

	errType, patternID := ClassifyErrorWithPattern(text, 1)
	if errType != ErrTypeRateLimit {
		t.Fatalf("setup: errType = %q, want %q", errType, ErrTypeRateLimit)
	}

	raw := truncate(text, 500)
	excerpt := ClassifiedTextExcerpt(text, patternID)

	if !strings.Contains(text, excerpt) {
		t.Errorf("excerpt %q is not a substring of the original classified text — window invariant violated", excerpt)
	}
	if !strings.Contains(raw, excerpt) {
		t.Errorf("excerpt %q (match within Raw's truncation window) is not a substring of Raw %q", excerpt, raw)
	}
}
