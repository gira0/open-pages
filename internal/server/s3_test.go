package server

import (
	"net/http/httptest"
	"testing"

	"github.com/gira0/open-pages/internal/config"
	"github.com/johannesboyne/gofakes3"
	"github.com/johannesboyne/gofakes3/backend/s3mem"
)

// newS3TestServer builds a server whose sites live in an in-process fake S3 bucket.
func newS3TestServer(t *testing.T) *Server {
	t.Helper()
	backend := s3mem.New()
	if err := backend.CreateBucket("pages"); err != nil {
		t.Fatal(err)
	}
	fake := httptest.NewServer(gofakes3.New(backend).Server())
	t.Cleanup(fake.Close)
	return newServeTestServer(t, func(c *config.Config) {
		c.Storage.Type = config.StorageS3
		c.Storage.S3 = config.S3Config{
			Endpoint: fake.URL, Region: "us-east-1", Bucket: "pages", Prefix: "op",
			PathStyle: true, AccessKey: "test", SecretKey: "test-secret",
		}
	})
}

func TestS3SiteServing(t *testing.T) {
	s := newS3TestServer(t)
	deploySite(t, s, "blog", blogFiles)

	missing := "<h1>custom missing</h1>"
	runServeCases(t, s, siteHost("blog"), []serveCase{
		{"site root", "/", 200, "<h1>blog</h1>", "text/html", ""},
		{"css", "/app.css", 200, "body{}", "text/css", ""},
		{"nested file", "/nested/deep/x.txt", 200, "x", "text/plain", ""},
		{"directory index", "/docs/", 200, "<h1>docs</h1>", "text/html", ""},
		{"directory redirects", "/docs", 301, "", "", "./docs/"},
		{"no listing", "/nested/", 404, missing, "text/html", ""},
		{"no listing without index", "/empty/", 404, missing, "text/html", ""},
		{"custom 404", "/nope.html", 404, missing, "text/html", ""},
		{"file with slash", "/app.css/", 404, missing, "text/html", ""},
	})

	// Range requests and conditional requests work through the seekable file.
	w := do(s, "GET", siteHost("blog"), "/nested/deep/x.txt", "Range", "bytes=0-0")
	if w.Code != 206 || w.Body.String() != "x" {
		t.Errorf("range: %d %q", w.Code, w.Body.String())
	}
	etag := do(s, "GET", siteHost("blog"), "/app.css").Header().Get("ETag")
	if etag == "" {
		t.Fatal("no ETag")
	}
	if w := do(s, "GET", siteHost("blog"), "/app.css", "If-None-Match", etag); w.Code != 304 {
		t.Errorf("conditional: %d, want 304", w.Code)
	}
}

func TestS3DeployRollbackDelete(t *testing.T) {
	s := newS3TestServer(t)
	s.cfg.KeepVersions = 2
	deploySite(t, s, "blog", map[string]string{"index.html": "one"})
	site := mustSite(t, s, "blog")
	v1, _ := s.CurrentVersion("blog")
	for _, body := range []string{"two", "three"} {
		if _, err := s.deploy(t.Context(), site, writeZip(t, map[string]string{"index.html": body})); err != nil {
			t.Fatal(err)
		}
	}
	if vs, _ := s.Versions("blog"); len(vs) != 2 || vs[0] == v1 {
		t.Fatalf("versions = %v, want the oldest pruned", vs)
	}
	if w := do(s, "GET", siteHost("blog"), "/"); w.Body.String() != "three" {
		t.Fatalf("serving %q", w.Body.String())
	}
	vs, _ := s.Versions("blog")
	if err := s.rollbackSite(t.Context(), site, vs[0]); err != nil {
		t.Fatal(err)
	}
	if w := do(s, "GET", siteHost("blog"), "/"); w.Body.String() != "two" {
		t.Fatalf("after rollback serving %q", w.Body.String())
	}
	if err := s.deleteSite(t.Context(), site); err != nil {
		t.Fatal(err)
	}
	if w := do(s, "GET", siteHost("blog"), "/"); w.Code == 200 || w.Body.String() == "two" {
		t.Fatalf("deleted site still served: %d %q", w.Code, w.Body.String())
	}
	if vs, _ := s.Versions("blog"); len(vs) != 0 {
		t.Fatalf("versions after delete: %v", vs)
	}
}

func TestS3Readiness(t *testing.T) {
	s := newS3TestServer(t)
	if w := do(s, "GET", "", "/readyz"); w.Code != 200 {
		t.Fatalf("/readyz = %d: %s", w.Code, w.Body)
	}
}
