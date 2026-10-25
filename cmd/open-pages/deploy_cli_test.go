package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gira0/open-pages/internal/cli"
)

func cliEnv(token, server string) func(string) string {
	return func(k string) string {
		switch k {
		case "OPEN_PAGES_TOKEN":
			return token
		case "OPEN_PAGES_URL":
			return server
		}
		return ""
	}
}

func apiToken(t *testing.T, ts *httptest.Server, c *http.Client) string {
	t.Helper()
	return mintToken(t, ts, c, `{"name":"cli"}`)["token"].(string)
}

func TestDeployCommand(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	token := apiToken(t, ts, c)
	dir := writeSiteTree(t, map[string]string{"index.html": "<h1>v1</h1>", "css/site.css": "body{}"})

	var out bytes.Buffer
	// The site doesn't exist yet: deploy creates it.
	if err := cli.DeployCommand(context.Background(), []string{"blog", dir}, cliEnv(token, ts.URL), &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "deployed blog") {
		t.Fatalf("output %q", out.String())
	}
	got, err := os.ReadFile(filepath.Join(s.CurrentDir("blog"), "css", "site.css"))
	if err != nil || string(got) != "body{}" {
		t.Fatalf("css = %q, %v", got, err)
	}

	// Deploying again to the existing site makes a second version; the flag form works too.
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<h1>v2</h1>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cli.DeployCommand(context.Background(), []string{"-server", ts.URL, "blog", dir}, cliEnv(token, ""), &out); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(s.CurrentDir("blog"), "index.html"))
	if string(got) != "<h1>v2</h1>" {
		t.Fatalf("index = %q", got)
	}
	if versions, _ := s.Versions("blog"); len(versions) != 2 {
		t.Fatalf("got versions %v, want 2", versions)
	}
}

func TestDeployVisibility(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	token := apiToken(t, ts, c)
	dir := writeSiteTree(t, map[string]string{"index.html": "hi"})
	visibility := func(name string) string {
		t.Helper()
		site, err := s.getSite(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		return site.Visibility
	}
	deploy := func(args ...string) {
		t.Helper()
		if err := cli.DeployCommand(context.Background(), args, cliEnv(token, ts.URL), &bytes.Buffer{}); err != nil {
			t.Fatal(err)
		}
	}

	// A new site is created with the requested visibility.
	deploy("-visibility", "restricted", "blog", dir)
	if got := visibility("blog"); got != visRestricted {
		t.Fatalf("new site visibility = %q, want restricted", got)
	}
	// Without the flag, a redeploy leaves it alone.
	deploy("blog", dir)
	if got := visibility("blog"); got != visRestricted {
		t.Fatalf("visibility after plain redeploy = %q, want restricted", got)
	}
	// With the flag, an existing site's visibility changes.
	deploy("-visibility", "authenticated", "blog", dir)
	if got := visibility("blog"); got != visAuthenticated {
		t.Fatalf("visibility = %q, want authenticated", got)
	}
	if versions, _ := s.Versions("blog"); len(versions) != 3 {
		t.Fatalf("got versions %v, want 3", versions)
	}
	// Without the flag, a new site is public as before.
	deploy("notes", dir)
	if got := visibility("notes"); got != visPublic {
		t.Fatalf("default visibility = %q, want public", got)
	}
}

// TestDeployVisibilityOrder checks that a visibility change that narrows the audience is
// made before the upload, one that widens it only after a successful upload, and that a
// failed upload leaves the visibility alone.
func TestDeployVisibilityOrder(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	token := apiToken(t, ts, c)
	dir := writeSiteTree(t, map[string]string{"index.html": "hi"})

	// A proxy in front of the server records each request and can fail uploads.
	target, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	var (
		log        []string
		failUpload bool
	)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upload := strings.HasSuffix(r.URL.Path, "/upload")
		if upload {
			log = append(log, "upload")
			if failUpload {
				http.Error(w, "nope", http.StatusInternalServerError)
				return
			}
		} else if r.Method == http.MethodPut {
			log = append(log, "put")
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(front.Close)

	deploy := func(vis string) error {
		log = nil
		return cli.DeployCommand(context.Background(), []string{"-visibility", vis, "blog", dir}, cliEnv(token, front.URL), &bytes.Buffer{})
	}
	visibility := func() string {
		t.Helper()
		site, err := s.getSite(context.Background(), "blog")
		if err != nil {
			t.Fatal(err)
		}
		return site.Visibility
	}

	if err := deploy(visAuthenticated); err != nil {
		t.Fatal(err)
	}
	// Narrowing: the PUT comes first.
	if err := deploy(visRestricted); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(log, []string{"put", "upload"}) || visibility() != visRestricted {
		t.Fatalf("narrowing: requests %v, visibility %q", log, visibility())
	}
	// Widening with a failed upload: the visibility stays as it was.
	failUpload = true
	if err := deploy(visPublic); err == nil {
		t.Fatal("deploy succeeded although the upload failed")
	}
	if !slices.Equal(log, []string{"upload"}) || visibility() != visRestricted {
		t.Fatalf("failed widening: requests %v, visibility %q", log, visibility())
	}
	// Widening: the PUT comes after the upload.
	failUpload = false
	if err := deploy(visPublic); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(log, []string{"upload", "put"}) || visibility() != visPublic {
		t.Fatalf("widening: requests %v, visibility %q", log, visibility())
	}
	// Unchanged: no PUT at all.
	if err := deploy(visPublic); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(log, []string{"upload"}) {
		t.Fatalf("unchanged: requests %v", log)
	}
}

func TestDeployCommandFailures(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	token := apiToken(t, ts, c)
	other := secondUser(t, ts)
	createSiteVia(t, ts, other, "theirs")
	dir := writeSiteTree(t, map[string]string{"index.html": "hi"})

	cases := map[string]struct {
		args  []string
		token string
		want  string
	}{
		"no token":        {[]string{"blog", dir}, "", "OPEN_PAGES_TOKEN"},
		"bad token":       {[]string{"blog", dir}, tokenPrefix + strings.Repeat("0", 64), "HTTP 401"},
		"foreign site":    {[]string{"theirs", dir}, token, "HTTP 403"},
		"bad site name":   {[]string{"Not_A_Label", dir}, token, "invalid site name"},
		"reserved name":   {[]string{"admin", dir}, token, "reserved"},
		"missing dir":     {[]string{"blog", filepath.Join(dir, "nope")}, token, "nope"},
		"no create":       {[]string{"-create=false", "blog", dir}, token, "HTTP 404"},
		"wrong arg count": {[]string{"blog"}, token, "usage"},
		"bad visibility":  {[]string{"-visibility", "secret", "blog", dir}, token, "visibility must be"},
		"no create, vis":  {[]string{"-create=false", "-visibility", "restricted", "blog", dir}, token, "HTTP 404"},
		"foreign vis":     {[]string{"-visibility", "public", "theirs", dir}, token, "HTTP 403"},
		"bad server":      {[]string{"-server", "ftp://x", "blog", dir}, token, "server URL"},
		"unreachable":     {[]string{"-server", "http://127.0.0.1:1", "blog", dir}, token, "request failed"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := cli.DeployCommand(context.Background(), tc.args, cliEnv(tc.token, ts.URL), &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got error %v, want one containing %q", err, tc.want)
			}
		})
	}
	if _, err := os.Stat(s.SiteDir("blog")); err == nil {
		t.Fatal("a failed deploy left a site behind")
	}
}

func writeSiteTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}
