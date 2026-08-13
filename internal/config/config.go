// internal/config/config.go — configuration loading and validation.
//
// Config is loaded once at startup from a YAML file. All values can be
// overridden by environment variables using the env: prefix convention.
// Nothing is hardcoded — model names, host addresses, API keys, and paths
// are all config-driven.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// AdapterType identifies the backend adapter to use for a backend.
type AdapterType string

const (
	AdapterClaudeCLI     AdapterType = "claude_cli"
	AdapterCodexCLI      AdapterType = "codex_cli"
	AdapterCodexSubagent AdapterType = "codex_subagent"
	AdapterOllamaHTTP    AdapterType = "ollama_http"
	AdapterAnthropicAPI  AdapterType = "anthropic_api"
	AdapterOpenAIAPI     AdapterType = "openai_api"
	AdapterGeminiCLI     AdapterType = "gemini_cli"
	AdapterBedrockAPI    AdapterType = "bedrock_api"
)

// Duration is a time.Duration that unmarshals from a YAML string (e.g. "30m", "1h").
// When the field is not present or is zero, callers should apply their own defaults.
type Duration time.Duration

// UnmarshalYAML implements yaml.Unmarshaler so Duration fields in config structs
// can be written as human-readable strings ("30m", "2h", "15s") instead of raw
// integer nanoseconds. An empty string unmarshals to 0 (caller applies default).
func (d *Duration) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var s string
	if err := unmarshal(&s); err != nil {
		return err
	}
	if s == "" {
		*d = 0
		return nil
	}
	dur, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(dur)
	return nil
}

// QuotaProbeConfig configures the idle-quota probe loop for a backend.
// Only meaningful for claude_cli backends; ignored on all other adapter types.
type QuotaProbeConfig struct {
	// Enabled activates the probe loop. Default false.
	Enabled bool `yaml:"enabled"`

	// Interval is how long to wait without an organic rate_limit_event before
	// firing a probe. Parsed from a duration string (e.g. "30m", "1h").
	// Default: 30 minutes.
	Interval Duration `yaml:"interval"`

	// Model is the cheapest model to use for the probe ping.
	// Default: "claude-haiku-4-5".
	Model string `yaml:"model"`
}

// BackendConfig is the configuration for one LLM backend.
type BackendConfig struct {
	// Adapter is the adapter type (required).
	Adapter AdapterType `yaml:"adapter"`

	// Model is the provider-specific model string passed directly to the backend
	// adapter without any transformation by the router.
	//
	// Resolution of family aliases (e.g. "claude-sonnet" → current default Sonnet
	// version) happens at the provider layer — the claude CLI, codex CLI, or
	// Anthropic/OpenAI API resolves the alias on every call. The router is not
	// involved in alias expansion and does not cache or refresh model strings.
	//
	// Examples:
	//   Pinned version:          claude-sonnet-4-6   (exact, stable)
	//   Family alias:            claude-sonnet        (provider resolves to current default)
	//   Codex (no alias scheme): o4-mini              (pinned; update explicitly to roll forward)
	Model string `yaml:"model"`

	// ReasoningEffort is the optional reasoning effort hint (low|medium|high).
	// Only used by codex_cli and openai_api adapters. Kept for backward compatibility.
	// For anthropic_api and claude_cli, use Effort instead.
	ReasoningEffort string `yaml:"reasoning_effort"`

	// Effort is the provider-agnostic effort hint: low|medium|high|xhigh|max.
	// Maps to output_config.effort for anthropic_api; to --thinking for claude_cli
	// (when the CLI supports it). Empty string means "use provider default" (field
	// is omitted from the wire request entirely).
	// For codex_cli/openai_api: if Effort is set it takes precedence over ReasoningEffort.
	Effort string `yaml:"effort"`

	// ThinkingMode enables extended thinking. Supported values:
	//   "" (empty) — off, no thinking field sent (default)
	//   "adaptive" — send thinking.type="adaptive" (Anthropic Opus 4.7+/4.8+)
	// Maps to thinking.type for anthropic_api; to --thinking for claude_cli
	// (when the CLI supports it).
	ThinkingMode string `yaml:"thinking_mode"`

	// Tier is the codex subagent tier (flagship|mini|spark).
	// Only used by codex_subagent adapter.
	Tier string `yaml:"tier"`

	// URL is the base URL for HTTP-based adapters (ollama_http, anthropic_api, openai_api).
	URL string `yaml:"url"`

	// APIKey is the API key for API-based adapters. Use "env:VAR_NAME" to read from env.
	APIKey string `yaml:"api_key"`

	// Region is the AWS region for the bedrock_api adapter (required — Bedrock
	// has no SDK default region). Ignored by all other adapter types.
	Region string `yaml:"region"`

	// Profile is an optional named AWS shared-config/credentials profile for
	// the bedrock_api adapter. Empty uses the standard SDK credential chain
	// with no profile override. Ignored by all other adapter types.
	Profile string `yaml:"profile"`

	// OpenAIAPIKey enables Layer 2 (OpenAI usage API polling) for codex backends.
	// Use "env:VAR_NAME" to read from env.
	OpenAIAPIKey string `yaml:"openai_api_key"`

	// CodexProviderID is the model_providers.<id> key in the operator's local
	// codex config.toml whose http_headers should be patched with an
	// OpenAI-Project header at invoke time. Only used by codex_cli, and only
	// when OpenAIProjectID is also set. Empty (default) means no header
	// injection — zero behavior change. The router does not read or validate
	// codex config.toml; this is an opaque operator-supplied provider id
	// passed straight to a -c override flag.
	CodexProviderID string `yaml:"codex_provider_id"`

	// OpenAIProjectID is the value injected as the OpenAI-Project header via
	// CodexProviderID's http_headers override. Only used by codex_cli, and
	// only when CodexProviderID is also set. Empty (default) means no header
	// injection. Opaque operator-supplied string; the router does not
	// interpret or validate it.
	OpenAIProjectID string `yaml:"openai_project_id"`

	// CostWeight is the routing preference multiplier. Higher = preferred.
	// Default 1.0. Free local backends should be 1.5; expensive flagship 0.3–0.4.
	CostWeight float64 `yaml:"cost_weight"`

	// RateWindowSeconds is the length of the rate limit window in seconds.
	// Used for ChatGPT Plus window accounting (typically 10800 = 3hr).
	RateWindowSeconds int `yaml:"rate_window_seconds"`

	// RateWindowMaxMessages is the maximum messages per rate window before
	// soft score penalty kicks in. Set to the known platform limit.
	RateWindowMaxMessages int `yaml:"rate_window_max_messages"`

	// TimeoutSeconds is the per-call timeout. Default 180 (3 min).
	TimeoutSeconds int `yaml:"timeout_seconds"`

	// BinPath is the explicit binary path override (e.g. /usr/local/bin/claude).
	// If empty, the binary is auto-resolved from PATH + known install dirs.
	BinPath string `yaml:"bin_path"`

	// CapacityPolling configures a capacity poller for local backends (llama.cpp, Ollama).
	// Leave empty for cloud API backends.
	CapacityPolling CapacityPollingConfig `yaml:"capacity_polling"`

	// QuotaProbe configures the idle-quota probe loop.
	// Only active for claude_cli backends with Enabled=true.
	QuotaProbe QuotaProbeConfig `yaml:"quota_probe"`
}

// CapacityPollingConfig configures a capacity poller for local backends (llama.cpp, Ollama).
// Leave empty for cloud API backends; the poller is disabled when Type is "".
type CapacityPollingConfig struct {
	// Type is the poller type: "llamacpp" or "ollama". Empty = disabled.
	Type string `yaml:"type"`

	// BaseURL is the local server URL (e.g. "http://localhost:8080").
	// Defaults to the backend's url field when empty.
	BaseURL string `yaml:"base_url"`

	// IntervalSeconds is the poll interval in seconds.
	// Default: 4 for llamacpp, 7 for ollama.
	IntervalSeconds int `yaml:"interval_seconds"`

	// TotalVRAMBytes is the total GPU VRAM in bytes (Ollama only). 0 = unknown.
	// Required to compute VRAMHeadroom; without it only ModelHot is tracked.
	TotalVRAMBytes int64 `yaml:"total_vram_bytes"`
}

// Timeout returns the call timeout, defaulting to 3 minutes.
func (b *BackendConfig) Timeout() time.Duration {
	if b.TimeoutSeconds <= 0 {
		return 3 * time.Minute
	}
	return time.Duration(b.TimeoutSeconds) * time.Second
}

// ResolvedCostWeight returns CostWeight, defaulting to 1.0.
func (b *BackendConfig) ResolvedCostWeight() float64 {
	if b.CostWeight <= 0 {
		return 1.0
	}
	return b.CostWeight
}

// ResolvedAPIKey returns the API key, resolving env: references.
func (b *BackendConfig) ResolvedAPIKey() string {
	return ResolveEnvRef(b.APIKey)
}

// ResolvedOpenAIAPIKey returns the OpenAI API key, resolving env: references.
func (b *BackendConfig) ResolvedOpenAIAPIKey() string {
	return ResolveEnvRef(b.OpenAIAPIKey)
}

// RoutingConfig controls the routing and health-check behavior.
type RoutingConfig struct {
	// Strategy is "scored" (default) or "ordered".
	// Scored: pick the highest-scoring available backend.
	// Ordered: try backends in chain order, no scoring.
	Strategy string `yaml:"strategy"`

	// QuotaWarningThreshold fires quota_low alerts when estimated remaining
	// capacity drops below this fraction (0.0–1.0). Default 0.2.
	QuotaWarningThreshold float64 `yaml:"quota_warning_threshold"`

	// HealthProbeIntervalSeconds is the passive health probe interval.
	// Default 120.
	HealthProbeIntervalSeconds int `yaml:"health_probe_interval_seconds"`

	// QuotaPollIntervalSeconds is the OpenAI usage API poll interval.
	// Default 300. Only active when openai_api_key is configured.
	QuotaPollIntervalSeconds int `yaml:"quota_poll_interval_seconds"`

	// DegradedFailureThreshold is consecutive failures before DEGRADED.
	// Default 3.
	DegradedFailureThreshold int `yaml:"degraded_failure_threshold"`

	// OfflineFailureThreshold is consecutive failures before OFFLINE.
	// Default 6.
	OfflineFailureThreshold int `yaml:"offline_failure_threshold"`

	// LatencyPenaltyThresholdMs is the call latency EMA (ms) above which a soft
	// score penalty is applied. At 2× threshold the score is halved; at 4× it is
	// quartered. Set to 0 to disable. Default 15000 (15s).
	LatencyPenaltyThresholdMs int `yaml:"latency_penalty_threshold_ms"`

	// ActiveProbeEnabled enables live probe calls for DEGRADED and RECOVERING
	// backends in the background health loop. Default false.
	ActiveProbeEnabled bool `yaml:"active_probe_enabled"`

	// ActiveProbeTimeoutSeconds is the per-call timeout for active health probes.
	// Default 30.
	ActiveProbeTimeoutSeconds int `yaml:"active_probe_timeout_seconds"`

	// RateLimitTokensWarningThreshold is the tokens-remaining value below which a
	// soft penalty is applied to backends with live rate-limit header data.
	// 0 (or unset) disables the penalty. Suggested starting value: 1000.
	RateLimitTokensWarningThreshold int64 `yaml:"rate_limit_tokens_warning_threshold"`

	// OfflineRecoveryProbeIntervalSeconds is how long to wait between bounded
	// recovery probes on OFFLINE backends that have no pending quota/rate-limit
	// reset time. Covers all offline causes including auth failures and soft
	// failure cascades — not just quota/rate-limit (which TryRecover already
	// handles via reset times).
	//
	// Default when absent: 300 seconds (5 minutes). Set to 0 to disable
	// (preserves strict manual-intervention semantics for operators who prefer it).
	//
	// Implemented as a pointer so that nil (field absent from YAML) can be
	// distinguished from an explicit 0 (operator opt-out). All other RoutingConfig
	// int fields use the <=0→default sentinel, which conflates "absent" and "0".
	// This field cannot use that pattern because 0 is a meaningful explicit value.
	OfflineRecoveryProbeIntervalSeconds *int `yaml:"offline_recovery_probe_interval_seconds"`
}

// OfflineRecoveryProbeInterval returns the configured offline recovery probe
// interval in seconds. 0 means disabled. The field is a pointer; this method
// returns the dereferenced value (safe after validate() has set the default).
func (r *RoutingConfig) OfflineRecoveryProbeInterval() int {
	if r.OfflineRecoveryProbeIntervalSeconds == nil {
		return 300 // matches validate() default; guards callers before validate runs
	}
	return *r.OfflineRecoveryProbeIntervalSeconds
}

// AlertsConfig configures alerting behavior.
type AlertsConfig struct {
	// Webhooks is the list of static webhook endpoints defined in config.
	// Dynamic endpoints can be registered at runtime via the /webhooks API.
	Webhooks []WebhookConfig `yaml:"webhooks"`

	// QuotaWarningThreshold overrides routing.quota_warning_threshold for alerts.
	QuotaWarningThreshold float64 `yaml:"quota_warning_threshold"`

	// WebhookMaxRetry is the maximum delivery attempts per event (including first).
	// Default 5.
	WebhookMaxRetry int `yaml:"webhook_max_retry"`

	// WebhookInitialBackoffMs is the backoff before the second attempt (ms).
	// Doubles on each subsequent retry. Default 500.
	WebhookInitialBackoffMs int `yaml:"webhook_initial_backoff_ms"`

	// WebhookTimeoutSeconds is the per-attempt HTTP timeout. Default 10.
	WebhookTimeoutSeconds int `yaml:"webhook_timeout_seconds"`
}

// WebhookConfig is a registered webhook endpoint.
type WebhookConfig struct {
	URL    string   `yaml:"url"`
	Events []string `yaml:"events"`
	Secret string   `yaml:"secret"`
}

// ProxyConfig is the HTTP proxy server configuration.
type ProxyConfig struct {
	// Port is the HTTP listen port. Default 8765.
	Port int `yaml:"port"`

	// Host is the HTTP listen address. Default 127.0.0.1.
	Host string `yaml:"host"`

	// Token is the bearer token for authentication. Use "env:VAR_NAME" to read from env.
	// Env override: CLAGENTIC_ROUTER_TOKEN.
	Token string `yaml:"token"`

	// AdminToken is the bearer token for admin-only endpoints (backend control,
	// internal rate-limit, webhook management, logs, stats, quota, doctor).
	// If empty, falls back to Token. Use "env:VAR_NAME" to read from env.
	// Env override: CLAGENTIC_ROUTER_ADMIN_TOKEN.
	AdminToken string `yaml:"admin_token"`
}

// AnthropicConfig controls the inbound POST /v1/messages endpoint — an
// Anthropic Messages API surface offering transparent passthrough for
// normal Claude models and role:*/chain:*/backend:* routing through the
// router's fallback chains (see internal/server/messages.go).
type AnthropicConfig struct {
	// UpstreamURL is the passthrough target for non-role:* model requests.
	// Default: https://api.anthropic.com.
	UpstreamURL string `yaml:"upstream_url"`

	// UpstreamAPIKey optionally substitutes a configured key for the
	// upstream request in passthrough mode, overriding whatever credential
	// the client sent. Use "env:VAR_NAME" to read from env. Empty (default)
	// means the client's own x-api-key/Authorization header is forwarded
	// unchanged — the router does not see or need an Anthropic key.
	UpstreamAPIKey string `yaml:"upstream_api_key"`
}

// ResolvedUpstreamURL returns the passthrough upstream base URL, defaulting
// to https://api.anthropic.com when unset.
func (a *AnthropicConfig) ResolvedUpstreamURL() string {
	if a.UpstreamURL == "" {
		return "https://api.anthropic.com"
	}
	return strings.TrimRight(a.UpstreamURL, "/")
}

// ResolvedUpstreamAPIKey returns the upstream API key override, resolving
// env: references. Empty means "forward the client's own credential".
func (a *AnthropicConfig) ResolvedUpstreamAPIKey() string {
	return ResolveEnvRef(a.UpstreamAPIKey)
}

// BedrockConfig controls the inbound POST /model/{modelId}/invoke[-with-response-stream]
// endpoints — the AWS Bedrock Runtime InvokeModel wire shape (see
// internal/server/bedrock_invoke.go). Like AnthropicConfig, it offers
// transparent SigV4-signed passthrough to real AWS Bedrock for plain model
// IDs and translated routing for role:*/chain:*/backend:* model IDs.
type BedrockConfig struct {
	// Region is the AWS region passthrough requests are signed and sent to
	// (e.g. "us-east-1"). Required only when a passthrough (non-routed)
	// request is actually received; routed-mode-only deployments may leave
	// this empty. Use "env:VAR_NAME" to read from env.
	Region string `yaml:"region"`

	// Profile is an optional named AWS shared-config/credentials profile
	// used to resolve credentials for passthrough signing. Empty uses the
	// standard SDK credential chain with no profile override.
	Profile string `yaml:"profile"`
}

// ResolvedRegion returns the Bedrock passthrough region, resolving env: references.
func (b *BedrockConfig) ResolvedRegion() string {
	return ResolveEnvRef(b.Region)
}

// ResolvedToken returns the bearer token, resolving env: references.
func (p *ProxyConfig) ResolvedToken() string {
	return ResolveEnvRef(p.Token)
}

// ResolvedAdminToken returns the admin token, resolving env: references.
// Falls back to ResolvedToken() when admin_token is not set.
func (p *ProxyConfig) ResolvedAdminToken() string {
	t := ResolveEnvRef(p.AdminToken)
	if t == "" {
		return p.ResolvedToken()
	}
	return t
}

// Address returns "host:port".
func (p *ProxyConfig) Address() string {
	host := p.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := p.Port
	if port == 0 {
		port = 8765
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// StorageConfig controls state persistence.
type StorageConfig struct {
	// DBPath is the SQLite database file path.
	// Default: $XDG_STATE_HOME/clagentic-router/state.db or ~/.local/state/clagentic-router/state.db
	DBPath string `yaml:"db_path"`

	// LogFlushIntervalSeconds is how often state is flushed to SQLite.
	// Default 30.
	LogFlushIntervalSeconds int `yaml:"log_flush_interval_seconds"`

	// RollingWindowRetentionSeconds is how long to keep rolling window rows.
	// Default 86400 (1 day).
	RollingWindowRetentionSeconds int `yaml:"rolling_window_retention_seconds"`
}

// LogConfig controls structured logging behavior.
type LogConfig struct {
	// Level is the minimum log level: debug|info|warn|error. Default info.
	// Override at runtime with CLAGENTIC_ROUTER_LOG_LEVEL env var.
	Level string `yaml:"level"`

	// Format is the log output format: text|json. Default text.
	// Override at runtime with CLAGENTIC_ROUTER_LOG_FORMAT env var.
	// Use json for structured log ingestion (Loki, CloudWatch, etc.).
	Format string `yaml:"format"`
}

// DeployConfig controls the optional "clagentic-router update" self-deploy
// subcommand: rebuild the binary from source and restart the running
// service. All fields are optional with defaults matching a stock
// systemd install; a clean third-party install works unconfigured.
type DeployConfig struct {
	// SourceDir is the module root to build from. Default "." (the current
	// working directory). The update subcommand does not fetch or check out
	// anything itself — it builds whatever source is present at this path,
	// which is expected to already reflect the desired revision (e.g. a
	// post-merge automation step that runs with cwd already synced to the
	// merged commit).
	SourceDir string `yaml:"source_dir"`

	// InstallPath is the absolute path of the installed binary that the
	// running service executes. Default "/usr/local/bin/clagentic-router".
	InstallPath string `yaml:"install_path"`

	// ServiceName is the systemd unit name (without the .service suffix) to
	// restart after a successful install. Default "clagentic-router".
	// Set to "" explicitly only via ServiceManager below if not using systemd.
	ServiceName string `yaml:"service_name"`

	// ServiceManager selects how the running daemon is restarted after
	// install: "systemd" (default) or "none" (install only, no restart —
	// for setups where an external supervisor handles restarts).
	ServiceManager string `yaml:"service_manager"`
}

// ResolvedSourceDir returns SourceDir, defaulting to ".".
func (d *DeployConfig) ResolvedSourceDir() string {
	if d.SourceDir == "" {
		return "."
	}
	return d.SourceDir
}

// ResolvedInstallPath returns InstallPath, defaulting to the standard
// systemd-unit ExecStart location for a stock install.
func (d *DeployConfig) ResolvedInstallPath() string {
	if d.InstallPath == "" {
		return "/usr/local/bin/clagentic-router"
	}
	return d.InstallPath
}

// ResolvedServiceName returns ServiceName, defaulting to "clagentic-router".
func (d *DeployConfig) ResolvedServiceName() string {
	if d.ServiceName == "" {
		return "clagentic-router"
	}
	return d.ServiceName
}

// ResolvedServiceManager returns ServiceManager, defaulting to "systemd".
func (d *DeployConfig) ResolvedServiceManager() string {
	if d.ServiceManager == "" {
		return "systemd"
	}
	return d.ServiceManager
}

// Config is the top-level router configuration.
type Config struct {
	// Backends maps backend ID → configuration.
	Backends map[string]*BackendConfig `yaml:"backends"`

	// Tiers maps tier alias → ordered list of backend IDs.
	// A tier alias can contain multiple backends (e.g. multiple claude-haiku instances).
	// Backends are scored and the best one is selected.
	Tiers map[string][]string `yaml:"tiers"`

	// Chains maps named chain → ordered list of tier aliases or backend IDs.
	// Use "role:chain-name" in the model field to reference these.
	Chains map[string][]string `yaml:"chains"`

	Routing   RoutingConfig   `yaml:"routing"`
	Alerts    AlertsConfig    `yaml:"alerts"`
	Proxy     ProxyConfig     `yaml:"proxy"`
	Storage   StorageConfig   `yaml:"storage"`
	Log       LogConfig       `yaml:"log"`
	Anthropic AnthropicConfig `yaml:"anthropic"`
	Bedrock   BedrockConfig   `yaml:"bedrock"`

	// Deploy configures the optional "update" self-deploy subcommand.
	// Every field defaults to a stock systemd install; omit entirely for
	// that default behavior.
	Deploy DeployConfig `yaml:"deploy"`

	// RegistryPath is the path to the models registry YAML (tier alias definitions).
	// If empty, only the Tiers map is used for resolution.
	RegistryPath string `yaml:"registry_path"`
}

// Load reads a Config from the YAML file at path.
// Environment variable overrides in the form "env:VAR_NAME" are resolved at call time.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &cfg, nil
}

// validate checks required fields and fills defaults.
func (c *Config) validate() error {
	if len(c.Backends) == 0 {
		return fmt.Errorf("no backends configured")
	}
	for id, b := range c.Backends {
		if b.Adapter == "" {
			return fmt.Errorf("backend %q: adapter is required", id)
		}
		switch b.Adapter {
		case AdapterClaudeCLI, AdapterCodexCLI, AdapterCodexSubagent,
			AdapterOllamaHTTP, AdapterAnthropicAPI, AdapterOpenAIAPI,
			AdapterGeminiCLI, AdapterBedrockAPI:
		default:
			return fmt.Errorf("backend %q: unknown adapter %q", id, b.Adapter)
		}
		if b.Adapter == AdapterOllamaHTTP && b.URL == "" {
			return fmt.Errorf("backend %q: ollama_http requires url", id)
		}
		if b.Adapter == AdapterBedrockAPI && b.Region == "" {
			return fmt.Errorf("backend %q: bedrock_api requires region (no SDK default region exists for Bedrock)", id)
		}
	}
	// Fill routing defaults
	if c.Routing.DegradedFailureThreshold <= 0 {
		c.Routing.DegradedFailureThreshold = 3
	}
	if c.Routing.OfflineFailureThreshold <= 0 {
		c.Routing.OfflineFailureThreshold = 6
	}
	if c.Routing.QuotaWarningThreshold <= 0 {
		c.Routing.QuotaWarningThreshold = 0.2
	}
	if c.Routing.HealthProbeIntervalSeconds <= 0 {
		c.Routing.HealthProbeIntervalSeconds = 120
	}
	if c.Routing.QuotaPollIntervalSeconds <= 0 {
		c.Routing.QuotaPollIntervalSeconds = 300
	}
	if c.Routing.Strategy == "" {
		c.Routing.Strategy = "scored"
	}
	if c.Routing.LatencyPenaltyThresholdMs <= 0 {
		c.Routing.LatencyPenaltyThresholdMs = 15000
	}
	if c.Routing.ActiveProbeTimeoutSeconds <= 0 {
		c.Routing.ActiveProbeTimeoutSeconds = 30
	}
	// OfflineRecoveryProbeIntervalSeconds is a pointer field: nil = absent from
	// YAML (apply default 300); non-nil = operator-set (honour as-is, including 0
	// which means disabled). The pointer is required because 0 is a meaningful
	// explicit value (disable), not a "not configured" sentinel.
	if c.Routing.OfflineRecoveryProbeIntervalSeconds == nil {
		v := 300
		c.Routing.OfflineRecoveryProbeIntervalSeconds = &v
	}
	return nil
}

// ResolveEnvRef returns the value of an env: reference, or the literal string.
// "env:FOO_BAR" → os.Getenv("FOO_BAR")
// "literal-value" → "literal-value"
func ResolveEnvRef(s string) string {
	if strings.HasPrefix(s, "env:") {
		return os.Getenv(strings.TrimPrefix(s, "env:"))
	}
	return s
}
