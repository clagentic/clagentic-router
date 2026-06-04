// internal/backend/thinking.go — effort and thinking-mode types shared by all adapters.
//
// These types are defined here (backend package) so any adapter can reference them
// without violating the import graph (backend → config is allowed; config → backend
// is not). Config stores raw strings; adapters cast to these types at construction.
package backend

// EffortLevel is the effort hint for extended thinking models.
// Empty string means "use provider default" (omit the field entirely).
type EffortLevel string

const (
	EffortLow    EffortLevel = "low"
	EffortMedium EffortLevel = "medium"
	EffortHigh   EffortLevel = "high"
	EffortXHigh  EffortLevel = "xhigh"
	EffortMax    EffortLevel = "max"
)

// ThinkingMode controls whether extended thinking is requested.
type ThinkingMode string

const (
	// ThinkingOff is the default — omit the thinking field entirely.
	ThinkingOff ThinkingMode = ""

	// ThinkingAdaptive enables adaptive thinking (Opus 4.7+ / 4.8+).
	// On these models, thinking.type must be "adaptive"; "enabled" + budget_tokens
	// is rejected with HTTP 400.
	ThinkingAdaptive ThinkingMode = "adaptive"
)
