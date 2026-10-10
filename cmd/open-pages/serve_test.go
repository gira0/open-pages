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

// newServeTestServer builds a server with the given settings applied to the default config.
func newServeTestServer(t *testing.T, mutate func(*config.Config)) *Server {
	t.Helper()
	cfg := config.Default()
	cfg.DataPath = t.TempDir()
	cfg.TmpPath = t.TempDir()
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

// deployReserved deploys a site whose name createSite refuses, by inserting its row
// directly, to prove the resolver doesn't let such a site shadow anything.
func deployReserved(t *testing.T, s *Server, site string, files map[string]string) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), "INSERT INTO docs (name) VALUES (?)", site); err != nil {
		t.Fatal(err)
	}
	if _, err := deployNamed(t, s, site, writeZip(t, files)); err != nil {
		t.Fatal(err)
	}
}

// do sends a request straight to the handler. host may be "" for the default.
func do(s *Server, method, host, target string, hdr ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, nil)
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

func TestPathModeServing(t *testing.T) {
	s := newServeTestServer(t, nil)
	deploySite(t, s, "blog", blogFiles)

	missing := "<h1>custom missing</h1>"
	runServeCases(t, s, "", []serveCase{
		{"site root", "/blog/", 200, "<h1>blog</h1>", "text/html", ""},
		{"root without slash redirects", "/blog", 301, "", "", "./blog/"},
		{"explicit index", "/blog/index.html", 200, "<h1>blog</h1>", "text/html", ""},
		{"css", "/blog/app.css", 200, "body{}", "text/css", ""},
		{"json", "/blog/data.json", 200, `{"a":1}`, "application/json", ""},
		{"nested file", "/blog/nested/deep/x.txt", 200, "x", "text/plain", ""},
		{"directory index", "/blog/docs/", 200, "<h1>docs</h1>", "text/html", ""},
		{"directory redirects", "/blog/docs", 301, "", "", "./docs/"},
		{"redirect keeps query", "/blog/docs?a=b", 301, "", "", "./docs/?a=b"},
		{"no listing", "/blog/nested/", 404, missing, "text/html", ""},
		{"no listing without index", "/blog/empty/", 404, missing, "text/html", ""},
		{"custom 404", "/blog/nope.html", 404, missing, "text/html", ""},
		{"file with slash", "/blog/app.css/", 404, missing, "text/html", ""},
		{"unknown site", "/ghost/", 404, "404 page not found", "text/plain", ""},
		{"invalid site name", "/Blog/", 404, "404 page not found", "text/plain", ""},
		{"bare host", "/", 404, "404 page not found", "text/plain", ""},
	})
}

func TestSitePlain404WithoutCustomPage(t *testing.T) {
	s := newServeTestServer(t, nil)
	deploySite(t, s, "bare", map[string]string{"index.html": "hi"})
	w := do(s, "GET", "", "/bare/missing")
	if w.Code != 404 || !strings.Contains(w.Body.String(), "404 page not found") {
		t.Fatalf("got %d %q", w.Code, w.Body)
	}
}

func TestSiteNotDeployed(t *testing.T) {
	s := newServeTestServer(t, nil)
	if _, err := s.createSite(t.Context(), "empty", "", testOwner(t, s), 0, visPublic); err != nil {
		t.Fatal(err)
	}
	expectStatus(t, do(s, "GET", "", "/empty/").Code, http.StatusNotFound)
}

func TestSiteHeadersAndConditional(t *testing.T) {
	s := newServeTestServer(t, nil)
	deploySite(t, s, "blog", blogFiles)

	w := do(s, "GET", "", "/blog/app.css")
	etag, lm := w.Header().Get("ETag"), w.Header().Get("Last-Modified")
	if etag == "" || lm == "" {
		t.Fatalf("missing validators: ETag %q Last-Modified %q", etag, lm)
	}
	if w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers: %v", w.Header())
	}

	expectStatus(t, do(s, "GET", "", "/blog/app.css", "If-None-Match", etag).Code, http.StatusNotModified)
	expectStatus(t, do(s, "GET", "", "/blog/app.css", "If-Modified-Since", lm).Code, http.StatusNotModified)

	w = do(s, "GET", "", "/blog/app.css", "Range", "bytes=0-3")
	expectStatus(t, w.Code, http.StatusPartialContent)
	if w.Body.String() != "body" {
		t.Errorf("range body %q", w.Body)
	}
}

func TestSiteHEAD(t *testing.T) {
	s := newServeTestServer(t, nil)
	deploySite(t, s, "blog", blogFiles)

	w := do(s, "HEAD", "", "/blog/app.css")
	expectStatus(t, w.Code, http.StatusOK)
	if w.Body.Len() != 0 || w.Header().Get("Content-Length") != "6" {
		t.Errorf("HEAD: body %q, Content-Length %q", w.Body, w.Header().Get("Content-Length"))
	}
	w = do(s, "HEAD", "", "/blog/nope")
	expectStatus(t, w.Code, http.StatusNotFound)
	if w.Body.Len() != 0 {
		t.Errorf("HEAD 404 had a body: %q", w.Body)
	}
}

func TestSiteMethodNotAllowed(t *testing.T) {
	s := newServeTestServer(t, nil)
	deploySite(t, s, "blog", blogFiles)
	expectStatus(t, do(s, "POST", "", "/blog/").Code, http.StatusMethodNotAllowed)
}

// A new deploy and a rollback must change what is served and the ETag.
func TestSiteFollowsCurrentVersion(t *testing.T) {
	s := newServeTestServer(t, nil)
	deploySite(t, s, "blog", map[string]string{"index.html": "v1"})
	old, _ := s.CurrentVersion("blog")
	tag1 := do(s, "GET", "", "/blog/").Header().Get("ETag")

	if _, err := deployNamed(t, s, "blog", writeZip(t, map[string]string{"index.html": "v2"})); err != nil {
		t.Fatal(err)
	}
	w := do(s, "GET", "", "/blog/", "If-None-Match", tag1)
	if w.Code != 200 || w.Body.String() != "v2" {
		t.Fatalf("after deploy: %d %q", w.Code, w.Body)
	}
	if err := s.SwitchCurrent("blog", old); err != nil {
		t.Fatal(err)
	}
	if got := do(s, "GET", "", "/blog/").Body.String(); got != "v1" {
		t.Fatalf("after rollback: %q", got)
	}
}

// Site names must not shadow API routes in path mode.
func TestPathModeDoesNotShadowAPI(t *testing.T) {
	s := newServeTestServer(t, nil)
	expectStatus(t, do(s, "GET", "", "/v1/ping").Code, http.StatusOK)
	expectStatus(t, do(s, "GET", "", "/index").Code, http.StatusOK)
	// Even if a site called v1 exists (created behind the API's back), the API wins.
	deployReserved(t, s, "v1", map[string]string{"ping": "evil"})
	if got := do(s, "GET", "", "/v1/ping").Body.String(); !strings.Contains(got, "pong") {
		t.Errorf("/v1/ping = %q", got)
	}
	if got := do(s, "GET", "", "/v1/other"); got.Code != 404 || strings.Contains(got.Body.String(), "evil") {
		t.Errorf("/v1/other served a site: %d", got.Code)
	}
}

func TestCreateSiteRejectsReserved(t *testing.T) {
	s := newServeTestServer(t, nil)
	for _, name := range []string{"www", "api", "v1"} {
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
	for _, name := range []string{"v1", "index", "www", "api", "admin", "ui", "static", "assets", "health", "metrics", "login"} {
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

	for _, target := range attackTargets {
		checkAttack(t, do(s, "GET", "", "/blog/"+target), "/blog/"+target)
	}
	// A symlink that stays inside the site is fine.
	if w := do(s, "GET", "", "/blog/link-inside"); w.Code != 200 || w.Body.String() != "body{}" {
		t.Errorf("inner symlink: %d %q", w.Code, w.Body)
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

func TestLoadConfigURLMode(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "s.ini")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg, err := config.Load(write("[sites]\n"))
	if err != nil || cfg.URLMode != config.URLModePath {
		t.Fatalf("default: %q %v", cfg.URLMode, err)
	}
	if _, err := config.Load(write("[sites]\nurl_mode = bogus\n")); err == nil {
		t.Error("expected an error for an unknown url_mode")
	}
}
