// internal/backend/adapter.go — backend adapter interface and shared types.
//
// An Adapter is a thin wrapper around one LLM invocation path (subprocess or HTTP).
// Adapters are stateless — all state tracking lives in the router layer.
// Adapters return InvokeError on failure; the router uses the ErrorType to
// update backend state and decide whether to advance the fallback chain.
package backend

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ctxKeyReqID is the context key for the per-request ID.
// Defined here (backend package) so router and server can both use it
// without creating an import cycle.
type ctxKeyReqID struct{}

// WithRequestID returns a copy of ctx with the given request ID attached.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKeyReqID{}, id)
}

// RequestIDFromCtx extracts the request ID from ctx, or returns "" if absent.
func RequestIDFromCtx(ctx context.Context) string {
	if id, ok := ctx.Value(ctxKeyReqID{}).(string); ok {
		return id
	}
	return ""
}

// Message is one turn in a conversation (OpenAI-compatible).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is the input to an adapter invocation.
type Request struct {
	Messages  []Message
	MaxTokens int
}

// RateLimitInfo holds provider rate-limit header values from one response.
// All fields are best-effort — absent headers leave fields at zero value.
// These reflect per-minute windows, distinct from billing quota fields.
type RateLimitInfo struct {
	TokensRemaining   int64
	TokensResetAt     time.Time
	RequestsRemaining int64
	RequestsResetAt   time.Time
}

// Response is the output of a successful adapter invocation.
type Response struct {
	Content             string
	PromptTokensEst     int
	CompletionTokensEst int
	CostUSD             float64
	// RateLimitInfo carries per-minute rate-limit window data harvested from
	// provider response headers. Zero value means no data (headers absent).
	RateLimitInfo RateLimitInfo
	// RateLimitEvent carries quota utilization data parsed from a claude CLI
	// rate_limit_event stream-json line. nil when the CLI did not emit one
	// (below threshold or not a claude_cli adapter).
	RateLimitEvent *RateLimitEvent
}

// ErrorType classifies why an invocation failed.
// The router uses this to decide state machine transitions and chain behavior.
type ErrorType string

const (
	ErrTypeQuota     ErrorType = "quota"      // Hard credit/quota exhaustion
	ErrTypeRateLimit ErrorType = "rate_limit" // Soft rate limit (window-based)
	ErrTypeAuth      ErrorType = "auth"       // Authentication failure (permanent until fixed)
	ErrTypeNetwork   ErrorType = "network"    // Connectivity failure
	ErrTypeTimeout   ErrorType = "timeout"    // Call exceeded timeout
	ErrTypeNotFound  ErrorType = "not_found"  // CLI binary not on PATH
	ErrTypeSchema    ErrorType = "schema"     // Output failed validation
	ErrTypeUnknown   ErrorType = "unknown"    // Unclassified failure
)

// InvokeError is returned by adapters when invocation fails.
// It carries the classified error type so the router can update state correctly.
type InvokeError struct {
	Type ErrorType
	Raw  string // first 500 chars of stderr / error message
}

func (e *InvokeError) Error() string {
	return fmt.Sprintf("%s: %s", e.Type, e.Raw)
}

// Capabilities declares what an adapter's wire protocol can carry, independent
// of any single request. Values are static per adapter — they describe the
// adapter's transport contract, not per-backend config or runtime state.
//
// This is deliberately generic: it describes protocol capability only, never
// a specific consumer's roles, config keys, or naming scheme.
type Capabilities struct {
	// SupportsTools is true when the adapter's Invoke path can carry tool
	// definitions and round-trip tool_use/tool_result content to the
	// provider. False means any tools attached to a request would be
	// silently dropped if sent through this adapter — callers MUST NOT do
	// that; see router chain filtering.
	SupportsTools bool
	// SupportsStreaming is true when the adapter can stream incremental
	// output. All current adapters return complete responses (Invoke is
	// synchronous), so this is false everywhere today; the field exists so
	// a future streaming-capable adapter can declare it without an
	// interface change.
	SupportsStreaming bool
	// SupportsImages is true when the adapter's Invoke path can carry
	// multimodal (image) content blocks through to the provider.
	SupportsImages bool
}

// Adapter is the interface all backend adapters implement.
type Adapter interface {
	// ID returns the backend identifier (matches config key).
	ID() string
	// Invoke sends the request to the LLM and returns the response.
	// Returns *InvokeError on failure.
	Invoke(ctx context.Context, req *Request) (*Response, error)
	// Capabilities reports what this adapter's wire protocol supports.
	// The returned value is static (does not depend on req or runtime
	// state) — see Capabilities doc for field semantics.
	Capabilities() Capabilities
}

// FormatMessages converts a messages slice into a prompt string for CLI-based adapters.
// Returns (prompt, systemPrompt). The prompt is the user-facing input;
// systemPrompt is the optional system instruction.
func FormatMessages(messages []Message) (prompt, system string) {
	var userParts []string
	for _, m := range messages {
		switch m.Role {
		case "system":
			// Last system message wins
			system = m.Content
		case "user":
			userParts = append(userParts, m.Content)
		case "assistant":
			// Include prior turns as context markers
			userParts = append(userParts, fmt.Sprintf("[Previous assistant response: %s]", truncate(m.Content, 500)))
		}
	}
	return strings.Join(userParts, "\n\n"), system
}

// EstimateTokens returns a rough token estimate (chars/4 + overhead).
func EstimateTokens(messages []Message) int {
	total := 50 // base overhead
	for _, m := range messages {
		total += len(m.Content)/4 + 4
	}
	return total
}

// truncate truncates s to at most n characters.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// extraBinDirs is searched in addition to PATH when resolving binaries.
// Covers common install locations for claude and codex CLIs.
var extraBinDirs = []string{
	"/root/.local/bin",
	"/root/.local/share/claude/bin",
	"/usr/local/bin",
	"/usr/bin",
	"/root/.npm-global/bin",
	"/opt/claude/bin",
	"/home/linuxbrew/.linuxbrew/bin",
}

// ResolveBinary resolves a binary to its absolute path.
// Resolution order:
//  1. envOverride (e.g. CLAUDE_BIN env var) — explicit wins.
//  2. which against PATH + extraBinDirs.
//
// Returns "" if not found anywhere.
func ResolveBinary(name, envOverride string) string {
	if envOverride != "" {
		if candidate := os.Getenv(envOverride); candidate != "" {
			if filepath.IsAbs(candidate) {
				if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
					return candidate
				}
			}
			if resolved, err := exec.LookPath(candidate); err == nil {
				return resolved
			}
		}
	}
	// Try standard PATH first
	if resolved, err := exec.LookPath(name); err == nil {
		return resolved
	}
	// Try extra dirs
	for _, dir := range extraBinDirs {
		candidate := filepath.Join(dir, name)
		if fi, err := os.Stat(candidate); err == nil && !fi.IsDir() {
			return candidate
		}
	}
	return ""
}
