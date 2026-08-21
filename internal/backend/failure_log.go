// internal/backend/failure_log.go — shared CLI-adapter failure-log field
// computation (lr-151fa7).
//
// DRUMMER's decision (lr-151fa7, split from lr-c1d353): every CLI adapter's
// nonzero-exit/in-band-failure log line gets four new fields —
// stderr_len, stdout_len, matched_pattern_id (all Info) and
// classified_text_excerpt (Debug only, bounded). HTTP adapters
// (anthropic_api, openai_api, bedrock_api, ollama_http) get none of this:
// they have no subprocess, no exit code, no stderr/stdout buffers to
// measure, and classify via httpStatusToErrorType on a status code rather
// than ClassifyError on free text, so "matched pattern" and "stdout/stderr
// length" do not translate to that path at all.
//
// This file exists so the excerpt-window logic lives in exactly one place
// rather than being reimplemented per adapter (claude_cli.go, codex_cli.go,
// codex_subagent.go, gemini_cli.go all call ClassifiedTextExcerpt).
package backend

import "strings"

// excerptMaxChars is the hard cap on classifiedTextExcerpt's return value,
// per DRUMMER's decision: "Hard cap 200 chars total."
const excerptMaxChars = 200

// excerptContextChars is the maximum number of characters kept on each side
// of a pattern match, per DRUMMER's decision: "max 30 chars before the
// match + the match + max 30 chars after."
const excerptContextChars = 30

// excerptNoMatchChars is the number of leading characters returned when no
// pattern matched, per DRUMMER's decision: "If no match: first 100 chars."
const excerptNoMatchChars = 100

// ClassifiedTextExcerpt returns a bounded window on text — the SAME string
// ClassifyErrorWithPattern classified and that InvokeError.Raw reports
// (PR #61/lr-c1d353's invariant; see failure_log_test.go's
// TestClassifiedTextExcerpt_WindowOnSameStringAsRaw) — never a
// separately-sourced copy. Debug-level only at the call site: classification
// text can carry model prose, session ids, tool names, tool parameter
// values, and secrets if present in the input, so this excerpt is gated
// behind the same trust boundary handlers.go:362-364 already applies to
// raw backend error text reaching CLIENTS — journal-side logging is a
// different boundary, decided explicitly here rather than by default (see
// this function's callers, all guarded by an `if logger.Enabled(Debug)`-
// equivalent slog.Debug call).
//
// When patternID is non-empty and text contains it, the excerpt is a window
// centered on the FIRST occurrence of patternID in text: up to
// excerptContextChars before the match, the match itself, and up to
// excerptContextChars after — capped in total at excerptMaxChars. A
// patternID that does not literally occur in text (e.g. a synthetic
// exit-code-derived id, or a caller passing a stale/mismatched pair) falls
// back to the no-match case below rather than silently omitting the window.
//
// When patternID is empty (ErrTypeUnknown — no pattern matched), the
// excerpt is the first excerptNoMatchChars characters of text.
func ClassifiedTextExcerpt(text, patternID string) string {
	if patternID != "" {
		if idx := indexFold(text, patternID); idx >= 0 {
			start := idx - excerptContextChars
			if start < 0 {
				start = 0
			}
			end := idx + len(patternID) + excerptContextChars
			if end > len(text) {
				end = len(text)
			}
			return capExcerpt(text[start:end])
		}
	}
	if len(text) <= excerptNoMatchChars {
		return capExcerpt(text)
	}
	return capExcerpt(text[:excerptNoMatchChars])
}

// capExcerpt enforces the excerptMaxChars hard cap regardless of which
// branch above produced s. Operates on bytes, not runes: text may contain
// binary data or null bytes (DRUMMER's own boundary-case list), where rune
// decoding is not guaranteed meaningful — a byte-accurate cap is the only
// bound that holds unconditionally.
func capExcerpt(s string) string {
	if len(s) <= excerptMaxChars {
		return s
	}
	return s[:excerptMaxChars]
}

// indexFold returns the byte index of the first case-insensitive occurrence
// of substr in s, or -1 if absent. ClassifyErrorWithPattern matches against
// strings.ToLower(stderr) for the plain-substring table (patternID is
// already lowercase in that case) but against the ORIGINAL-case text for
// the three word-boundary regex patterns (patternID is a fixed lowercase
// name like "billing" that may not appear verbatim in mixed-case text at
// all) — a case-insensitive search here is correct for both, and a plain
// strings.Index would silently produce idx==-1 (falling through to the
// no-match window) for a mixed-case real match, understating the excerpt's
// usefulness for exactly the regex-pattern case this field exists to help
// debug.
func indexFold(s, substr string) int {
	if substr == "" {
		return -1
	}
	return strings.Index(toLowerASCII(s), toLowerASCII(substr))
}

// toLowerASCII lowercases only ASCII letters, byte-for-byte, so the
// returned string has identical length and identical byte offsets to the
// input — required for indexFold's result to be a valid index into the
// ORIGINAL (non-lowercased) text. strings.ToLower is not used here because
// it can change a string's byte length for non-ASCII input (e.g. Turkish
// dotless I casing, multi-byte case folds), which would silently
// misalign the returned index against the original text on stream content
// that contains such input.
func toLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}
