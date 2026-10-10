// Package logging holds the log settings shared by the configuration loader and the
// process logger: the accepted [log] formats and levels, and the slog setup built from them.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Log formats accepted by [log] format.
const (
	FormatText = "text"
	FormatJSON = "json"
)

// ParseLevel reads a [log] level: debug, info, warn or error (case-insensitive). Anything
// else, including slog's "INFO+1" offsets, is an error.
func ParseLevel(s string) (slog.Level, error) {
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

// New builds a logger writing to w at the given minimum level, as JSON when format is
// FormatJSON and as key=value text otherwise.
func New(w io.Writer, level slog.Level, format string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if format == FormatJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}
