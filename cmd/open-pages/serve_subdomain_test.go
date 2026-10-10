package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gira0/open-pages/internal/config"
)

func newSubdomainServer(t *testing.T) *Server {
	t.Helper()
	return newServeTestServer(t, func(c *config.Config) {
		c.URLMode = config.URLModeSubdomain
		c.BaseDomain = "pages.corp"
	})
}

func TestSubdomainModeServing(t *testing.T) {
	s := newSubdomainServer(t)
	deploySite(t, s, "blog", blogFiles)

	missing := "<h1>custom missing</h1>"
	runServeCases(t, s, "blog.pages.corp", []serveCase{
		{"site root", "/", 200, "<h1>blog</h1>", "text/html", ""},
		{"css", "/app.css", 200, "body{}", "text/css", ""},
		{"directory index", "/docs/", 200, "<h1>docs</h1>", "text/html", ""},
		{"directory redirects", "/docs", 301, "", "", "./docs/"},
		{"no listing", "/nested/", 404, missing, "text/html", ""},
		{"custom 404", "/nope", 404, missing, "text/html", ""},
		{"site name in path is just a path", "/blog/", 404, missing, "text/html", ""},
	})
	// Host matching ignores case, a port and a trailing dot.
	for _, host := range []string{"BLOG.Pages.Corp", "blog.pages.corp:8080", "blog.pages.corp."} {
		if w := do(s, "GET", host, "/app.css"); w.Code != 200 {
			t.Errorf("host %q: status %d", host, w.Code)
		}
	}
}

func TestSubdomainModeUnknownHosts(t *testing.T) {
	s := newSubdomainServer(t)
	deploySite(t, s, "blog", blogFiles)

	for _, host := range []string{"ghost.pages.corp", "Bad_Name.pages.corp", "a.blog.pages.corp", "-.pages.corp"} {
		if w := do(s, "GET", host, "/v1/ping"); w.Code != 404 {
			t.Errorf("host %q: status %d, want 404 (must not reach the API)", host, w.Code)
		}
	}
	// A host that only ends in the same letters is not a subdomain.
	if w := do(s, "GET", "blogpages.corp", "/app.css"); w.Code == 200 {
		t.Errorf("blogpages.corp served a site")
	}
	if w := do(s, "POST", "blog.pages.corp", "/"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST on a site: %d", w.Code)
	}
}

// The API and UI stay on the bare base_domain in subdomain mode, and sites aren't served there.
func TestSubdomainModeBaseDomainIsAPI(t *testing.T) {
	s := newSubdomainServer(t)
	deploySite(t, s, "blog", blogFiles)

	for _, host := range []string{"pages.corp", "pages.corp:8080"} {
		expectStatus(t, do(s, "GET", host, "/v1/ping").Code, http.StatusOK)
		expectStatus(t, do(s, "GET", host, "/index").Code, http.StatusOK)
		expectStatus(t, do(s, "GET", host, "/blog/").Code, http.StatusNotFound)
	}
	// A site called v1 is just another subdomain here.
	deployReserved(t, s, "v1", map[string]string{"index.html": "v1 site"})
	if got := do(s, "GET", "v1.pages.corp", "/").Body.String(); got != "v1 site" {
		t.Errorf("v1 site = %q", got)
	}
}

func TestSubdomainModeHEADAndConditional(t *testing.T) {
	s := newSubdomainServer(t)
	deploySite(t, s, "blog", blogFiles)

	w := do(s, "HEAD", "blog.pages.corp", "/app.css")
	if w.Code != 200 || w.Body.Len() != 0 || w.Header().Get("ETag") == "" {
		t.Fatalf("HEAD: %d %q %v", w.Code, w.Body, w.Header())
	}
	etag := w.Header().Get("ETag")
	expectStatus(t, do(s, "GET", "blog.pages.corp", "/app.css", "If-None-Match", etag).Code, http.StatusNotModified)
}

func TestSubdomainModeTraversal(t *testing.T) {
	s := newSubdomainServer(t)
	deploySite(t, s, "blog", blogFiles)
	deploySite(t, s, "other", map[string]string{"index.html": "OTHER-SECRET"})
	plantEscapes(t, s, "blog", "other")

	for _, target := range attackTargets {
		checkAttack(t, do(s, "GET", "blog.pages.corp", "/"+target), "blog.pages.corp/"+target)
	}
	// Host header tricks must not select another site's files either.
	for _, host := range []string{"blog.pages.corp/../other", "..pages.corp", ".pages.corp"} {
		checkAttack(t, do(s, "GET", host, "/"), "host "+host)
	}
}

func TestLoadConfigSubdomain(t *testing.T) {
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "s.ini")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg, err := config.Load(write("[sites]\nurl_mode = Subdomain\nbase_domain = Pages.Corp\n"))
	if err != nil || cfg.URLMode != config.URLModeSubdomain || cfg.BaseDomain != "pages.corp" {
		t.Fatalf("got %+v, %v", cfg, err)
	}
	for _, bad := range []string{
		"[sites]\nurl_mode = subdomain\n",
		"[sites]\nurl_mode = subdomain\nbase_domain = pages.corp:8080\n",
	} {
		if _, err := config.Load(write(bad)); err == nil || !strings.Contains(err.Error(), "base_domain") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}
