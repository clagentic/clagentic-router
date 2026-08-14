// internal/backend/capabilities_test.go — unit tests asserting every
// production Adapter's declared Capabilities() matches this codebase's
// current wire-level reality (lr-be9454).
//
// This is a deliberate guardrail: Capabilities() is a router-wide safety
// signal (a tools-bearing routed request is refused when no backend
// declares SupportsTools), so a mismatch between what an adapter declares
// and what it actually sends/parses on the wire would silently reopen the
// exact drop-without-signal defect this capability model exists to close.
package backend

import "testing"

// As of this writing, every adapter's Invoke sends plain-text messages and
// parses plain-text responses only — none marshal a tools field or a
// non-text content block. If an adapter's Invoke is later extended to
// actually round-trip tools/images, its Capabilities() must be updated in
// the same change (see each adapter's Capabilities doc for its specific
// gap) and this test updated alongside it.
func TestAdapterCapabilities_NoneSupportToolsOrImagesToday(t *testing.T) {
	adapters := map[string]Adapter{
		"claude_cli":     NewClaudeCLIAdapter("claude_cli", "", "/nonexistent/claude", EffortLevel(""), ThinkingOff, nil),
		"codex_cli":      NewCodexCLIAdapter("codex_cli", "", "", "", "", "/nonexistent/codex"),
		"codex_subagent": NewCodexSubagentAdapter("codex_subagent", "flagship", "/nonexistent/claude", nil),
		"gemini_cli":     NewGeminiCLIAdapter("gemini_cli", "", "/nonexistent/gemini"),
		"ollama_http":    NewOllamaHTTPAdapter("ollama_http", "http://localhost:11434", "test-model", 0),
		"anthropic_api":  NewAnthropicAPIAdapter("anthropic_api", "test-model", "test-key", "", 0, EffortLevel(""), ThinkingOff),
		"openai_api":     NewOpenAIAPIAdapter("openai_api", "test-model", "test-key", "", 0),
		"bedrock_api":    newBedrockAPIAdapterWithClient("bedrock_api", "test-model", nil),
	}

	for id, adp := range adapters {
		caps := adp.Capabilities()
		if caps.SupportsTools {
			t.Errorf("%s: SupportsTools=true, but no adapter's Invoke marshals a tools field today", id)
		}
		if caps.SupportsImages {
			t.Errorf("%s: SupportsImages=true, but no adapter's Invoke sends non-text content blocks today", id)
		}
		if caps.SupportsStreaming {
			t.Errorf("%s: SupportsStreaming=true, but every adapter's Invoke is synchronous/complete-response today", id)
		}
	}
}
