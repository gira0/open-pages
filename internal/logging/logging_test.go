package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	good := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug}, {"INFO", slog.LevelInfo}, {" Warn ", slog.LevelWarn}, {"error", slog.LevelError},
	}
	for _, c := range good {
		if got, err := ParseLevel(c.in); err != nil || got != c.want {
			t.Errorf("ParseLevel(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{"INFO+1", "WARN-2", "", "trace"} {
		if _, err := ParseLevel(bad); err == nil {
			t.Errorf("ParseLevel(%q) accepted", bad)
		}
	}
}

func TestNew(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, slog.LevelWarn, FormatJSON)
	l.Info("hidden")
	l.Warn("shown", "k", "v")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not one JSON object: %v: %q", err, buf.String())
	}
	if rec["msg"] != "shown" || rec["k"] != "v" {
		t.Errorf("record = %v", rec)
	}

	buf.Reset()
	New(&buf, slog.LevelInfo, FormatText).Warn("plain")
	if !strings.Contains(buf.String(), "msg=plain") {
		t.Errorf("text output = %q", buf.String())
	}
}
