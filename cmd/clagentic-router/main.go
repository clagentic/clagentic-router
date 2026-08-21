// cmd/clagentic-router/main.go — entry point for clagentic-router daemon and CLI.
//
// Subcommands:
//
//	serve [--config path]              Start the routing daemon
//	update [--config path]             Rebuild from source and restart the running service
//	health [--server url]              GET /health
//	doctor [--server url]              GET /doctor  (live probes)
//	quota  [--server url]              GET /quota
//	logs   [--server url] [--backend id] [--limit N]
//	call   [--server url] --model M --message TEXT
//	backend reset   <id> [--server url]
//	backend disable <id> [--server url]
//	backend enable  <id> [--server url]
//	version                            Print version and exit
//
// All non-serve subcommands act as thin HTTP clients — they call the daemon
// and print the response as pretty-printed JSON.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/config"
	"github.com/clagentic/clagentic-router/internal/router"
	"github.com/clagentic/clagentic-router/internal/server"
	"github.com/clagentic/clagentic-router/internal/store"
	"github.com/clagentic/clagentic-router/internal/webhook"
)

// version is set at build time via -ldflags "-X main.version=<rev>" (see
// Makefile's LDFLAGS). It MUST remain a var, not a const: the linker's -X
// flag can only overwrite a package-level string variable's initial value —
// it silently no-ops against a const, which is exactly what happened before
// this fix (lr-92ee18 B1): the Makefile's -X main.version flag looked
// correct, ldflags-injected the intended revision, and the binary still
// reported "0.1.0" with no main.version symbol at all, making build
// staleness undetectable from the version/health/doctor surfaces that exist
// to catch it. A build invoked without -X (e.g. `go build` with no
// Makefile) still reports this sane default.
var version = "0.1.0"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
	}

	switch args[0] {
	case "serve":
		return cmdServe(args[1:])
	case "update":
		return cmdUpdate(args[1:])
	case "health":
		return cmdGet(args[1:], "/health")
	case "doctor":
		return cmdGet(args[1:], "/doctor")
	case "quota":
		return cmdGet(args[1:], "/quota")
	case "metrics":
		return cmdGetText(args[1:], "/metrics")
	case "logs":
		return cmdLogs(args[1:])
	case "call":
		return cmdCall(args[1:])
	case "backend":
		return cmdBackend(args[1:])
	case "version", "--version", "-v":
		fmt.Println("clagentic-router", version)
		return nil
	case "help", "--help", "-h":
		printUsage()
		return nil
	default:
		return fmt.Errorf("unknown subcommand %q (try: help)", args[0])
	}
}

// --- serve ---

// serveFlags holds parsed flags for the serve subcommand.
type serveFlags struct {
	cfg          *config.Config
	unsafeNoAuth bool
}

func cmdServe(args []string) error {
	sf, err := parseServeFlags(args)
	if err != nil {
		return err
	}
	cfg := sf.cfg

	setupLogging(cfg)

	// Startup gate: refuse to start without authentication unless --unsafe-no-auth
	// is explicitly passed. This prevents accidental open deployments. (lr-c7ac,
	// admin-token gate added lr-7a26e0.)
	token := cfg.Proxy.ResolvedToken()
	adminToken := cfg.Proxy.ResolvedAdminToken()
	if err := checkAuthGates(token, adminToken, sf.unsafeNoAuth); err != nil {
		return err
	}
	if sf.unsafeNoAuth {
		slog.Warn("clagentic-router: running WITHOUT authentication (--unsafe-no-auth)")
	}

	// Open store
	var st *store.Store
	dbPath := resolveDBPath(cfg)
	if dbPath != "" {
		st, err = store.Open(dbPath)
		if err != nil {
			return fmt.Errorf("open store: %w", err)
		}
		defer st.Close()
		slog.Info("store opened", "path", dbPath)
		// Warn if DB file permissions are broader than 0600 (store.Open sets 0600,
		// but the file may have been created by a previous version or another process).
		if info, err := os.Stat(dbPath); err == nil {
			if info.Mode().Perm()&0o077 != 0 {
				slog.Warn("store: DB file permissions are broader than 0600",
					"path", dbPath, "mode", info.Mode().Perm())
			}
		}
	}

	// Build adapters
	adapters, err := buildAdapters(cfg)
	if err != nil {
		return fmt.Errorf("build adapters: %w", err)
	}
	if len(adapters) == 0 {
		return fmt.Errorf("no backends configured (check config file)")
	}

	// Build webhook deliverer and alert hook
	deliverer := buildDeliverer(cfg, st)
	deliverer.Start()
	defer deliverer.Stop()
	alertHook := buildAlertHook(cfg, deliverer)

	// Build router
	r := router.New(cfg, adapters, st, alertHook)

	// Register usage pollers for backends with openai_api_key configured
	usagePollers := buildUsagePollers(cfg)
	if len(usagePollers) > 0 {
		r.RegisterUsagePollers(usagePollers)
		slog.Info("usage polling enabled", "pollers", len(usagePollers))
	}

	// Register capacity pollers for local backends (llama.cpp, Ollama)
	buildAndRegisterCapacityPollers(cfg, r)

	r.Start()
	defer r.Stop()

	// Build and start HTTP server
	addr := cfg.Proxy.Address()
	if bindHost, _, err := net.SplitHostPort(addr); err == nil {
		if bindHost == "0.0.0.0" || bindHost == "::" {
			slog.Warn("clagentic-router: binding on all interfaces — ensure a TLS-terminating reverse proxy is in front")
		}
	}
	srv := server.New(addr, token, adminToken, sf.unsafeNoAuth, r, st,
		cfg.Anthropic.ResolvedUpstreamURL(), cfg.Anthropic.ResolvedUpstreamAPIKey(),
		cfg.Bedrock.ResolvedRegion(), cfg.Bedrock.Profile,
		cfg.CacheMetrics.Enabled, cfg.CacheMetrics.ResolvedPath(), version)

	// Graceful shutdown on SIGINT/SIGTERM
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		slog.Info("shutting down", "signal", sig)
		srv.Close()
	}()

	slog.Info("clagentic-router starting",
		"version", version,
		"addr", addr,
		"backends", len(adapters),
		"auth", token != "",
		"admin_token_separate", adminToken != token,
	)

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("listen: %w", err)
	}
	return nil
}

// checkAuthGates enforces the boot-time refuse-to-start-unauthenticated
// policy for both the inference token and the admin token. Returns a
// descriptive error naming the empty token when unsafeNoAuth is false;
// returns nil (including when unsafeNoAuth is true, which intentionally
// bypasses both checks). Extracted from cmdServe so the gate logic is
// unit-testable without standing up a full server. (lr-7a26e0)
func checkAuthGates(token, adminToken string, unsafeNoAuth bool) error {
	if unsafeNoAuth {
		return nil
	}
	if token == "" {
		return fmt.Errorf("proxy.token is not set — refusing to start without authentication. " +
			"Set proxy.token in config (e.g. \"env:CLAGENTIC_ROUTER_TOKEN\") or pass " +
			"--unsafe-no-auth for development only")
	}
	if adminToken == "" {
		return fmt.Errorf("proxy.admin_token (or its fallback proxy.token) resolved empty — refusing to start " +
			"without authentication on the admin/control-plane routes. Set proxy.admin_token or proxy.token " +
			"in config (e.g. \"env:CLAGENTIC_ROUTER_ADMIN_TOKEN\") or pass --unsafe-no-auth for development only")
	}
	return nil
}

func parseServeFlags(args []string) (serveFlags, error) {
	configPath := defaultConfigPath()
	logLevel := "info"
	unsafeNoAuth := false

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--config", "-c":
			if i+1 >= len(args) {
				return serveFlags{}, fmt.Errorf("--config requires a value")
			}
			i++
			configPath = args[i]
		case "--log-level":
			if i+1 >= len(args) {
				return serveFlags{}, fmt.Errorf("--log-level requires a value")
			}
			i++
			logLevel = args[i]
		case "--unsafe-no-auth":
			unsafeNoAuth = true
		default:
			return serveFlags{}, fmt.Errorf("unknown flag %q", args[i])
		}
	}

	_ = logLevel // used below via setupLogging
	os.Setenv("CLAGENTIC_ROUTER_LOG_LEVEL", logLevel)

	cfg, err := config.Load(configPath)
	if err != nil {
		return serveFlags{}, fmt.Errorf("load config %s: %w", configPath, err)
	}
	return serveFlags{cfg: cfg, unsafeNoAuth: unsafeNoAuth}, nil
}

func setupLogging(cfg *config.Config) {
	// Level: env overrides config, config overrides default (info).
	// Accept deprecated CLAGENTIC_LOG_LEVEL with a one-time warning.
	levelStr := cfg.Log.Level
	if v := os.Getenv("CLAGENTIC_LOG_LEVEL"); v != "" {
		fmt.Fprintln(os.Stderr, "CLAGENTIC_LOG_LEVEL is deprecated, use CLAGENTIC_ROUTER_LOG_LEVEL")
		levelStr = v
	}
	if v := os.Getenv("CLAGENTIC_ROUTER_LOG_LEVEL"); v != "" {
		levelStr = v
	}
	level := slog.LevelInfo
	switch levelStr {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}

	// Format: env overrides config, config overrides default (text).
	// Accept deprecated CLAGENTIC_LOG_FORMAT with a one-time warning.
	formatStr := cfg.Log.Format
	if v := os.Getenv("CLAGENTIC_LOG_FORMAT"); v != "" {
		fmt.Fprintln(os.Stderr, "CLAGENTIC_LOG_FORMAT is deprecated, use CLAGENTIC_ROUTER_LOG_FORMAT")
		formatStr = v
	}
	if v := os.Getenv("CLAGENTIC_ROUTER_LOG_FORMAT"); v != "" {
		formatStr = v
	}

	opts := &slog.HandlerOptions{Level: level}
	var h slog.Handler
	if formatStr == "json" {
		h = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		h = slog.NewTextHandler(os.Stderr, opts)
	}
	slog.SetDefault(slog.New(h))
}

func defaultConfigPath() string {
	// Check common locations in order:
	// 1. CLAGENTIC_ROUTER_CONFIG env var
	// 2. ./router.yaml
	// 3. ~/.config/clagentic/router/router.yaml  (canonical per CLI-NAMING-STANDARD)
	// 4. ~/.config/clagentic-router/router.yaml  (deprecated — accepted for one release cycle)
	// 5. /etc/clagentic/router/router.yaml
	// 6. /etc/clagentic-router/router.yaml        (deprecated)
	if v := os.Getenv("CLAGENTIC_ROUTER_CONFIG"); v != "" {
		return v
	}
	home := os.Getenv("HOME")
	canonicalUser := filepath.Join(home, ".config", "clagentic", "router", "router.yaml")
	legacyUser := filepath.Join(home, ".config", "clagentic-router", "router.yaml")
	candidates := []struct {
		path       string
		deprecated bool
	}{
		{"router.yaml", false},
		{canonicalUser, false},
		{legacyUser, true},
		{"/etc/clagentic/router/router.yaml", false},
		{"/etc/clagentic-router/router.yaml", true},
	}
	for _, c := range candidates {
		if _, err := os.Stat(c.path); err == nil {
			if c.deprecated {
				slog.Warn("config path is deprecated; move to canonical location",
					"current", c.path,
					"canonical", canonicalUser)
			}
			return c.path
		}
	}
	return "router.yaml" // fall through; Load will produce a clear error
}

func resolveDBPath(cfg *config.Config) string {
	if cfg.Storage.DBPath != "" {
		return cfg.Storage.DBPath
	}
	// XDG_STATE_HOME / ~/.local/state / fallback to ~/.clagentic-router
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return ""
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "clagentic-router", "state.db")
}

// buildAdapters constructs an Adapter for each configured backend.
// Unknown or unsupported adapter types are logged and skipped (not fatal) so
// a partial config can still route to the available backends.
func buildAdapters(cfg *config.Config) (map[string]backend.Adapter, error) {
	adapters := make(map[string]backend.Adapter, len(cfg.Backends))
	for id, b := range cfg.Backends {
		a, err := buildAdapter(id, b)
		if err != nil {
			slog.Warn("skipping backend: build failed", "backend", id, "err", err)
			continue
		}
		adapters[id] = a
		slog.Debug("backend registered", "backend", id, "adapter", b.Adapter)
	}
	return adapters, nil
}

func buildAdapter(id string, b *config.BackendConfig) (backend.Adapter, error) {
	timeout := b.Timeout()

	switch b.Adapter {
	case config.AdapterClaudeCLI:
		return backend.NewClaudeCLIAdapter(id, b.Model, b.BinPath,
			backend.EffortLevel(b.Effort), backend.ThinkingMode(b.ThinkingMode), b.MaxTurns), nil

	case config.AdapterCodexCLI:
		// Provider-id discovery is cached at construction time (never
		// per-Invoke): reads the operator's local codex config.toml.
		// codex_provider_id remains honored as an explicit override for the
		// ambiguous multi-provider case; the default path (unset) needs
		// zero operator config. openai_project_id has no discovery path —
		// it is override-only (see codex_discovery.go doc) and is passed
		// through unchanged. Any provider-discovery failure degrades to an
		// empty pair — see codex_discovery.go doc — so this can never block
		// adapter construction.
		providerID, projectID := backend.DiscoverCodexProjectHeader(b.CodexProviderID, b.OpenAIProjectID)

		model := b.Model
		if model == "" {
			// Model discovery (lr-82e68e) is purely additive: it only
			// engages when the operator has not pinned an explicit model
			// string. Resolve the codex binary once here so both discovery
			// and the adapter itself share one resolved path (avoids a
			// duplicate "binary resolved" log line from ResolveBinPath).
			bin := backend.ResolveBinPath("codex", b.BinPath, "CODEX_BIN")
			resolved, err := backend.ResolveCodexModel(context.Background(), bin, b.ResolvedModelRank())
			if err != nil {
				return nil, fmt.Errorf("codex_cli: resolve model (rank %d): %w", b.ResolvedModelRank(), err)
			}
			model = resolved
		}
		return backend.NewCodexCLIAdapter(id, model, b.ReasoningEffort, providerID, projectID, b.BinPath), nil

	case config.AdapterCodexSubagent:
		return backend.NewCodexSubagentAdapter(id, b.Tier, b.BinPath, b.MaxTurns), nil

	case config.AdapterOllamaHTTP:
		if b.URL == "" {
			return nil, fmt.Errorf("ollama_http requires url")
		}
		return backend.NewOllamaHTTPAdapter(id, b.URL, b.Model, timeout), nil

	case config.AdapterAnthropicAPI:
		apiKey := b.ResolvedAPIKey()
		if apiKey == "" {
			return nil, fmt.Errorf("anthropic_api requires api_key (or env:VAR)")
		}
		apiURL := b.URL
		if apiURL == "" {
			apiURL = "https://api.anthropic.com"
		}
		return backend.NewAnthropicAPIAdapter(id, b.Model, apiKey, apiURL, timeout,
			backend.EffortLevel(b.Effort), backend.ThinkingMode(b.ThinkingMode)), nil

	case config.AdapterOpenAIAPI:
		apiKey := b.ResolvedAPIKey()
		if apiKey == "" {
			return nil, fmt.Errorf("openai_api requires api_key (or env:VAR)")
		}
		apiURL := b.URL
		if apiURL == "" {
			apiURL = "https://api.openai.com"
		}
		return backend.NewOpenAIAPIAdapter(id, b.Model, apiKey, apiURL, timeout), nil

	case config.AdapterGeminiCLI:
		return backend.NewGeminiCLIAdapter(id, b.Model, b.BinPath), nil

	case config.AdapterBedrockAPI:
		if b.Region == "" {
			return nil, fmt.Errorf("bedrock_api requires region")
		}
		return backend.NewBedrockAPIAdapter(context.Background(), id, b.Model, b.Region, b.Profile)

	default:
		return nil, fmt.Errorf("unknown adapter type %q", b.Adapter)
	}
}

// buildDeliverer constructs the webhook Deliverer from config and store.
// Static endpoints from cfg.Alerts.Webhooks are pre-loaded; dynamic endpoints
// are read from the store at delivery time.
func buildDeliverer(cfg *config.Config, st *store.Store) *webhook.Deliverer {
	whCfg := webhook.Config{
		MaxRetry:         cfg.Alerts.WebhookMaxRetry,
		InitialBackoffMs: cfg.Alerts.WebhookInitialBackoffMs,
		TimeoutSeconds:   cfg.Alerts.WebhookTimeoutSeconds,
	}
	static := make([]webhook.StaticEndpoint, 0, len(cfg.Alerts.Webhooks))
	for _, wh := range cfg.Alerts.Webhooks {
		static = append(static, webhook.NewStaticEndpoint(
			wh.URL,
			wh.Events,
			config.ResolveEnvRef(wh.Secret),
		))
	}
	return webhook.New(whCfg, st, static)
}

// buildAlertHook returns an AlertHook that logs state changes and enqueues
// HTTP webhook deliveries via the Deliverer.
func buildAlertHook(cfg *config.Config, d *webhook.Deliverer) router.AlertHook {
	return func(n router.Notification) {
		slog.Warn("backend state change",
			"event", n.Event,
			"backend", n.BackendID,
			"status", string(n.Snapshot.Status),
			"consecutive_failures", n.Snapshot.ConsecutiveFailures,
		)
		d.Enqueue(webhook.DeliveryEvent{
			Event:     n.Event,
			BackendID: n.BackendID,
			Snapshot:  n.Snapshot,
		})
	}
}

// buildUsagePollers constructs UsagePollers for backends that have openai_api_key set.
// Only backends with an OpenAI-compatible billing URL (or no URL, which defaults to
// api.openai.com) are polled. Non-OpenAI backends (ollama, Anthropic, etc.) are
// skipped even if they happen to have an openai_api_key configured.
// Pollers are registered with the router via RegisterUsagePollers before Start.
func buildUsagePollers(cfg *config.Config) []*backend.UsagePoller {
	interval := time.Duration(cfg.Routing.QuotaPollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	var pollers []*backend.UsagePoller
	for id, b := range cfg.Backends {
		key := b.ResolvedOpenAIAPIKey()
		if key == "" {
			continue
		}
		// Only poll OpenAI-family backends. Skip adapters whose URL clearly
		// points at a non-OpenAI host, which would otherwise receive the
		// admin key in the Authorization header by mistake.
		apiURL := b.URL
		if apiURL != "" &&
			!strings.Contains(apiURL, "openai.com") &&
			!strings.Contains(apiURL, "api.openai") {
			slog.Debug("usage poller skipped: URL is not openai.com", "backend", id, "url", apiURL)
			continue
		}
		poller := backend.NewUsagePoller(id, key, apiURL, interval, nil)
		pollers = append(pollers, poller)
		slog.Debug("usage poller registered", "backend", id, "interval", interval)
	}
	return pollers
}

// buildAndRegisterCapacityPollers constructs and registers local backend capacity
// pollers for any backend that has capacity_polling.type configured.
// Pollers are registered with the router; they are started when r.Start() is called.
func buildAndRegisterCapacityPollers(cfg *config.Config, r *router.Router) {
	for id, b := range cfg.Backends {
		cp := b.CapacityPolling
		if cp.Type == "" {
			continue
		}
		baseURL := cp.BaseURL
		if baseURL == "" {
			baseURL = b.URL
		}
		if baseURL == "" {
			slog.Warn("capacity_polling: no base_url and no url configured, skipping", "backend", id)
			continue
		}
		interval := time.Duration(cp.IntervalSeconds) * time.Second

		switch cp.Type {
		case "llamacpp":
			p := backend.NewLlamaCppPoller(id, baseURL, interval, nil)
			r.RegisterLlamaCppPoller(p)
			slog.Info("llamacpp capacity poller registered", "backend", id, "base_url", baseURL)
		case "ollama":
			p := backend.NewOllamaPoller(id, baseURL, b.Model, cp.TotalVRAMBytes, interval, nil)
			r.RegisterOllamaPoller(p)
			slog.Info("ollama capacity poller registered", "backend", id, "base_url", baseURL, "model", b.Model)
		default:
			slog.Warn("capacity_polling: unknown type, skipping", "backend", id, "type", cp.Type)
		}
	}
}

// --- client subcommands ---

type clientFlags struct {
	server string
	token  string
	// tokenSource names where token came from, for diagnostic use only
	// (never logged/printed on success — only surfaced by enrichAuthError
	// on an actual 401, and even then it names the SOURCE, never the token
	// value). One of: "--token", "--token-file", "env:CLAGENTIC_ROUTER_TOKEN",
	// "env:CLAGENTIC_ROUTER_ADMIN_TOKEN", "envfile:<path>", or "" (unresolved).
	tokenSource string
	// envFileTried is the deployment env file path resolution attempted
	// (whether or not it yielded a token), so enrichAuthError can name the
	// exact file an operator should check even when resolution failed
	// entirely (lr-92ee18 B3).
	envFileTried string
}

// parseClientFlags parses the shared --server/--token/--token-file flags
// used by every client subcommand (health, doctor, quota, logs, call,
// backend ...) and resolves the bearer token.
//
// Resolution order (lr-92ee18 B3):
//  1. --token / -t flag (explicit, wins outright)
//  2. --token-file flag (reads and trims the file's contents)
//  3. CLAGENTIC_ROUTER_TOKEN / CLAGENTIC_ROUTER_ADMIN_TOKEN in the CALLER's
//     own environment (pre-existing behavior)
//  4. The deployment's EnvironmentFile (see resolveTokenFromEnvFile) —
//     the daemon's systemd unit loads CLAGENTIC_ROUTER_TOKEN from
//     EnvironmentFile=/etc/clagentic/router/env (see
//     deploy/clagentic-router.service), which is NOT sourced into an
//     operator's interactive shell. Before this fix, an operator running
//     "clagentic-router doctor" from their own shell always fell through
//     step 3 with the env var unset and got a bare 401 — the single best
//     diagnostic surface for a broken deployment effectively did not exist
//     for the default (systemd EnvironmentFile) deployment shape. This step
//     reads the same file the daemon itself was configured to load from,
//     so the common case resolves with zero extra operator action.
//
// Token material itself is never logged; only the SOURCE it resolved from
// is recorded in tokenSource, for error diagnostics.
func parseClientFlags(args []string) (clientFlags, []string, error) {
	f := clientFlags{
		server: defaultServerURL(),
	}
	var tokenFilePath string
	var remaining []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--server", "-s":
			if i+1 >= len(args) {
				return f, nil, fmt.Errorf("--server requires a value")
			}
			i++
			f.server = args[i]
		case "--token", "-t":
			if i+1 >= len(args) {
				return f, nil, fmt.Errorf("--token requires a value")
			}
			i++
			f.token = args[i]
			f.tokenSource = "--token"
		case "--token-file":
			if i+1 >= len(args) {
				return f, nil, fmt.Errorf("--token-file requires a value")
			}
			i++
			tokenFilePath = args[i]
		default:
			remaining = append(remaining, args[i])
		}
	}

	if f.token == "" && tokenFilePath != "" {
		data, err := os.ReadFile(tokenFilePath)
		if err != nil {
			return f, nil, fmt.Errorf("--token-file %s: %w", tokenFilePath, err)
		}
		f.token = strings.TrimSpace(string(data))
		f.tokenSource = "--token-file " + tokenFilePath
	}

	if f.token == "" {
		if v := os.Getenv("CLAGENTIC_ROUTER_TOKEN"); v != "" {
			f.token = v
			f.tokenSource = "env:CLAGENTIC_ROUTER_TOKEN"
		} else if v := os.Getenv("CLAGENTIC_ROUTER_ADMIN_TOKEN"); v != "" {
			f.token = v
			f.tokenSource = "env:CLAGENTIC_ROUTER_ADMIN_TOKEN"
		}
	}

	if f.token == "" {
		envFile := resolveDeploymentEnvFilePath()
		f.envFileTried = envFile
		if v, ok := resolveTokenFromEnvFile(envFile); ok {
			f.token = v
			f.tokenSource = "envfile:" + envFile
		}
	}

	return f, remaining, nil
}

// resolveDeploymentEnvFilePath returns the path client subcommands check for
// a daemon-style token when no token was resolved from a flag or the
// caller's own shell environment (lr-92ee18 B3 step 4).
//
// CLAGENTIC_ROUTER_ENV_FILE lets an operator point at a non-default
// EnvironmentFile path (matching a customized systemd unit); the default
// mirrors deploy/clagentic-router.service's EnvironmentFile= value exactly,
// so a stock install needs no configuration for this to work.
func resolveDeploymentEnvFilePath() string {
	if v := os.Getenv("CLAGENTIC_ROUTER_ENV_FILE"); v != "" {
		return v
	}
	return "/etc/clagentic/router/env"
}

// resolveTokenFromEnvFile reads a systemd EnvironmentFile-style file
// (KEY=VALUE per line, '#' comments, blank lines ignored) and returns the
// first non-empty value of CLAGENTIC_ROUTER_TOKEN or
// CLAGENTIC_ROUTER_ADMIN_TOKEN found (TOKEN checked first, matching
// ProxyConfig.ResolvedAdminToken's "admin_token falls back to token" — the
// same bearer value authenticates both inference and admin routes unless
// admin_token is separately set, and this file cannot distinguish which the
// daemon actually resolved, so the more commonly-set var is tried first).
// ok is false when the file does not exist, cannot be read, or contains
// neither variable — never an error: this is a best-effort fallback, not a
// required config source, and a missing/unreadable file must never abort a
// client subcommand that could otherwise proceed via --token/--token-file/
// the caller's own env.
func resolveTokenFromEnvFile(path string) (value string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	values := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)
		// EnvironmentFile values may be quoted; strip matching quotes (systemd's
		// own parser accepts both, and an operator hand-editing this file is
		// likely to quote a token containing special characters).
		val = strings.Trim(val, `"'`)
		values[key] = val
	}
	for _, key := range []string{"CLAGENTIC_ROUTER_TOKEN", "CLAGENTIC_ROUTER_ADMIN_TOKEN"} {
		if v := values[key]; v != "" {
			return v, true
		}
	}
	return "", false
}

// enrichAuthError turns a bare 401 from apiGet/apiPost into a diagnostic
// that names the variable and file an operator should check, never a bare
// "HTTP 401" (lr-92ee18 B3's acceptance criterion). No-op (returns err
// unchanged) for any error that is not itself a 401 — this must never mask
// a different failure as an auth problem. Never includes token material:
// only variable names and file paths, matching the task's "never print
// token material — name the variable, not the value" constraint.
func enrichAuthError(err error, f clientFlags) error {
	if err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		return err
	}
	if f.tokenSource != "" {
		// A token WAS resolved (from f.tokenSource) and the server still
		// rejected it — likely stale/wrong, not "no token found." Name the
		// source so the operator knows which credential to check, not just
		// that auth failed.
		return fmt.Errorf("%w (a token was resolved from %s but the server rejected it — "+
			"verify it matches the daemon's proxy.token/proxy.admin_token)", err, f.tokenSource)
	}
	hint := fmt.Sprintf("no token found: checked --token, --token-file, "+
		"CLAGENTIC_ROUTER_TOKEN / CLAGENTIC_ROUTER_ADMIN_TOKEN in this shell's environment, "+
		"and %s (set CLAGENTIC_ROUTER_ENV_FILE to point elsewhere). "+
		"Set one of these to the same value as proxy.token in the daemon's router.yaml.",
		f.envFileTried)
	return fmt.Errorf("%w (%s)", err, hint)
}

func defaultServerURL() string {
	if v := os.Getenv("CLAGENTIC_ROUTER_URL"); v != "" {
		return v
	}
	return "http://localhost:8765"
}

func cmdGet(args []string, path string) error {
	f, _, err := parseClientFlags(args)
	if err != nil {
		return err
	}
	body, err := apiGet(f, path)
	if err != nil {
		return err
	}
	return prettyPrint(body)
}

func cmdGetText(args []string, path string) error {
	f, _, err := parseClientFlags(args)
	if err != nil {
		return err
	}
	url := f.server + path
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	_, err = io.Copy(os.Stdout, resp.Body)
	return err
}

func cmdLogs(args []string) error {
	f, remaining, err := parseClientFlags(args)
	if err != nil {
		return err
	}
	backendID := ""
	limit := "50"
	for i := 0; i < len(remaining); i++ {
		switch remaining[i] {
		case "--backend":
			if i+1 >= len(remaining) {
				return fmt.Errorf("--backend requires a value")
			}
			i++
			backendID = remaining[i]
		case "--limit":
			if i+1 >= len(remaining) {
				return fmt.Errorf("--limit requires a value")
			}
			i++
			limit = remaining[i]
		default:
			return fmt.Errorf("unknown flag %q", remaining[i])
		}
	}
	path := fmt.Sprintf("/logs?limit=%s", limit)
	if backendID != "" {
		path += "&backend=" + backendID
	}
	body, err := apiGet(f, path)
	if err != nil {
		return err
	}
	return prettyPrint(body)
}

func cmdCall(args []string) error {
	f, remaining, err := parseClientFlags(args)
	if err != nil {
		return err
	}
	model := ""
	message := ""
	maxTokens := 0
	for i := 0; i < len(remaining); i++ {
		switch remaining[i] {
		case "--model", "-m":
			if i+1 >= len(remaining) {
				return fmt.Errorf("--model requires a value")
			}
			i++
			model = remaining[i]
		case "--message":
			if i+1 >= len(remaining) {
				return fmt.Errorf("--message requires a value")
			}
			i++
			message = remaining[i]
		case "--max-tokens":
			if i+1 >= len(remaining) {
				return fmt.Errorf("--max-tokens requires a value")
			}
			i++
			fmt.Sscan(remaining[i], &maxTokens)
		default:
			// Treat bare argument as message if model is set
			if model != "" && message == "" {
				message = remaining[i]
			} else {
				return fmt.Errorf("unknown argument %q", remaining[i])
			}
		}
	}
	if model == "" {
		return fmt.Errorf("--model is required")
	}
	if message == "" {
		// Read from stdin
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		message = string(data)
	}
	if message == "" {
		return fmt.Errorf("--message is required (or pipe via stdin)")
	}

	payload := map[string]interface{}{
		"model": model,
		"messages": []map[string]string{
			{"role": "user", "content": message},
		},
	}
	if maxTokens > 0 {
		payload["max_tokens"] = maxTokens
	}

	body, err := apiPost(f, "/v1/chat/completions", payload)
	if err != nil {
		return err
	}

	// Extract and print just the assistant content by default
	var resp map[string]interface{}
	if err := json.Unmarshal(body, &resp); err != nil {
		fmt.Println(string(body))
		return nil
	}
	if choices, ok := resp["choices"].([]interface{}); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]interface{}); ok {
			if msg, ok := choice["message"].(map[string]interface{}); ok {
				if content, ok := msg["content"].(string); ok {
					fmt.Println(content)
					return nil
				}
			}
		}
	}
	return prettyPrint(body)
}

func cmdBackend(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: backend <reset|disable|enable> <id> [--server url]")
	}
	action := args[0]
	switch action {
	case "reset", "disable", "enable":
	default:
		return fmt.Errorf("unknown backend action %q (reset|disable|enable)", action)
	}

	f, remaining, err := parseClientFlags(args[1:])
	if err != nil {
		return err
	}
	if len(remaining) == 0 {
		return fmt.Errorf("backend %s requires a backend ID", action)
	}
	id := remaining[0]

	body, err := apiPost(f, fmt.Sprintf("/backends/%s/%s", id, action), nil)
	if err != nil {
		return err
	}
	return prettyPrint(body)
}

// --- HTTP helpers ---

func apiGet(f clientFlags, path string) ([]byte, error) {
	url := f.server + path
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, enrichAuthError(fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body)), f)
	}
	return body, nil
}

func apiPost(f clientFlags, path string, payload interface{}) ([]byte, error) {
	url := f.server + path
	var reqBody io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		reqBody = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, reqBody)
	if err != nil {
		return nil, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if f.token != "" {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, enrichAuthError(fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body)), f)
	}
	return body, nil
}

func prettyPrint(data []byte) error {
	var v interface{}
	if err := json.Unmarshal(data, &v); err != nil {
		fmt.Println(string(data))
		return nil
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printUsage() {
	fmt.Print(`clagentic-router ` + version + ` — LLM routing daemon

Usage:
  clagentic-router serve [--config PATH] [--log-level debug|info|warn|error] [--unsafe-no-auth]
  clagentic-router update [--config PATH] [--source-dir PATH]
  clagentic-router health  [--server URL] [--token TOKEN | --token-file PATH]
  clagentic-router doctor  [--server URL] [--token TOKEN | --token-file PATH]
  clagentic-router quota   [--server URL] [--token TOKEN | --token-file PATH]
  clagentic-router metrics [--server URL] [--token TOKEN | --token-file PATH]
  clagentic-router logs    [--server URL] [--token TOKEN | --token-file PATH] [--backend ID] [--limit N]
  clagentic-router call    [--server URL] [--token TOKEN | --token-file PATH] --model MODEL [--message TEXT]
  clagentic-router backend reset|disable|enable ID [--server URL] [--token TOKEN | --token-file PATH]
  clagentic-router version

Serve flags:
  --unsafe-no-auth  Start without authentication (development only — never use in production)

Client token resolution (health/doctor/quota/metrics/logs/call/backend):
  Tried in order until one yields a non-empty value: --token, --token-file
  PATH, CLAGENTIC_ROUTER_TOKEN or CLAGENTIC_ROUTER_ADMIN_TOKEN in this
  shell's environment, then the deployment's EnvironmentFile (default
  /etc/clagentic/router/env, matching deploy/clagentic-router.service;
  override with CLAGENTIC_ROUTER_ENV_FILE). The last step exists because the
  daemon's own token normally lives ONLY in that file (loaded via systemd's
  EnvironmentFile=, never sourced into an operator's interactive shell) — so
  a plain "clagentic-router doctor" run from an operator's own terminal
  resolves the running daemon's token with no extra configuration, instead
  of always 401ing. A 401 response names which sources were checked (never
  the token value itself).

Update:
  Maintains a git checkout at deploy.source_dir (default: a managed
  checkout under $XDG_DATA_HOME/clagentic-router/src that update clones —
  deploy.repo_url required — or pulls, ff-only, itself), rebuilds the
  binary from it, atomically installs it at deploy.install_path (default
  /usr/local/bin/clagentic-router), then restarts deploy.service_name via
  systemd (default "clagentic-router"; set deploy.service_manager: "none"
  to install without restarting). Uses the same config resolved by "serve" —
  add an optional [deploy] block to router.yaml only if these defaults do
  not match your install. --source-dir PATH overrides deploy.source_dir for
  this invocation only (wins over config, like --config itself) — set it to
  an existing checkout (e.g. "." to build from update's own working
  directory) to opt out of the managed-checkout clone/pull behavior; update
  never touches the git state of an explicitly-set source_dir.

Environment variables:
  CLAGENTIC_ROUTER_CONFIG       Config file path (default: ./router.yaml)
  CLAGENTIC_ROUTER_URL          Daemon URL for client commands (default: http://localhost:8765)
  CLAGENTIC_ROUTER_TOKEN        Bearer token for inference endpoints and client commands
  CLAGENTIC_ROUTER_ADMIN_TOKEN  Admin bearer token (defaults to CLAGENTIC_ROUTER_TOKEN if unset)
  CLAGENTIC_ROUTER_ENV_FILE     Deployment EnvironmentFile path client commands fall back to
                                reading a token from (default: /etc/clagentic/router/env)
  CLAGENTIC_ROUTER_LOG_LEVEL    Log level for serve (debug|info|warn|error)
  CLAGENTIC_ROUTER_LOG_FORMAT   Log format for serve (text|json)

Config file search order:
  $CLAGENTIC_ROUTER_CONFIG
  ./router.yaml
  ~/.config/clagentic/router/router.yaml
  ~/.config/clagentic-router/router.yaml  (deprecated)
  /etc/clagentic/router/router.yaml
  /etc/clagentic-router/router.yaml       (deprecated)
`)
}
