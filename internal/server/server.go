// internal/server/server.go — HTTP server setup and bearer auth middleware.
//
// All routes require Authorization: Bearer <token>.
// The API surface is OpenAI-compatible at /v1/chat/completions with
// router-specific extensions at /health, /doctor, /quota, /metrics,
// and /backends/{id}/* control endpoints.
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

// New creates a new Server. token is the bearer token for authentication.
func New(addr, token string, r *router.Router, st *store.Store) *Server {
	h := &Handler{router: r, store: st, token: token}
	mux := http.NewServeMux()

	// OpenAI-compatible inference
	mux.HandleFunc("POST /v1/chat/completions", h.auth(h.chatCompletions))
	mux.HandleFunc("GET /v1/models", h.auth(h.models))

	// Health and observability
	mux.HandleFunc("GET /health", h.auth(h.health))
	mux.HandleFunc("GET /doctor", h.auth(h.doctor))
	mux.HandleFunc("GET /quota", h.auth(h.quota))
	mux.HandleFunc("GET /metrics", h.auth(h.metrics))
	mux.HandleFunc("GET /logs", h.auth(h.logs))
	mux.HandleFunc("GET /stats", h.auth(h.stats))

	// Internal event ingestion (from clagentic-console and other first-party callers)
	mux.HandleFunc("POST /v1/internal/rate-limit", h.auth(h.rateLimitEvent))

	// Backend control
	mux.HandleFunc("POST /backends/{id}/reset", h.auth(h.backendReset))
	mux.HandleFunc("POST /backends/{id}/disable", h.auth(h.backendDisable))
	mux.HandleFunc("POST /backends/{id}/enable", h.auth(h.backendEnable))

	// Webhook management
	mux.HandleFunc("POST /webhooks", h.auth(h.webhookCreate))
	mux.HandleFunc("DELETE /webhooks/{id}", h.auth(h.webhookDelete))
	mux.HandleFunc("GET /webhooks", h.auth(h.webhookList))

	// Version (no auth — useful for healthcheck scripts)
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

// auth wraps a handler with bearer token authentication.
func (h *Handler) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.token != "" {
			hdr := r.Header.Get("Authorization")
			if !strings.HasPrefix(hdr, "Bearer ") || strings.TrimPrefix(hdr, "Bearer ") != h.token {
				writeError(w, http.StatusUnauthorized, "unauthorized", "invalid or missing bearer token")
				return
			}
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
