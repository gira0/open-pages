package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gira0/open-pages/internal/config"
)

const (
	testAppHost       = "pages.corp"
	testContentDomain = "pages-content.corp"
)

// validSites are the required [sites] settings of a settings file; the key that follows
// them is read as part of [sites].
const validSites = "[sites]\napp_host = " + testAppHost + "\ncontent_domain = " + testContentDomain + "\n"

// siteHost is the host a site is served on.
func siteHost(site string) string { return site + "." + testContentDomain }

// newServeTestServer builds a server with the given settings applied to the default config.
func newServeTestServer(t *testing.T, mutate func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.DataPath = t.TempDir()
	cfg.TmpPath = t.TempDir()
	cfg.AppHost, cfg.ContentDomain = testAppHost, testContentDomain
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.db.Close() })
	return s
}

func writeZip(t *testing.T, files map[string]string) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "up-*.zip")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	if _, err := f.Write(zipArchive(t, files)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	return f
}

// deploySite registers site and deploys files as its live version.
func deploySite(t *testing.T, s *Server, site string, files map[string]string) {
	t.Helper()
	created, err := s.createSite(t.Context(), site, "", testOwner(t, s), 0, visPublic)
	if err != nil {
		t.Fatal(err)
	}
	if created.Name != site {
		t.Fatalf("created %q, want %q", created.Name, site)
	}
	if _, err := deployNamed(t, s, site, writeZip(t, files)); err != nil {
		t.Fatal(err)
	}
}

// do sends a request straight to the handler. host may be "" for the app host.
func do(s *Server, method, host, target string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
	r.Host = testAppHost
	if host != "" {
		r.Host = host
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	return w
}

var blogFiles = map[string]string{
	"index.html":        "<h1>blog</h1>",
	"404.html":          "<h1>custom missing</h1>",
	"app.css":           "body{}",
	"data.json":         `{"a":1}`,
	"docs/index.html":   "<h1>docs</h1>",
	"docs/guide.html":   "guide",
	"empty/.keep":       "",
	"nested/deep/x.txt": "x",
}

type serveCase struct {
	name, target string
	status       int
	body, ctype  string
	location     string
}

func runServeCases(t *testing.T, s *Server, host string, tests []serveCase) {
	t.Helper()
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := do(s, "GET", host, tc.target)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d (body %q)", w.Code, tc.status, w.Body)
			}
			if tc.body != "" && strings.TrimSpace(w.Body.String()) != tc.body {
				t.Errorf("body %q, want %q", w.Body, tc.body)
			}
			if got := w.Header().Get("Content-Type"); tc.ctype != "" && !strings.HasPrefix(got, tc.ctype) {
				t.Errorf("Content-Type %q, want prefix %q", got, tc.ctype)
			}
			if tc.location != "" && w.Header().Get("Location") != tc.location {
				t.Errorf("Location %q, want %q", w.Header().Get("Location"), tc.location)
			}
		})
	}
}

func TestSiteServing(t *testing.T) {
	s := newServeTestServer(t, nil)
	deploySite(t, s, "blog", blogFiles)

	missing := "<h1>custom missing</h1>"
	runServeCases(t, s, siteHost("blog"), []serveCase{
		{"site root", "/", 200, "<h1>blog</h1>", "text/html", ""},
		{"explicit index", "/index.html", 200, "<h1>blog</h1>", "text/html", ""},
		{"css", "/app.css", 200, "body{}", "text/css", ""},
		{"json", "/data.json", 200, `{"a":1}`, "application/json", ""},
		{"nested file", "/nested/deep/x.txt", 200, "x", "text/plain", ""},
		{"directory index", "/docs/", 200, "<h1>docs</h1>", "text/html", ""},
		{"directory redirects", "/docs", 301, "", "", "./docs/"},
		{"redirect keeps query", "/docs?a=b", 301, "", "", "./docs/?a=b"},
		{"no listing", "/nested/", 404, missing, "text/html", ""},
		{"no listing without index", "/empty/", 404, missing, "text/html", ""},
		{"custom 404", "/nope.html", 404, missing, "text/html", ""},
		{"file with slash", "/app.css/", 404, missing, "text/html", ""},
		{"site name in path is just a path", "/blog/", 404, missing, "text/html", ""},
	})
	// Host matching ignores case, a port and a trailing dot.
	for _, host := range []string{"BLOG.Pages-Content.Corp", "blog.pages-content.corp:8080", "blog.pages-content.corp.", "blog.pages-content.corp.:8080"} {
		if w := do(s, "GET", host, "/app.css"); w.Code != 200 {
			t.Errorf("host %q: status %d", host, w.Code)
		}
	}
}

// The app host serves the API, the UI and static assets, and never site content, even for
// a path that matches a site name or a file of one.
func TestAppHostNeverServesSites(t *testing.T) {
	s := newServeTestServer(t, nil)
	deploySite(t, s, "blog", blogFiles)
	// A site named like an API route segment (inserted behind the API's back) is only ever
	// reachable on its own host.
	if _, err := s.db.ExecContext(t.Context(), "INSERT INTO docs (name) VALUES ('v1')"); err != nil {
		t.Fatal(err)
	}
	if _, err := deployNamed(t, s, "v1", writeZip(t, map[string]string{"ping": "evil", "index.html": "v1 site"})); err != nil {
		t.Fatal(err)
	}

	for _, host := range []string{testAppHost, "PAGES.corp.", "pages.corp:8080"} {
		expectStatus(t, do(s, "GET", host, "/v1/ping").Code, http.StatusOK)
		expectStatus(t, do(s, "GET", host, "/index").Code, http.StatusOK)
		for _, target := range []string{"/blog/", "/blog/app.css", "/blog", "/app.css", "/v1/other", "/v1/ping/", "/"} {
			w := do(s, "GET", host, target)
			if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "blog") || strings.Contains(w.Body.String(), "evil") {
				t.Errorf("%s %s: %d %q, want a plain 404", host, target, w.Code, w.Body)
			}
		}
	}
	if got := do(s, "GET", "", "/v1/ping").Body.String(); !strings.Contains(got, "pong") {
		t.Errorf("/v1/ping = %q", got)
	}
	if got := do(s, "GET", siteHost("v1"), "/").Body.String(); got != "v1 site" {
		t.Errorf("v1 site = %q", got)
	}
}

// A content host serves only its site: never the API, the UI or static assets.
func TestContentHostServesOnlySites(t *testing.T) {
	s := newServeTestServer(t, nil)
	deploySite(t, s, "blog", blogFiles)

	for _, target := range []string{"/v1/ping", "/index", "/v1/openapi.yaml", "/v1/sites", s.static.prefix() + "app.js", "/healthz"} {
		if w := do(s, "GET", siteHost("blog"), target); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "pong") {
			t.Errorf("%s on a content host: %d %q", target, w.Code, w.Body)
		}
	}
	expectStatus(t, do(s, "GET", siteHost("ghost"), "/").Code, http.StatusNotFound)
}

// Hosts that are neither the app host nor a site host get a 404 and never reach the API.
func TestUnknownHosts(t *testing.T) {
	s := newServeTestServer(t, nil)
	deploySite(t, s, "blog", blogFiles)

	for _, host := range []string{
		"evil.example", "example.com", "127.0.0.1", "localhost:8080", "[::1]:8080", "pages-content.corp", "blogpages-content.corp",
		"Bad_Name.pages-content.corp", "a.blog.pages-content.corp", "-.pages-content.corp", "..pages-content.corp", ".pages-content.corp",
		"blog.pages-content.corp/../pages.corp", "blog.pages.corp", "pages.corp.evil.example", "sub.pages.corp",
	} {
		for _, target := range []string{"/v1/ping", "/index", "/app.css", "/", "/v1/auth/user"} {
			if w := do(s, "GET", host, target); w.Code != http.StatusNotFound {
				t.Errorf("host %q %s: status %d, want 404", host, target, w.Code)
			}
		}
		if w := do(s, "POST", host, "/v1/user/login"); w.Code == http.StatusOK || w.Code == http.StatusCreated {
			t.Errorf("host %q reached the API: %d", host, w.Code)
		}
	}
	// An empty Host header is just another unknown host.
	r := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	r.Host = ""
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, r)
	expectStatus(t, w.Code, http.StatusNotFound)
}

// Health probes by IP or localhost keep working on any host; nothing else does there.
func TestHealthzOnAnyHost(t *testing.T) {
	s := newServeTestServer(t, nil)
	s.cfg.MetricsToken = testMetricsToken
	for _, host := range []string{testAppHost, "127.0.0.1:8080", "localhost", "10.0.0.7", "other.example"} {
		expectStatus(t, do(s, "GET", host, "/healthz").Code, http.StatusOK)
		expectStatus(t, do(s, "HEAD", host, "/healthz").Code, http.StatusOK)
	}
	// Only GET and HEAD of the exact path: no 405s, no redirects.
	for _, req := range [][2]string{{"POST", "/healthz"}, {"GET", "/healthz/"}, {"GET", "//healthz"}, {"GET", "/x/../healthz"}} {
		expectStatus(t, do(s, req[0], "10.0.0.7", req[1]).Code, http.StatusNotFound)
	}
	// /metrics is for the app host only (and the dedicated listener).
	expectStatus(t, do(s, "GET", "127.0.0.1:8080", "/metrics", "Authorization", "Bearer "+testMetricsToken).Code, http.StatusNotFound)
	expectStatus(t, do(s, "GET", testAppHost, "/metrics", "Authorization", "Bearer "+testMetricsToken).Code, http.StatusOK)
}

func TestSitePlain404WithoutCustomPage(t *testing.T) {
	s := newServeTestServer(t, nil)
	deploySite(t, s, "bare", map[string]string{"index.html": "hi"})
	w := do(s, "GET", siteHost("bare"), "/missing")
	if w.Code != 404 || !strings.Contains(w.Body.String(), "404 page not found") {
		t.Fatalf("got %d %q", w.Code, w.Body)
	}
}

func TestSiteNotDeployed(t *testing.T) {
	s := newServeTestServer(t, nil)
	if _, err := s.createSite(t.Context(), "empty", "", testOwner(t, s), 0, visPublic); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, do(s, "GET", siteHost("empty"), "/").Code, http.StatusNotFound)
}

func TestSiteHeadersAndConditional(t *testing.T) {
	s := newServeTestServer(t, nil)
	deploySite(t, s, "blog", blogFiles)
	host := siteHost("blog")

	w := do(s, "GET", host, "/app.css")
	etag, lm := w.Header().Get("ETag"), w.Header().Get("Last-Modified")
	if etag == "" || lm == "" {
		t.Fatalf("missing validators: ETag %q Last-Modified %q", etag, lm)
	}
	if w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers: %v", w.Header())
	}

	expectStatus(t, do(s, "GET", host, "/app.css", "If-None-Match", etag).Code, http.StatusNotModified)
	expectStatus(t, do(s, "GET", host, "/app.css", "If-Modified-Since", lm).Code, http.StatusNotModified)

	w = do(s, "GET", host, "/app.css", "Range", "bytes=0-3")
	expectStatus(t, w.Code, http.StatusPartialContent)
	if w.Body.String() != "body" {
		t.Errorf("range body %q", w.Body)
	}
}

func TestSiteHEAD(t *testing.T) {
	s := newServeTestServer(t, nil)
	deploySite(t, s, "blog", blogFiles)
	host := siteHost("blog")

	w := do(s, "HEAD", host, "/app.css")
	expectStatus(t, w.Code, http.StatusOK)
	if w.Body.Len() != 0 || w.Header().Get("Content-Length") != "6" || w.Header().Get("ETag") == "" {
		t.Errorf("HEAD: body %q, Content-Length %q, ETag %q", w.Body, w.Header().Get("Content-Length"), w.Header().Get("ETag"))
	}
	w = do(s, "HEAD", host, "/nope")
	expectStatus(t, w.Code, http.StatusNotFound)
	if w.Body.Len() != 0 {
		t.Errorf("HEAD 404 had a body: %q", w.Body)
	}
}

// Only GET and HEAD are allowed on a content host, whatever the path.
func TestSiteMethodNotAllowed(t *testing.T) {
	s := newServeTestServer(t, nil)
	deploySite(t, s, "blog", blogFiles)
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH", "OPTIONS"} {
		for _, target := range []string{"/", "/v1/auth/sites", "/app.css"} {
			w := do(s, method, siteHost("blog"), target)
			if w.Code != http.StatusMethodNotAllowed || w.Header().Get("Allow") != "GET, HEAD" {
				t.Errorf("%s %s: %d Allow %q", method, target, w.Code, w.Header().Get("Allow"))
			}
		}
	}
}

// A new deploy and a rollback must change what is served and the ETag.
func TestSiteFollowsCurrentVersion(t *testing.T) {
	s := newServeTestServer(t, nil)
	deploySite(t, s, "blog", map[string]string{"index.html": "v1"})
	host := siteHost("blog")
	old, _ := s.CurrentVersion("blog")
	tag1 := do(s, "GET", host, "/").Header().Get("ETag")

	if _, err := deployNamed(t, s, "blog", writeZip(t, map[string]string{"index.html": "v2"})); err != nil {
		t.Fatal(err)
	}
	w := do(s, "GET", host, "/", "If-None-Match", tag1)
	if w.Code != 200 || w.Body.String() != "v2" {
		t.Fatalf("after deploy: %d %q", w.Code, w.Body)
	}
	if err := s.SwitchCurrent("blog", old); err != nil {
		t.Fatal(err)
	}
	if got := do(s, "GET", host, "/").Body.String(); got != "v1" {
		t.Fatalf("after rollback: %q", got)
	}
}

func TestCreateSiteRejectsReserved(t *testing.T) {
	s := newServeTestServer(t, nil)
	for _, name := range []string{"www", "api", "admin"} {
		if _, err := s.createSite(t.Context(), name, "", testOwner(t, s), 0, visPublic); err == nil {
			t.Errorf("createSite(%q) succeeded", name)
		}
	}
	if _, err := s.createSite(t.Context(), "blog", "", testOwner(t, s), 0, visPublic); err != nil {
		t.Errorf("createSite(blog): %v", err)
	}
}

func TestReservedNameRejectedOnCreate(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts)
	for _, name := range []string{"www", "api", "admin", "ui", "static", "assets", "login"} {
		got := post(t, c, ts.URL+"/v1/auth/sites", "application/json", []byte(`{"name":"`+name+`"}`))
		expectStatus(t, got, http.StatusBadRequest)
	}
}

// plantEscapes adds symlinks and secrets around the live version of site and returns
// the names of the symlinks that point outside it.
func plantEscapes(t *testing.T, s *Server, site, other string) {
	t.Helper()
	secretDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(secretDir, "secret.txt"), []byte("TOP-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.SiteDir(site), "sibling.txt"), []byte("TOP-SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	live, err := filepath.EvalSymlinks(s.CurrentDir(site))
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{
		"link-file":   filepath.Join(secretDir, "secret.txt"),
		"link-dir":    secretDir,
		"link-up":     "../../sibling.txt",
		"link-other":  filepath.Join(s.SiteDir(other), currentLink),
		"link-inside": "app.css", // allowed: stays inside the site
	} {
		if err := os.Symlink(target, filepath.Join(live, name)); err != nil {
			t.Fatal(err)
		}
	}
}

// attackTargets are paths relative to a site root that try to leave it.
var attackTargets = []string{
	"../other/",
	"%2e%2e/other/",
	"%2e%2e%2f%2e%2e%2fsibling.txt",
	"..%2f..%2fsibling.txt",
	"../../../../etc/passwd",
	"%2e%2e/%2e%2e/%2e%2e/etc/passwd",
	"..%5c..%5csibling.txt",
	"docs/../../../sibling.txt",
	"/..//..//sibling.txt",
	"%00",
	"index.html%00.png",
	"link-file",
	"link-dir/secret.txt",
	"link-up",
	"link-other/index.html",
	"current/index.html",
	"versions/",
}

func checkAttack(t *testing.T, w *httptest.ResponseRecorder, label string) {
	t.Helper()
	body := w.Body.String()
	if strings.Contains(body, "SECRET") || strings.Contains(body, "root:") {
		t.Errorf("%s leaked content: %q", label, body)
	}
	if w.Code == 200 {
		t.Errorf("%s: status 200", label)
	}
}

func TestTraversalAttempts(t *testing.T) {
	s := newServeTestServer(t, nil)
	deploySite(t, s, "blog", blogFiles)
	deploySite(t, s, "other", map[string]string{"index.html": "OTHER-SECRET"})
	plantEscapes(t, s, "blog", "other")
	host := siteHost("blog")

	for _, target := range attackTargets {
		checkAttack(t, do(s, "GET", host, "/"+target), host+"/"+target)
	}
	// A symlink that stays inside the site is fine.
	if w := do(s, "GET", host, "/link-inside"); w.Code != 200 || w.Body.String() != "body{}" {
		t.Errorf("inner symlink: %d %q", w.Code, w.Body)
	}
	// Host header tricks must not select another site's files either.
	for _, h := range []string{"blog.pages-content.corp/../other", "..pages-content.corp", ".pages-content.corp"} {
		checkAttack(t, do(s, "GET", h, "/"), "host "+h)
	}
}

// testOwner makes sure a user row exists to own test sites and returns its id.
func testOwner(t *testing.T, s *Server) int64 {
	t.Helper()
	_, err := s.db.ExecContext(t.Context(),
		"INSERT OR IGNORE INTO user (userid, email, password) VALUES (1, 'owner@example.com', 'x')")
	if err != nil {
		t.Fatal(err)
	}
	return 1
}

// The session cookie is host-only (no Domain attribute), so a browser never sends it to a
// content host, and it is Secure when cookie_secure is on.
func TestSessionCookieIsHostOnly(t *testing.T) {
	s := newServeTestServer(t, func(c *config.Config) { c.CookieSecure = true })
	send := func(path string) *httptest.ResponseRecorder {
		body := strings.NewReader(`{"email":"a@example.com","password":"correct horse"}`)
		r := httptest.NewRequest(http.MethodPost, path, body)
		r.Host = testAppHost
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.routes().ServeHTTP(w, r)
		return w
	}
	expectStatus(t, send("/v1/user/register").Code, http.StatusCreated)
	w := send("/v1/user/login")
	cookies := w.Result().Cookies()
	if w.Code != http.StatusOK || len(cookies) == 0 {
		t.Fatalf("login: %d, %d cookies", w.Code, len(cookies))
	}
	for _, c := range cookies {
		if c.Domain != "" || !c.Secure || !c.HttpOnly {
			t.Errorf("cookie %s: Domain %q Secure %v HttpOnly %v", c.Name, c.Domain, c.Secure, c.HttpOnly)
		}
	}
}
