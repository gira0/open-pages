package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gira0/open-pages/internal/config"
	"github.com/gira0/open-pages/internal/logging"
)

const testMetricsToken = "0123456789abcdef-token"

// lockedBuffer is a bytes.Buffer that is safe to share with the server's goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fetched is what the tests need from a response, so no body is left open.
type fetched struct {
	StatusCode int
	Header     http.Header
}

// fetch sends a GET with optional headers and returns the response and its body.
func fetch(t *testing.T, url string, header http.Header) (fetched, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return fetched{resp.StatusCode, resp.Header}, string(body)
}

// metricsTestServer is a test server with /metrics enabled on the main listener.
func metricsTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	_, s := newTestServer(t)
	s.cfg.MetricsToken = testMetricsToken
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	return ts, s
}

func bearer(token string) http.Header {
	return http.Header{"Authorization": {"Bearer " + token}}
}

func TestLogConfig(t *testing.T) {
	write := func(ini string) string {
		path := filepath.Join(t.TempDir(), "settings.ini")
		if err := os.WriteFile(path, []byte(ini), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	cfg, err := config.Load(write("[log]\nlevel = DEBUG\nformat = JSON\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != slog.LevelDebug || cfg.LogFormat != logging.FormatJSON {
		t.Errorf("got level %v format %q", cfg.LogLevel, cfg.LogFormat)
	}

	cfg, err = config.Load(write(""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != slog.LevelInfo || cfg.LogFormat != logging.FormatText {
		t.Errorf("defaults: level %v format %q", cfg.LogLevel, cfg.LogFormat)
	}
	if cfg.MetricsListen != "" || cfg.MetricsToken != "" {
		t.Error("metrics must be off by default")
	}

	for _, ini := range []string{
		"[log]\nlevel = loud\n",
		"[log]\nformat = xml\n",
		"[metrics]\ntoken = short\n",
	} {
		if _, err := config.Load(write(ini)); err == nil {
			t.Errorf("expected an error for %q", ini)
		}
	}

	cfg, err = config.Load(write("[metrics]\nlisten = 127.0.0.1:9100\ntoken = " + testMetricsToken + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MetricsListen != "127.0.0.1:9100" || cfg.MetricsToken != testMetricsToken {
		t.Errorf("metrics config = %q, %q", cfg.MetricsListen, cfg.MetricsToken)
	}
}

func TestNewLoggerFormatsAndLevels(t *testing.T) {
	var buf bytes.Buffer
	cfg := config.Default()
	cfg.LogFormat = logging.FormatJSON
	cfg.LogLevel = slog.LevelWarn
	l := newLogger(&buf, cfg)
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
	cfg.LogFormat = logging.FormatText
	newLogger(&buf, cfg).Warn("plain")
	if !strings.Contains(buf.String(), "msg=plain") {
		t.Errorf("text output = %q", buf.String())
	}
}

func TestRequestIDHeaderAndLog(t *testing.T) {
	var logs lockedBuffer
	cfg := config.Default()
	old := slog.Default()
	slog.SetDefault(newLogger(&logs, cfg))
	t.Cleanup(func() { slog.SetDefault(old) })

	s := &Server{cfg: cfg, metrics: newMetrics()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := s.observe(mux)

	serve := func(incoming string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/v1/ping?secret=x", nil)
		if incoming != "" {
			req.Header.Set(requestIDHeader, incoming)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	first := serve("")
	second := serve("client-chosen\nforged=1")
	id1, id2 := first.Header().Get(requestIDHeader), second.Header().Get(requestIDHeader)
	if id1 == "" || id2 == "" || id1 == id2 {
		t.Fatalf("request IDs %q and %q must be set and distinct", id1, id2)
	}
	out := logs.String()
	if strings.Contains(out, "client-chosen") || strings.Contains(out, "forged") || strings.Contains(out, "secret") {
		t.Errorf("request-derived values leaked into the log: %q", out)
	}
	for _, id := range []string{id1, id2} {
		if !strings.Contains(out, "request_id="+id) {
			t.Errorf("log has no line for request %s: %q", id, out)
		}
	}
	if !strings.Contains(out, `route="GET /v1/ping"`) {
		t.Errorf("log has no route: %q", out)
	}
}

func TestMethodLabelBoundsCardinality(t *testing.T) {
	if methodLabel("GET") != "GET" || methodLabel("BREW") != "OTHER" {
		t.Error("methodLabel must keep known methods and fold the rest")
	}
}

func TestHealthz(t *testing.T) {
	ts, s := newTestServer(t)

	resp, body := fetch(t, ts.URL+"/healthz", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", resp.StatusCode, body)
	}
	if resp.Header.Get(requestIDHeader) == "" {
		t.Error("missing request ID")
	}
	var got struct {
		Status string
		Checks map[string]string
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "ok" || got.Checks["database"] != "ok" || got.Checks["data_dir"] != "ok" {
		t.Errorf("body = %s", body)
	}
	if left, _ := filepath.Glob(filepath.Join(s.sites, ".healthz-*")); len(left) != 0 {
		t.Errorf("probe files left behind: %v", left)
	}

	// An unwritable data directory fails the probe.
	if err := os.RemoveAll(s.tmp); err != nil {
		t.Fatal(err)
	}
	resp, body = fetch(t, ts.URL+"/healthz", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, `"data_dir":"fail"`) {
		t.Errorf("missing tmp dir: status %d body %s", resp.StatusCode, body)
	}
	if err := os.MkdirAll(s.tmp, 0o700); err != nil {
		t.Fatal(err)
	}

	// So does a database that no longer answers.
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}
	resp, body = fetch(t, ts.URL+"/healthz", nil)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, `"database":"fail"`) {
		t.Errorf("closed db: status %d body %s", resp.StatusCode, body)
	}
}

func TestMetricsDisabledByDefault(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, _ := fetch(t, ts.URL+"/metrics", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status %d, want 404 when no token or listener is configured", resp.StatusCode)
	}
}

func TestMetricsRequiresToken(t *testing.T) {
	ts, _ := metricsTestServer(t)

	for name, h := range map[string]http.Header{
		"none":         nil,
		"wrong":        bearer("not-the-right-token-value"),
		"wrong scheme": {"Authorization": {"Basic " + testMetricsToken}},
		"prefix":       bearer(testMetricsToken[:len(testMetricsToken)-1]),
	} {
		resp, _ := fetch(t, ts.URL+"/metrics", h)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, resp.StatusCode)
		}
	}
	resp, body := fetch(t, ts.URL+"/metrics", bearer(testMetricsToken))
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(body, "# TYPE openpages_http_requests_total counter") {
		t.Errorf("body missing request counter:\n%s", body)
	}
}

// metricValue extracts the value of the exposition line starting with prefix.
func metricValue(t *testing.T, body, prefix string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(prefix) + ` (\S+)$`)
	m := re.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no metric %q in:\n%s", prefix, body)
	}
	return m[1]
}

func TestMetricsCountRequestsDeploysAndErrors(t *testing.T) {
	ts, s := metricsTestServer(t)
	c := loggedInClient(t, ts)
	createSiteVia(t, ts, c, "docs-site")

	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites/docs-site/upload", "application/zip",
		zipArchive(t, map[string]string{"index.html": "hi"})), http.StatusOK)
	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites/docs-site/upload", "application/zip",
		[]byte("not an archive")), http.StatusBadRequest)
	expectStatus(t, get(t, c, ts.URL+"/docs-site/some/unique/path"), http.StatusNotFound)

	// Simulate a failing handler to check the error counter.
	rec := httptest.NewRecorder()
	failing := s.observe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		internalError(w, "boom", io.ErrUnexpectedEOF)
	}))
	failing.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/x", nil))

	_, body := fetch(t, ts.URL+"/metrics", bearer(testMetricsToken))
	checks := map[string]string{
		`openpages_deploys_total{result="ok"}`:       "1",
		`openpages_deploys_total{result="rejected"}`: "1",
		`openpages_deploys_total{result="failed"}`:   "0",
		`openpages_errors_total`:                     "1",
	}
	for prefix, want := range checks {
		if got := metricValue(t, body, prefix); got != want {
			t.Errorf("%s = %s, want %s", prefix, got, want)
		}
	}
	if got := metricValue(t, body, `openpages_http_requests_total{method="GET",code="4xx"}`); got != "1" {
		t.Errorf("GET 4xx = %s, want 1", got)
	}
	if got := metricValue(t, body, `openpages_http_requests_total{method="POST",code="4xx"}`); got != "1" {
		t.Errorf("POST 4xx = %s, want 1", got)
	}
	if got := metricValue(t, body, `openpages_http_requests_total{method="GET",code="5xx"}`); got != "1" {
		t.Errorf("GET 5xx = %s, want 1", got)
	}
	// Cardinality: neither the site name nor any path may show up as a label.
	if strings.Contains(body, "docs-site") || strings.Contains(body, "unique") {
		t.Errorf("metrics leak request paths:\n%s", body)
	}
	// The histogram is cumulative and ends at +Inf == count.
	inf := metricValue(t, body, `openpages_http_request_duration_seconds_bucket{le="+Inf"}`)
	if count := metricValue(t, body, `openpages_http_request_duration_seconds_count`); inf != count {
		t.Errorf("+Inf bucket %s != count %s", inf, count)
	}
}

func TestMetricsDedicatedListener(t *testing.T) {
	_, s := newTestServer(t)
	if s.metricsServer() != nil {
		t.Fatal("no metrics server expected without [metrics] listen")
	}

	s.cfg.MetricsListen = "127.0.0.1:0"
	ms := s.metricsServer()
	if ms == nil {
		t.Fatal("expected a metrics server")
	}
	ts := httptest.NewServer(ms.Handler)
	t.Cleanup(ts.Close)

	// With no token the dedicated listener is open: restricting it is the network's job.
	resp, body := fetch(t, ts.URL+"/metrics", nil)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "openpages_errors_total") {
		t.Errorf("status %d body %s", resp.StatusCode, body)
	}
	// It serves nothing else.
	if resp, _ := fetch(t, ts.URL+"/healthz", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("healthz on metrics listener: status %d, want 404", resp.StatusCode)
	}

	// With a token configured it is required on this listener too.
	s.cfg.MetricsToken = testMetricsToken
	ts2 := httptest.NewServer(s.metricsServer().Handler)
	t.Cleanup(ts2.Close)
	if resp, _ := fetch(t, ts2.URL+"/metrics", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", resp.StatusCode)
	}
	if resp, _ := fetch(t, ts2.URL+"/metrics", bearer(testMetricsToken)); resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, want 200", resp.StatusCode)
	}
}

// Site names can't shadow the operational endpoints in path mode.
func TestOpsNamesAreReserved(t *testing.T) {
	for _, name := range []string{"healthz", "readyz", "metrics"} {
		if !reservedSiteName(name) {
			t.Errorf("%q must be a reserved site name", name)
		}
	}
	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts)
	for _, name := range []string{"healthz", "metrics"} {
		body := []byte(`{"name":"` + name + `"}`)
		if got := post(t, c, ts.URL+"/v1/auth/sites", "application/json", body); got == http.StatusCreated {
			t.Errorf("site %q was created", name)
		}
	}
}

func TestHealthProbeSingleFlight(t *testing.T) {
	var p healthProbe
	var calls atomic.Int32
	release := make(chan struct{})
	slow := func() error {
		calls.Add(1)
		<-release
		return nil
	}

	// A stuck check times out the request instead of hanging it.
	for range 3 {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		err := p.run(ctx, slow)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want deadline exceeded", err)
		}
	}
	// Repeated probes shared the one blocked check rather than starting more.
	if n := calls.Load(); n != 1 {
		t.Fatalf("check started %d times while stuck, want 1", n)
	}

	// Once the filesystem answers again, the next probe starts afresh and succeeds.
	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := p.run(ctx, func() error { return nil })
		cancel()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe never recovered: %v", err)
		}
	}
}

func TestLogLevelStrict(t *testing.T) {
	for _, ok := range []string{"debug", "INFO", " Warn ", "error"} {
		if _, err := logging.ParseLevel(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"INFO+1", "WARN-2", "", "trace"} {
		if _, err := logging.ParseLevel(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, level := range []string{"INFO+1", "WARN-2"} {
		path := filepath.Join(t.TempDir(), "settings.ini")
		if err := os.WriteFile(path, []byte("[log]\nlevel = "+level+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := config.Load(path); err == nil {
			t.Errorf("config accepted level %s", level)
		}
	}
}

func TestHandlerLogLinesCarryRequestID(t *testing.T) {
	var logs lockedBuffer
	old := slog.Default()
	slog.SetDefault(newLogger(&logs, config.Default()))
	t.Cleanup(func() { slog.SetDefault(old) })

	s := &Server{cfg: config.Default(), metrics: newMetrics()}
	h := s.observe(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctxLogger(r.Context()).Info("from handler")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	id := rec.Header().Get(requestIDHeader)
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if !strings.Contains(line, "request_id="+id) {
			t.Errorf("line without request ID: %q", line)
		}
	}
	if !strings.Contains(logs.String(), "from handler") {
		t.Error("handler line missing")
	}
}
