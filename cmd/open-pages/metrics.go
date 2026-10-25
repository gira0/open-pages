package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"
)

// metricsAuthorized reports whether r may read /metrics: always when no token is
// configured, otherwise only with a matching "Authorization: Bearer <token>" header.
func (s *Server) metricsAuthorized(r *http.Request) bool {
	if s.cfg.MetricsToken == "" {
		return true
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	// Hash first so the comparison is constant-time whatever the lengths are.
	want := sha256.Sum256([]byte(s.cfg.MetricsToken))
	have := sha256.Sum256([]byte(got))
	return subtle.ConstantTimeCompare(want[:], have[:]) == 1
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !s.metricsAuthorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "metrics token required")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	s.metrics.Write(w)
}

// metricsServer returns the dedicated metrics listener, or nil when [metrics] listen is
// empty. It serves only /metrics and is wrapped in the same request logging.
func (s *Server) metricsServer() *http.Server {
	if s.cfg.MetricsListen == "" {
		return nil
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	return &http.Server{
		Addr:              s.cfg.MetricsListen,
		Handler:           s.observe(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
}
