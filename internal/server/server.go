// internal/server/server.go — HTTP server setup and bearer auth middleware.
//
// Inference routes (/v1/chat/completions, /v1/models) use the inference token.
// Admin routes (control plane, observability, internal events) use the admin token.
// When admin_token is not separately configured, it falls back to the inference token,
// preserving exact backwards compatibility for existing deployments. (lr-c7ac)
package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/clagentic/clagentic-router/internal/backend"
	"github.com/clagentic/clagentic-router/internal/router"
	"github.com/clagentic/clagentic-router/internal/store"
)

// Server is the HTTP server for clagentic-router.
type Server struct {
	httpServer *http.Server
	handler    *Handler
}

// New creates a new Server.
// token is the inference bearer token; adminToken is the admin bearer token.
// When adminToken equals token (the default when admin_token is not configured),
// all routes accept the same credential — identical to previous behaviour.
// allowNoAuth must be true ONLY when the operator explicitly asked to run
// without authentication (--unsafe-no-auth); it is what makes an empty
// token/adminToken pass through instead of rejecting with 401 (lr-7a26e0).
// anthropicUpstreamURL/anthropicUpstreamAPIKey configure the POST /v1/messages
// passthrough target; anthropicUpstreamURL is required non-empty (callers pass
// config.AnthropicConfig.ResolvedUpstreamURL(), which defaults it).
// bedrockRegion/bedrockProfile configure the POST /model/{modelId}/invoke[-with-response-stream]
// passthrough target; bedrockRegion empty disables Bedrock passthrough (routed
// role:/chain:/backend: model IDs still work) — see bedrock_invoke.go.
// cacheMetricsEnabled/cacheMetricsPath gate and place the optional GET
// per-model cache-token exposition endpoint (lr-718af0, see
// cache_metrics.go); the route is registered only when cacheMetricsEnabled
// is true — an unconfigured install (cacheMetricsEnabled == false, the
// config default) never registers the path at all, not merely returns an
// empty body from it.
// version is the running binary's revision string (main.version, see
// cmd/clagentic-router/main.go), surfaced on /version, /health, and /doctor
// (lr-92ee18 B1). Passed through rather than read from a package-level var
// here so this package has no dependency on cmd/clagentic-router.
func New(addr, token, adminToken string, allowNoAuth bool, r *router.Router, st *store.Store, anthropicUpstreamURL, anthropicUpstreamAPIKey, bedrockRegion, bedrockProfile string, cacheMetricsEnabled bool, cacheMetricsPath string, version string) *Server {
	h := &Handler{
		router:                  r,
		store:                   st,
		token:                   token,
		adminToken:              adminToken,
		allowNoAuth:             allowNoAuth,
		anthropicUpstreamURL:    anthropicUpstreamURL,
		anthropicUpstreamAPIKey: anthropicUpstreamAPIKey,
		bedrockRegion:           bedrockRegion,
		bedrockProfile:          bedrockProfile,
		buildVersion:            version,
	}
	mux := http.NewServeMux()

	// OpenAI-compatible inference — inference token
	mux.HandleFunc("POST /v1/chat/completions", h.auth(h.chatCompletions))
	mux.HandleFunc("GET /v1/models", h.auth(h.models))

	// Anthropic Messages API — auth is mode-dependent, checked inside the
	// handler itself (see messages.go): routed mode requires the router's
	// own inference token; passthrough mode forwards whatever credential
	// the client presented and does not gate on the router token.
	mux.HandleFunc("POST /v1/messages", h.messages)

	// AWS Bedrock Runtime InvokeModel wire shape — what Claude Code speaks
	// when CLAUDE_CODE_USE_BEDROCK=1 redirects it here via
	// ANTHROPIC_BEDROCK_BASE_URL. Auth is mode-dependent exactly like
	// /v1/messages (see bedrock_invoke.go): routed mode requires the
	// router's own inference token; passthrough mode is SigV4-signed to the
	// real AWS Bedrock endpoint and does not gate on the router token.
	mux.HandleFunc("POST /model/{modelId}/invoke", h.bedrockInvoke)
	mux.HandleFunc("POST /model/{modelId}/invoke-with-response-stream", h.bedrockInvokeStream)

	// Health/observability — admin token
	mux.HandleFunc("GET /health", h.adminAuth(h.health))
	mux.HandleFunc("GET /doctor", h.adminAuth(h.doctor))
	mux.HandleFunc("GET /quota", h.adminAuth(h.quota))
	mux.HandleFunc("GET /v1/capacity", h.adminAuth(h.capacity))
	mux.HandleFunc("GET /metrics", h.adminAuth(h.metrics))
	if cacheMetricsEnabled {
		// Opt-in per-model cache-token exposition (lr-718af0) — path is
		// config-driven (cache_metrics.path, default /metrics/cache) and the
		// route is not registered at all when the feature is disabled, so an
		// unconfigured install has no new attack surface here. Defaulted here
		// (not only in config.CacheMetricsConfig.ResolvedPath) so this
		// constructor is correct for any caller, not only one that remembered
		// to call ResolvedPath() first.
		path := cacheMetricsPath
		if path == "" {
			path = "/metrics/cache"
		}
		mux.HandleFunc("GET "+path, h.adminAuth(h.cacheMetrics))
	}
	mux.HandleFunc("GET /logs", h.adminAuth(h.logs))
	mux.HandleFunc("GET /stats", h.adminAuth(h.stats))

	// Internal event ingestion — admin token
	mux.HandleFunc("POST /v1/internal/rate-limit", h.adminAuth(h.rateLimitEvent))

	// Backend control — admin token
	mux.HandleFunc("POST /backends/{id}/reset", h.adminAuth(h.backendReset))
	mux.HandleFunc("POST /backends/{id}/disable", h.adminAuth(h.backendDisable))
	mux.HandleFunc("POST /backends/{id}/enable", h.adminAuth(h.backendEnable))

	// Webhook management — admin token
	mux.HandleFunc("POST /webhooks", h.adminAuth(h.webhookCreate))
	mux.HandleFunc("DELETE /webhooks/{id}", h.adminAuth(h.webhookDelete))
	mux.HandleFunc("GET /webhooks", h.adminAuth(h.webhookList))

	// Version — no auth (useful for healthcheck scripts)
	mux.HandleFunc("GET /version", h.version)

	srv := &Server{
		handler: h,
		httpServer: &http.Server{
			Addr:         addr,
			Handler:      logging(mux),
			ReadTimeout:  10 * time.Second,
			WriteTimeout: 300 * time.Second, // long — LLM calls can take minutes
			IdleTimeout:  120 * time.Second,
		},
	}
	return srv
}

// ListenAndServe starts the HTTP server. Blocks until the server stops.
func (s *Server) ListenAndServe() error {
	slog.Info("clagentic-router: listening", "addr", s.httpServer.Addr)
	ln, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return err
	}
	return s.httpServer.Serve(ln)
}

// Close shuts down the server.
func (s *Server) Close() error {
	return s.httpServer.Close()
}

// auth wraps a handler requiring the inference bearer token.
// An empty token rejects with 401 unless h.allowNoAuth is true (the operator
// explicitly passed --unsafe-no-auth) — empty-string-means-open is not a
// valid default; it silently opens the server to anyone who constructs a
// Handler without threading the startup gate. (lr-7a26e0)
func (h *Handler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.token == "" {
			if !h.allowNoAuth {
				writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing bearer token")
				return
			}
			next(w, r)
			return
		}
		hdr := r.Header.Get("Authorization")
		if !strings.HasPrefix(hdr, "Bearer ") || strings.TrimPrefix(hdr, "Bearer ") != h.token {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing bearer token")
			return
		}
		next(w, r)
	}
}

// anthropicTokenPresented reports whether r carries the router's inference
// token via either Anthropic-style (x-api-key) or OpenAI-style
// (Authorization: Bearer) header. Used by messagesRouted, which — unlike
// passthrough — always requires the router's own token (see messages.go
// for the full auth-matrix rationale). An empty token is honored only when
// h.allowNoAuth is true (lr-7a26e0).
func (h *Handler) anthropicTokenPresented(r *http.Request) bool {
	if h.token == "" {
		return h.allowNoAuth
	}
	if r.Header.Get("x-api-key") == h.token {
		return true
	}
	if hdr := r.Header.Get("Authorization"); strings.HasPrefix(hdr, "Bearer ") {
		return strings.TrimPrefix(hdr, "Bearer ") == h.token
	}
	return false
}

// adminAuth wraps a handler requiring the admin bearer token.
// An empty adminToken rejects with 401 unless h.allowNoAuth is true (the
// operator explicitly passed --unsafe-no-auth). (lr-7a26e0)
func (h *Handler) adminAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.adminToken == "" {
			if !h.allowNoAuth {
				writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing admin bearer token")
				return
			}
			next(w, r)
			return
		}
		hdr := r.Header.Get("Authorization")
		if !strings.HasPrefix(hdr, "Bearer ") || strings.TrimPrefix(hdr, "Bearer ") != h.adminToken {
			writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing admin bearer token")
			return
		}
		next(w, r)
	}
}

// logging injects a request_id into the context and logs every request at Info.
//
// Fields logged: method, path, query (omitted when empty), status, latency_ms, request_id.
// 5xx responses are logged at Warn to surface server errors without changing log level.
func logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := uuid.NewString()
		ctx := backend.WithRequestID(r.Context(), requestID)
		r = r.WithContext(ctx)

		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rw.status,
			"latency_ms", time.Since(start).Milliseconds(),
			"request_id", requestID,
		}
		if q := r.URL.RawQuery; q != "" {
			attrs = append(attrs, "query", q)
		}
		if rw.status >= 500 {
			slog.Warn("http", attrs...)
		} else {
			slog.Info("http", attrs...)
		}
	})
}

// RequestID returns the request ID injected by the logging middleware,
// or an empty string if no request ID is present in ctx.
// Delegates to backend.RequestIDFromCtx — the key is defined there to avoid
// import cycles (router and server both need it).
func RequestID(ctx context.Context) string {
	return backend.RequestIDFromCtx(ctx)
}

// responseWriter wraps http.ResponseWriter to capture the status code.
type responseWriter struct {
	http.ResponseWriter
	status int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.status = code
	rw.ResponseWriter.WriteHeader(code)
}
