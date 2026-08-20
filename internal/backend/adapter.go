// internal/backend/adapter.go — backend adapter interface and shared types.
//
// An Adapter is a thin wrapper around one LLM invocation path (subprocess or HTTP).
// Adapters are stateless — all state tracking lives in the router layer.
// Adapters return InvokeError on failure; the router uses the ErrorType to
// update backend state and decide whether to advance the fallback chain.
package backend

import (
	"context"
	"encoding/json"
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

// ToolDef is one tool definition carried outbound to a tool-capable adapter
// (lr-add405). Its three fields — name, description, JSON Schema parameters —
// are genuinely neutral across the Anthropic, OpenAI, and Bedrock Converse
// families; only the wire envelope differs per family (Anthropic
// "input_schema", OpenAI "function.parameters", Bedrock
// "toolSpec.inputSchema.json"). InputSchema is carried as json.RawMessage
// rather than parsed into a Go struct: the router never validates or
// interprets a tool's parameter schema, it only relays it, and RawMessage is
// the only representation that guarantees no information loss (and no
// dialect leakage — a struct would have to pick a shape, and any shape
// picked is one brand's shape) when translating from one family's envelope
// to another's.
//
// Explicitly NOT represented here, because they are not commensurable across
// providers (see CLAUDE.md "Verify per-provider assumptions" and this task's
// lore record, lr-add405): tool-choice forcing, parallel-tool-call controls,
// and server-side/built-in tool types (Anthropic computer/text_editor,
// OpenAI built-ins). A caller sending those gets them ignored by adapters
// that marshal Tools, exactly as any other field this struct does not carry.
type ToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// ToolUse is one tool invocation the model asked the caller to perform,
// carried inbound from a tool-capable adapter's response (lr-add405). ID,
// Name, and a JSON arguments blob are the neutral core present across
// Anthropic tool_use blocks, OpenAI tool_calls entries, and Bedrock
// Converse toolUse content blocks; Input is RawMessage for the same
// no-information-loss/no-dialect-leakage reason as ToolDef.InputSchema.
//
// The router never executes a tool and never accepts a corresponding
// tool_result back in a later turn — see this package's doc and
// CLAUDE.md's "Single-shot tool carriage" scope note. The caller that sent
// Tools on the request owns running the tool and continuing the
// conversation itself, in a separate request.
type ToolUse struct {
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// Request is the input to an adapter invocation.
type Request struct {
	Messages  []Message
	MaxTokens int
	// Tools carries tool definitions outbound to an adapter whose
	// Capabilities().SupportsTools is true (lr-add405). Adapters that do not
	// support tools ignore this field entirely — the router-layer capability
	// filter (see router.FilterChainForTools and its sticky-through-fallback
	// enforcement in Route) is what prevents a tools-bearing request from
	// ever reaching one of those adapters in routed mode; this field itself
	// carries no enforcement. nil/empty means no tools on this request,
	// matching HasTools below.
	Tools []ToolDef
	// WorkingDir is the validated, absolute directory a subprocess adapter
	// sets as its cmd.Dir. Empty means "not specified by the caller" — see
	// DefaultWorkingDir for the fallback every subprocess adapter applies in
	// that case. HTTP adapters (anthropic_api, openai_api, bedrock_api)
	// ignore this field entirely; they have no subprocess and no cwd notion.
	//
	// This value is never inferred from server-side state (daemon cwd, HOME,
	// or any other host-local signal) — the router is a shared daemon and a
	// server-chosen directory would just be a different flavor of the bug
	// this field exists to fix for the next caller. It is set only from an
	// explicit, validated wire request field (see ResolveWorkingDir).
	WorkingDir string
	// HasTools records only whether the wire request carried a non-empty
	// tools field — never the tool definitions themselves. Set by the server
	// layer's hasTools() presence check (handlers.go / messages.go) before
	// backend.Request is constructed. The router forwards this bit into
	// call_log (store.CallLogInput.ToolsPresent) so "who is sending tools at
	// routed chains" is answerable without persisting tool names, schemas,
	// or any other request body content (lr-4aaf2a). Adapters ignore this
	// field entirely today — it exists for observability, not dispatch.
	HasTools bool
}

// DefaultWorkingDir is the cmd.Dir every subprocess adapter uses when the
// caller does not supply an explicit WorkingDir. It is deliberately neutral
// so a subprocess never inherits the daemon's own cwd (which may be a
// project directory carrying its own ./CLAUDE.md or ./.claude/settings.json
// that the daemon's operator never intended a router-spawned session to
// pick up).
const DefaultWorkingDir = "/"

// ResolveWorkingDir validates a caller-supplied working_dir wire value and
// returns the directory a subprocess adapter should use as cmd.Dir.
//
// raw == "" is the explicit-absent case: returns (DefaultWorkingDir, nil)
// with no inference attempted, mirroring the discovery-only-when-empty
// pattern used elsewhere in this codebase (buildAdapter in cmd/clagentic-router
// only invokes discovery when the corresponding config field is empty).
//
// A non-empty raw value is fail-loud: it must be an absolute path that
// exists on disk and is a directory. Any violation returns a descriptive
// error the caller should surface as a 4xx at the wire boundary — silently
// falling back to the default or letting exec fail opaquely later would
// reproduce the exact silent-wrong defect class this field exists to fix.
func ResolveWorkingDir(raw string) (string, error) {
	if raw == "" {
		return DefaultWorkingDir, nil
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("working_dir must be an absolute path, got %q", raw)
	}
	fi, err := os.Stat(raw)
	if err != nil {
		return "", fmt.Errorf("working_dir %q: %w", raw, err)
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("working_dir %q is not a directory", raw)
	}
	return raw, nil
}

// RateLimitInfo holds provider rate-limit header values from one response.
// All fields are best-effort — absent headers leave fields at zero value.
// These reflect per-minute windows, distinct from billing quota fields.
//
// TokensLimit/RequestsLimit (lr-c98c, Slice E) carry the window ceiling
// alongside the pre-existing Remaining fields, harvested from the
// provider's own "-limit" header (openai_api: x-ratelimit-limit-tokens/
// -requests; anthropic_api: anthropic-ratelimit-tokens-limit/
// -requests-limit) using the same best-effort parseIntHeader helper
// already used for Remaining — no new parsing mechanism. Zero means the
// header was absent, identically to every other field here. The router
// uses Limit alongside Remaining to compute a synthetic utilization
// (1.0 - remaining/limit) for quota_snapshots parity with claude_cli's
// rate_limit_event; see router.go's recordSuccess.
type RateLimitInfo struct {
	TokensRemaining   int64
	TokensLimit       int64
	TokensResetAt     time.Time
	RequestsRemaining int64
	RequestsLimit     int64
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
	// ToolUses carries any tool_use-equivalent blocks a tool-capable adapter
	// parsed out of the provider's response (lr-add405). Empty for every
	// adapter whose Capabilities().SupportsTools is false, and empty for a
	// tool-capable adapter's response when the model did not choose to call
	// a tool this turn. The router never executes these and never expects a
	// corresponding tool_result on a later call — see Request.Tools doc.
	ToolUses []ToolUse
	// CacheUsage carries per-model prompt-cache token accounting harvested
	// from the provider's response (lr-718af0). nil means this adapter
	// cannot report cache data for this call — either the adapter family has
	// no cache-accounting concept at all (verified per-family; see each
	// adapter's Capabilities/Invoke doc) or discovering support genuinely
	// requires a live capability check this repo's author could not run (see
	// claude_cli.go/codex_subagent.go/codex_cli.go doc comments for the
	// specific reasoning per adapter). A non-nil *CacheUsage with all-zero
	// fields is a real, reported cache miss — this distinction is load-bearing:
	// collapsing "unsupported" into a zero would make the derived hit-rate
	// metric indistinguishable from a real miss and actively misleading (see
	// CacheUsage's own doc and this task's acceptance criteria).
	CacheUsage *CacheUsage
}

// CacheUsage holds per-model prompt-cache token counts from one adapter
// invocation (lr-718af0). All three fields are real counts reported by the
// provider — never estimates, never derived from PromptTokensEst/
// CompletionTokensEst (those remain char/4-style estimates on CLI adapters
// that lack real usage data; CacheUsage exists precisely so a caller can
// tell the two apart).
//
// CacheWriteTokens is zero-valued (not unsupported) for a provider whose
// caching model has no separate "write" concept — e.g. openai_api's cache is
// automatic and read-only from the caller's perspective; there is nothing to
// write-account. That is a real, documented zero from a family that DOES
// report cache reads, distinct from the whole-struct-nil "cannot report
// anything" case above.
type CacheUsage struct {
	// InputTokens is the total input/prompt tokens billed for this call,
	// exactly as the provider reports it (may double-count cached tokens
	// depending on provider billing semantics — this struct does not attempt
	// to normalize that across providers, only to carry each provider's own
	// number through unmodified).
	InputTokens int64
	// CacheReadTokens is the count of input tokens served from an existing
	// prompt cache (a "hit"). Zero is a real, reported cache miss when
	// CacheUsage itself is non-nil.
	CacheReadTokens int64
	// CacheWriteTokens is the count of input tokens written to create or
	// extend a prompt cache entry this call. Zero either means no cache was
	// written this call, or the provider's caching model has no write-side
	// accounting at all (see doc above) — both are real zeros, not a "cannot
	// report" case, because CacheUsage itself is non-nil.
	CacheWriteTokens int64
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

	// ErrTypeMaxTurns marks a claude CLI exit whose terminal_reason (or a
	// synonymous "errors" entry) is max_turns — the CLI's own --max-turns
	// budget was exhausted before the model produced a final text result.
	// This is distinct from ErrTypeUnknown deliberately (lr-39ed6b): before
	// this type existed, budget exhaustion was logged as error_type=unknown
	// with a truncated raw error, indistinguishable from auth failure,
	// network error, or a crashed subprocess — the exact silent
	// misattribution that cost this repo two full misdiagnosis cycles
	// (lr-4abfe9, retro tome #800). It is treated as a hard failure, not a
	// degraded success: a max_turns exit may carry a successful tool_result
	// in its transcript, but the adapter has no reliable way to extract a
	// final answer from an intermediate tool_result block (that assembly is
	// exactly the step --max-turns cut off), so silently returning
	// "success" here would fabricate a completion the model never actually
	// produced. Giving it its own type — rather than leaving it inside
	// ErrTypeUnknown — lets the router log and alert on it distinctly, and
	// lets an operator raise max_turns for that backend instead of chasing
	// an auth/network red herring.
	ErrTypeMaxTurns ErrorType = "max_turns"
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
