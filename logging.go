package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/rs/xid"
)

// Log formats for [log] format.
const (
	logFormatText = "text"
	logFormatJSON = "json"
)

// requestIDHeader carries the request ID back to the client. The ID is always generated
// here; an incoming header of the same name is ignored, so nothing the client sends ends
// up in the logs.
const requestIDHeader = "X-Request-Id"

// parseLogLevel reads a [log] level: debug, info, warn or error (case-insensitive).
func parseLogLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, fmt.Errorf("log.level %q: must be debug, info, warn or error", s)
}

// newLogger builds the process logger from the [log] settings, writing to w.
func newLogger(w io.Writer, cfg Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == logFormatJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

type requestIDKey struct{}

// ctxLogger returns the default logger tagged with the ID of the request ctx belongs to,
// for log lines written deep in a handler. The ID is the generated one, never client input.
func ctxLogger(ctx context.Context) *slog.Logger {
	if id, ok := ctx.Value(requestIDKey{}).(string); ok {
		return slog.Default().With("request_id", id)
	}
	return slog.Default()
}

// requestIDOf returns the ID assigned to the request that w answers, for log lines
// written by handlers.
func requestIDOf(w http.ResponseWriter) string { return w.Header().Get(requestIDHeader) }

// statusRecorder remembers the status code a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap lets http.ResponseController reach the underlying writer (flush, deadlines).
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// observe is the outermost middleware: it assigns the request ID, then logs the request
// and feeds the metrics once the handler returns.
//
// Log lines carry only values the server produced itself: the request ID, the route
// pattern the mux matched, a method from a fixed list, status and duration. The raw URL
// path and the client address are deliberately left out.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := xid.New().String()
		w.Header().Set(requestIDHeader, id)
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		// The mux records the matched pattern on the request it receives, so keep using
		// this one after the handler returns.
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, id))
		next.ServeHTTP(rec, r)

		elapsed := time.Since(start)
		method := methodLabel(r.Method)
		s.metrics.observeRequest(method, rec.status, elapsed)

		level := slog.LevelInfo
		switch {
		case rec.status >= http.StatusInternalServerError:
			level = slog.LevelError
		case strings.HasSuffix(r.Pattern, " /healthz") || strings.HasSuffix(r.Pattern, " /metrics"):
			level = slog.LevelDebug // probes and scrapes would drown the log
		}
		slog.Log(r.Context(), level, "request", "request_id", id, "method", method,
			"route", r.Pattern, "status", rec.status, "duration", elapsed)
	})
}
