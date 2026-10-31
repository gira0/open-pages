package server

import (
	"bytes"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/gira0/open-pages/internal/logging"
	"github.com/gira0/open-pages/internal/metrics"
)

func proxies(t *testing.T, cidrs ...string) []netip.Prefix {
	t.Helper()
	var out []netip.Prefix
	for _, c := range cidrs {
		out = append(out, netip.MustParsePrefix(c))
	}
	return out
}

func TestClientIP(t *testing.T) {
	cases := []struct {
		name    string
		trusted []string
		peer    string
		xff     []string
		want    string
	}{
		{"no proxies configured ignores the header", nil, "203.0.113.9:4000", []string{"198.51.100.1"}, "203.0.113.9"},
		{"untrusted peer cannot spoof", []string{"10.0.0.0/8"}, "203.0.113.9:4000", []string{"198.51.100.1"}, "203.0.113.9"},
		{"trusted peer, one hop", []string{"10.0.0.0/8"}, "10.1.2.3:4000", []string{"198.51.100.1"}, "198.51.100.1"},
		{"trusted peer without header", []string{"10.0.0.0/8"}, "10.1.2.3:4000", nil, "10.1.2.3"},
		{"rightmost untrusted wins over a forged left entry", []string{"10.0.0.0/8"}, "10.1.2.3:4000", []string{"6.6.6.6, 198.51.100.1"}, "198.51.100.1"},
		{"skips trusted hops on the right", []string{"10.0.0.0/8"}, "10.1.2.3:4000", []string{"198.51.100.1, 10.9.9.9, 10.8.8.8"}, "198.51.100.1"},
		{"several header lines", []string{"10.0.0.0/8"}, "10.1.2.3:4000", []string{"6.6.6.6", "198.51.100.1, 10.9.9.9"}, "198.51.100.1"},
		{"every hop trusted", []string{"10.0.0.0/8"}, "10.1.2.3:4000", []string{"10.9.9.9"}, "10.1.2.3"},
		{"garbage entry is not believed", []string{"10.0.0.0/8"}, "10.1.2.3:4000", []string{"198.51.100.1, not-an-ip"}, "10.1.2.3"},
		{"ipv6", []string{"fd00::/8"}, "[fd00::1]:4000", []string{"2001:db8::7"}, "2001:db8::7"},
		{"ipv4-mapped peer matches an ipv4 network", []string{"10.0.0.0/8"}, "[::ffff:10.1.2.3]:4000", []string{"198.51.100.1"}, "198.51.100.1"},
		{"not an ip peer", []string{"10.0.0.0/8"}, "pipe", []string{"198.51.100.1"}, "pipe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			s.cfg.TrustedProxies = proxies(t, tc.trusted...)
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.peer
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := s.clientIP(r); got != tc.want {
				t.Errorf("clientIP = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestIsHTTPSAndOriginFor(t *testing.T) {
	s := &Server{}
	req := func(peer, proto string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = "pages.corp"
		r.RemoteAddr = peer
		if proto != "" {
			r.Header.Set("X-Forwarded-Proto", proto)
		}
		return r
	}
	// Default: the header is never believed.
	if s.isHTTPS(req("10.1.2.3:1", "https")) {
		t.Error("https believed with no trusted proxies")
	}
	if got := s.originFor("a.corp", req("10.1.2.3:1", "https")); got != "http://a.corp" {
		t.Errorf("originFor = %q", got)
	}

	s.cfg.TrustedProxies = proxies(t, "10.0.0.0/8")
	if !s.isHTTPS(req("10.1.2.3:1", "https")) || !s.isHTTPS(req("10.1.2.3:1", "HTTPS")) {
		t.Error("https from a trusted proxy not honoured")
	}
	if s.isHTTPS(req("10.1.2.3:1", "http")) || s.isHTTPS(req("10.1.2.3:1", "")) {
		t.Error("non-https treated as https")
	}
	if s.isHTTPS(req("203.0.113.9:1", "https")) {
		t.Error("X-Forwarded-Proto from an untrusted peer was honoured")
	}
	if s.isHTTPS(req("10.1.2.3:1", "https, http")) {
		t.Error("the nearest proxy's value (the last) must win")
	}
	if got := s.originFor("a.corp", req("10.1.2.3:1", "https")); got != "https://a.corp" {
		t.Errorf("originFor via trusted proxy = %q", got)
	}
	if got := s.originFor("a.corp", req("203.0.113.9:1", "https")); got != "http://a.corp" {
		t.Errorf("originFor via untrusted peer = %q", got)
	}
}

func TestRequestLogClientIP(t *testing.T) {
	var logs lockedBuffer
	old := slog.Default()
	slog.SetDefault(logging.New(&logs, slog.LevelInfo, logging.FormatText))
	t.Cleanup(func() { slog.SetDefault(old) })

	s := &Server{metrics: metrics.New()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {})
	h := s.observe(mux)
	serve := func() {
		r := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
		r.RemoteAddr = "10.1.2.3:4000"
		r.Header.Set("X-Forwarded-For", "198.51.100.1")
		h.ServeHTTP(httptest.NewRecorder(), r)
	}

	serve()
	if strings.Contains(logs.String(), "client_ip") || strings.Contains(logs.String(), "198.51.100.1") {
		t.Errorf("address logged without trusted proxies: %q", logs.String())
	}
	s.cfg.TrustedProxies = proxies(t, "10.0.0.0/8")
	serve()
	if !strings.Contains(logs.String(), "client_ip=198.51.100.1") {
		t.Errorf("client address not logged: %q", logs.String())
	}
}

// The audit trail records the forwarded client address only from a trusted proxy.
func TestAuditSourceUsesTrustedProxy(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.1.2.3:4000"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	src, _ := s.withAuditSource(r, viaSession).Value(auditSourceKey{}).(auditSource)
	if src.ip != "10.1.2.3" {
		t.Errorf("untrusted: ip = %q", src.ip)
	}
	s.cfg.TrustedProxies = proxies(t, "10.0.0.0/8")
	src, _ = s.withAuditSource(r, viaSession).Value(auditSourceKey{}).(auditSource)
	if src.ip != "198.51.100.1" {
		t.Errorf("trusted: ip = %q", src.ip)
	}
}

// With the root filesystem read-only, TMPDIR points at a volume or nowhere writable. The
// server stages everything under tmppath, so a missing TMPDIR must not break uploads of
// the usual size. (Go's multipart parser spills a form file larger than 32 MiB to TMPDIR;
// see docs/getting-started.md.)
func TestUploadsDoNotUseTMPDIR(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir()+"/does/not/exist")
	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts)
	createSiteVia(t, ts, c, "blog")
	files := map[string]string{"index.html": "<h1>hi</h1>"}

	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites/blog/upload", "application/zip", zipArchive(t, files)), http.StatusOK)
	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites/blog/upload", "application/gzip", tarGzArchive(files)), http.StatusOK)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "site.zip")
	fw.Write(zipArchive(t, files))
	mw.Close()
	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites/blog/formupload", mw.FormDataContentType(), body.Bytes()), http.StatusOK)
}
