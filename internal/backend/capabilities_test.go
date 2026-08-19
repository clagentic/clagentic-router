// internal/backend/capabilities_test.go — unit tests asserting every
// production Adapter's declared Capabilities() matches this codebase's
// current wire-level reality (lr-be9454, updated lr-add405).
//
// This is a deliberate guardrail: Capabilities() is a router-wide safety
// signal (a tools-bearing routed request is refused when no chain-resolved
// backend declares SupportsTools), so a mismatch between what an adapter
// declares and what it actually sends/parses on the wire would silently
// reopen the exact drop-without-signal defect this capability model exists
// to close.
//
// lr-add405 gave three HTTP adapters (anthropic_api, openai_api,
// bedrock_api) and one local-HTTP adapter (ollama_http) genuine tool
// carriage: each marshals a neutral backend.ToolDef list into its own wire
// envelope and parses tool_use/tool_calls back into backend.ToolUse. The
// four subprocess CLI adapters (claude_cli, codex_cli, codex_subagent,
// gemini_cli) still collapse every request to a flat prompt string via
// FormatMessages — no wire field to attach a tool schema to, no structured
// channel to parse a tool_use back out of — so they stay SupportsTools:
// false. This test asserts each adapter's actual declared value rather than
// a blanket "none support tools" so it fails loudly the moment any
// adapter's Capabilities() and its Invoke implementation drift apart, in
// either direction.
package backend

import "testing"

func TestAdapterCapabilities_MatchWireReality(t *testing.T) {
	adapters := map[string]Adapter{
		"claude_cli":     NewClaudeCLIAdapter("claude_cli", "", "/nonexistent/claude", EffortLevel(""), ThinkingOff, 0),
		"codex_cli":      NewCodexCLIAdapter("codex_cli", "", "", "", "", "/nonexistent/codex"),
		"codex_subagent": NewCodexSubagentAdapter("codex_subagent", "flagship", "/nonexistent/claude", 0),
		"gemini_cli":     NewGeminiCLIAdapter("gemini_cli", "", "/nonexistent/gemini"),
		"ollama_http":    NewOllamaHTTPAdapter("ollama_http", "http://localhost:11434", "test-model", 0),
		"anthropic_api":  NewAnthropicAPIAdapter("anthropic_api", "test-model", "test-key", "", 0, EffortLevel(""), ThinkingOff),
		"openai_api":     NewOpenAIAPIAdapter("openai_api", "test-model", "test-key", "", 0),
		"bedrock_api":    newBedrockAPIAdapterWithClient("bedrock_api", "test-model", nil),
	}

	// wantTools enumerates, per adapter ID, whether its Invoke genuinely
	// marshals a tools field outbound and parses tool_use/tool_calls
	// inbound. Every entry here must be backed by real code in that
	// adapter's Invoke — this is not a config table, it is a mirror of the
	// wire-level truth (see each adapter's Capabilities doc for the
	// specific marshal/parse call sites).
	wantTools := map[string]bool{
		"claude_cli":     false,
		"codex_cli":      false,
		"codex_subagent": false,
		"gemini_cli":     false,
		"ollama_http":    true,
		"anthropic_api":  true,
		"openai_api":     true,
		"bedrock_api":    true,
	}

	for id, adp := range adapters {
		caps := adp.Capabilities()
		want, ok := wantTools[id]
		if !ok {
			t.Fatalf("%s: no expected SupportsTools value declared in this test — add one", id)
		}
		if caps.SupportsTools != want {
			t.Errorf("%s: SupportsTools=%v, want %v (Capabilities() must match what Invoke actually marshals/parses on the wire)", id, caps.SupportsTools, want)
		}
		// No adapter sends non-text content blocks or streams incremental
		// output today — these two remain blanket-false across every
		// adapter, unlike SupportsTools.
		if caps.SupportsImages {
			t.Errorf("%s: SupportsImages=true, but no adapter's Invoke sends non-text content blocks today", id)
		}
		if caps.SupportsStreaming {
			t.Errorf("%s: SupportsStreaming=true, but every adapter's Invoke is synchronous/complete-response today", id)
		}
	}
}
