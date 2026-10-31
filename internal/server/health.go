package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
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

// healthProbe runs the filesystem check at most once at a time. A stalled filesystem
// (a hung NFS mount, say) can block the create/remove calls indefinitely and they can't
// be cancelled, so the check runs in its own goroutine and requests wait for it only until
// their deadline. Requests arriving while a check is in flight share its result instead
// of starting another, so blocked goroutines can't pile up: there is at most one.
type healthProbe struct {
	mu   sync.Mutex
	call *probeCall
}

type probeCall struct {
	done chan struct{}
	err  error // valid once done is closed
}

// run returns the result of fn, shared with any concurrent callers, or an error once ctx
// ends first (the check is then reported as stuck).
func (p *healthProbe) run(ctx context.Context, fn func() error) error {
	p.mu.Lock()
	c := p.call
	if c == nil {
		c = &probeCall{done: make(chan struct{})}
		p.call = c
		go func() {
			c.err = fn()
			p.mu.Lock()
			p.call = nil
			p.mu.Unlock()
			close(c.done)
		}()
	}
	p.mu.Unlock()

	select {
	case <-c.done:
		return c.err
	case <-ctx.Done():
		return fmt.Errorf("data directory check did not finish: %w", ctx.Err())
	}
}

// Drain marks the server as shutting down: /readyz answers 503 from now on, so a load
// balancer or Kubernetes stops sending new requests before the listener closes.
func (s *Server) Drain() { s.draining.Store(true) }

// handleLive reports that the process is up and serving. It deliberately does no I/O, not
// even a database ping: a liveness probe that touched a stalled volume would fail, get the
// pod killed and restarted in a loop while the volume is still stalled. Use /readyz for
// anything that depends on storage.
func (s *Server) handleLive(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// afterReadyChecks runs between the readiness checks and the answer; tests use it to
// start draining while a probe is in flight.
var afterReadyChecks = func() {}

// writeDraining is the /readyz answer once Drain has been called.
func writeDraining(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"status": "draining", "checks": map[string]string{}})
}

// handleReady reports whether the instance can do its job: the database answers and the
// site and staging directories are writable. It answers 503 while draining. It needs no
// login so load balancers and orchestrators can probe it, and it only says ok or fail per
// check, never why; the reason goes to the log. /healthz is an older name for it.
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if s.draining.Load() {
		writeDraining(w)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), healthTimeout)
	defer cancel()

	checks := map[string]string{"database": "ok", "data_dir": "ok"}
	if err := s.db.PingContext(ctx); err != nil {
		slog.Warn("health: database", "err", err, "request_id", requestIDOf(w))
		checks["database"] = "fail"
	}
	if err := s.health.run(ctx, func() error {
		return errors.Join(checkWritable(s.sites), checkWritable(s.tmp))
	}); err != nil {
		slog.Warn("health: data directory", "err", err, "request_id", requestIDOf(w))
		checks["data_dir"] = "fail"
	}
	afterReadyChecks()
	// Drain may have started while the checks ran; answering ok now would put a
	// terminating instance back into rotation.
	if s.draining.Load() {
		writeDraining(w)
		return
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
