// internal/server/cache_metrics.go — GET <cache_metrics.path> handler.
//
// Exposes the per-(backend,model) prompt-cache token aggregates captured at
// the router's Invoke boundary (lr-718af0, see router.recordCacheUsage and
// store.RecordCacheUsage) in Prometheus text format, mirroring the existing
// GET /metrics handler's exposition style. Registered only when
// cache_metrics.enabled is true (see server.go) — this file's handler is
// never reachable on an unconfigured install, matching the feature's
// opt-in contract.
//
// Counts/aggregates only: this handler emits integer token counts and call
// counters, never prompt content or request/response bodies (same boundary
// lr-4aaf2a and cache_usage.go's schema draw).
package server

import (
	"fmt"
	"net/http"
)

// cacheMetrics handles GET <cache_metrics.path> — Prometheus text format
// per-(backend,model) cache-token aggregates.
//
// router_cache_calls_reported / router_cache_calls_unsupported are separate
// series, never folded into one another or into a derived rate directly in
// this handler — a consumer computing hit-rate from
// router_cache_read_tokens_total / router_cache_input_tokens_total must
// itself check router_cache_calls_reported > 0 before treating a resulting
// 0 as a genuine miss rather than "no reporting adapter has run yet" (see
// store.CacheUsageRow.HitRate's doc for the same caveat at the Go level).
func (h *Handler) cacheMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")

	if h.store == nil {
		w.WriteHeader(http.StatusOK)
		return
	}

	rows, err := h.store.AllCacheUsage(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	w.WriteHeader(http.StatusOK)
	for _, row := range rows {
		fmt.Fprintf(w, "router_cache_input_tokens_total{backend=%q,model=%q} %d\n", row.BackendID, row.Model, row.InputTokensTotal)
		fmt.Fprintf(w, "router_cache_read_tokens_total{backend=%q,model=%q} %d\n", row.BackendID, row.Model, row.CacheReadTokensTotal)
		fmt.Fprintf(w, "router_cache_write_tokens_total{backend=%q,model=%q} %d\n", row.BackendID, row.Model, row.CacheWriteTokensTotal)
		fmt.Fprintf(w, "router_cache_calls_reported{backend=%q,model=%q} %d\n", row.BackendID, row.Model, row.CallsReported)
		fmt.Fprintf(w, "router_cache_calls_unsupported{backend=%q,model=%q} %d\n", row.BackendID, row.Model, row.CallsUnsupported)
	}
}
