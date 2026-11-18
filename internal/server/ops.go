package server

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// accessLog writes one structured record per request. It logs the chi route
// pattern next to the path, and the path is replaced by the pattern where it
// carries a secret (lead-magnet download tokens).
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		next.ServeHTTP(ww, r)
		route := ""
		if rc := chi.RouteContext(r.Context()); rc != nil {
			route = rc.RoutePattern()
		}
		path := r.URL.Path
		if strings.Contains(route, "{token}") {
			path = route
		}
		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}
		lvl := slog.LevelInfo
		switch {
		case r.URL.Path == "/livez" || r.URL.Path == "/readyz" || r.URL.Path == "/healthz":
			lvl = slog.LevelDebug // probes would drown everything else
		case status >= 500:
			lvl = slog.LevelError
		}
		slog.Default().LogAttrs(r.Context(), lvl, "http",
			slog.String("method", r.Method),
			slog.String("path", path),
			slog.String("route", route),
			slog.Int("status", status),
			slog.Int("bytes", ww.BytesWritten()),
			slog.Int64("duration_ms", time.Since(start).Milliseconds()),
			slog.String("ip", clientIP(r)),
			slog.String("request_id", middleware.GetReqID(r.Context())),
		)
	})
}

// handleLive is the liveness probe: the process is up and serving HTTP. It
// never touches the database, so a database outage doesn't get the container
// restarted in a loop.
func (s *Server) handleLive(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady is the readiness probe: 503 while shutting down (so the load
// balancer stops routing here first) or while the database is unreachable.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		writeError(w, http.StatusServiceUnavailable, "shutting_down", "server is shutting down")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		writeError(w, http.StatusServiceUnavailable, "db_unavailable", "database unreachable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// SetDraining marks the server as shutting down: /readyz starts failing.
func (s *Server) SetDraining() { s.draining.Store(true) }

// FlushPending sends queued revalidate webhooks now instead of waiting for
// the debounce timer, so edits made just before a shutdown still reach the FE.
func (s *Server) FlushPending() {
	rv := s.reval
	rv.mu.Lock()
	if rv.timer != nil {
		rv.timer.Stop()
	}
	rv.mu.Unlock()
	s.flushRevalidate()
}
