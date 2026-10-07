package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"
)

const healthTimeout = 2 * time.Second

// checkWritable proves dir accepts new files by creating and removing one.
func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".healthz-*")
	if err != nil {
		return err
	}
	return errors.Join(f.Close(), os.Remove(f.Name()))
}

// handleHealth reports whether the instance can do its job: the database answers and the
// site and staging directories are writable. It needs no login so load balancers and
// orchestrators can probe it, and it only says ok or fail per check, never why; the
// reason goes to the log.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()

	checks := map[string]string{"database": "ok", "data_dir": "ok"}
	if err := s.db.PingContext(ctx); err != nil {
		slog.Warn("health: database", "err", err, "request_id", requestIDOf(w))
		checks["database"] = "fail"
	}
	if err := errors.Join(checkWritable(s.sites), checkWritable(s.tmp)); err != nil {
		slog.Warn("health: data directory", "err", err, "request_id", requestIDOf(w))
		checks["data_dir"] = "fail"
	}

	status, code := "ok", http.StatusOK
	for _, v := range checks {
		if v != "ok" {
			status, code = "fail", http.StatusServiceUnavailable
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, code, map[string]any{"status": status, "checks": checks})
}
