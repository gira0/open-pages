package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) (*httptest.Server, *Server) {
	t.Helper()
	cfg := defaultConfig()
	cfg.DataPath = t.TempDir()
	cfg.TmpPath = t.TempDir()
	cfg.MaxUploadBytes = 1 << 20
	cfg.MaxExtractSize = 1 << 20
	cfg.MaxExtractFile = 50
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(func() { ts.Close(); s.db.Close() })
	return ts, s
}

func newClient(t *testing.T) *http.Client {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar}
}

// post sends a request and returns its status code.
func post(t *testing.T, c *http.Client, url, ctype string, body []byte) int {
	t.Helper()
	resp, err := c.Post(url, ctype, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// get sends a request and returns its status code.
func get(t *testing.T, c *http.Client, url string) int {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func expectStatus(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("got status %d, want %d", got, want)
	}
}

// loggedInClient registers a user and returns a client holding its session cookie.
func loggedInClient(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	c := newClient(t)
	creds := []byte(`{"email":"a@example.com","password":"correct horse"}`)
	expectStatus(t, post(t, c, ts.URL+"/v1/user/register", "application/json", creds), http.StatusCreated)
	expectStatus(t, post(t, c, ts.URL+"/v1/user/login", "application/json", creds), http.StatusOK)
	return c
}

// createSiteVia creates a site through the API.
func createSiteVia(t *testing.T, ts *httptest.Server, c *http.Client, name string) {
	t.Helper()
	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites", "application/json", []byte(`{"name":"`+name+`"}`)), http.StatusCreated)
}

func zipArchive(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	return buf.Bytes()
}

func tarGzArchive(files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(name, "/") {
			hdr.Typeflag, hdr.Mode = tar.TypeDir, 0o755
		}
		tw.WriteHeader(hdr)
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func TestAuthRequired(t *testing.T) {
	ts, s := newTestServer(t)
	c := newClient(t)

	expectStatus(t, get(t, c, ts.URL+"/v1/auth/user"), http.StatusUnauthorized)

	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites/blog/upload", "application/zip",
		zipArchive(t, map[string]string{"index.html": "hi"})), http.StatusUnauthorized)
	if entries, _ := os.ReadDir(s.sites); len(entries) != 0 {
		t.Fatalf("unauthenticated upload wrote %v", entries)
	}

	u, _ := url.Parse(ts.URL)
	c.Jar.SetCookies(u, []*http.Cookie{{Name: sessionCookie, Value: "bogus"}})
	expectStatus(t, get(t, c, ts.URL+"/v1/auth/user"), http.StatusUnauthorized)
}

func TestRegisterLoginLogout(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts)

	expectStatus(t, get(t, c, ts.URL+"/v1/auth/user"), http.StatusOK)

	creds := []byte(`{"email":"a@example.com","password":"correct horse"}`)
	expectStatus(t, post(t, c, ts.URL+"/v1/user/register", "application/json", creds), http.StatusConflict)

	other := newClient(t)
	form := []byte("email=a%40example.com&password=wrong+password")
	expectStatus(t, post(t, other, ts.URL+"/v1/user/login", "application/x-www-form-urlencoded", form), http.StatusUnauthorized)
	form = []byte("email=a%40example.com&password=correct+horse")
	expectStatus(t, post(t, other, ts.URL+"/v1/user/login", "application/x-www-form-urlencoded", form), http.StatusOK)

	expectStatus(t, post(t, c, ts.URL+"/v1/auth/logout", "", nil), http.StatusOK)
	expectStatus(t, get(t, c, ts.URL+"/v1/auth/user"), http.StatusUnauthorized)
}

func TestRegisterValidation(t *testing.T) {
	ts, _ := newTestServer(t)
	c := newClient(t)
	for _, body := range []string{
		`{"email":"not-an-email","password":"long enough"}`,
		`{"email":"a@example.com","password":"short"}`,
		`not json`,
	} {
		expectStatus(t, post(t, c, ts.URL+"/v1/user/register", "application/json", []byte(body)), http.StatusBadRequest)
	}
}

func TestUploadFormats(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	createSiteVia(t, ts, c, "blog")
	files := map[string]string{"index.html": "<h1>hi</h1>", "css/": "", "css/site.css": "body{}"}

	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites/blog/upload", "application/zip", zipArchive(t, files)), http.StatusOK)
	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites/blog/upload", "application/gzip", tarGzArchive(files)), http.StatusOK)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "site.zip")
	fw.Write(zipArchive(t, files))
	mw.Close()
	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites/blog/formupload", mw.FormDataContentType(), body.Bytes()), http.StatusOK)

	versions, err := s.Versions("blog")
	if err != nil || len(versions) != 3 {
		t.Fatalf("got versions %v, %v; want 3", versions, err)
	}
	for _, v := range versions {
		got, err := os.ReadFile(filepath.Join(s.SiteDir("blog"), "versions", v, "css", "site.css"))
		if err != nil || string(got) != "body{}" {
			t.Fatalf("%s: css/site.css = %q, %v", v, got, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(s.CurrentDir("blog"), "index.html"))
	if err != nil || string(got) != "<h1>hi</h1>" {
		t.Fatalf("current/index.html = %q, %v", got, err)
	}
}

func TestUploadRejectsBadArchives(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	createSiteVia(t, ts, c, "blog")
	url := ts.URL + "/v1/auth/sites/blog/upload"

	cases := map[string]struct {
		body []byte
		want int
	}{
		"zip traversal":   {zipArchive(t, map[string]string{"../escape.txt": "x"}), http.StatusBadRequest},
		"tar traversal":   {tarGzArchive(map[string]string{"../../escape.txt": "x"}), http.StatusBadRequest},
		"absolute path":   {tarGzArchive(map[string]string{"/tmp/escape.txt": "x"}), http.StatusBadRequest},
		"not an archive":  {[]byte("hello world"), http.StatusBadRequest},
		"too large":       {bytes.Repeat([]byte("x"), 2<<20), http.StatusRequestEntityTooLarge},
		"expands too far": {zipArchive(t, map[string]string{"big.txt": strings.Repeat("x", 2<<20)}), http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			expectStatus(t, post(t, c, url, "application/octet-stream", tc.body), tc.want)
		})
	}

	if versions, _ := s.Versions("blog"); len(versions) != 0 {
		t.Fatalf("rejected uploads left %v behind", versions)
	}
	if _, err := os.Lstat(s.CurrentDir("blog")); err == nil {
		t.Fatal("rejected uploads created a current link")
	}
	if left, _ := filepath.Glob(filepath.Join(s.SiteDir("blog"), ".staging-*")); len(left) != 0 {
		t.Fatalf("staging leftovers: %v", left)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.sites), "escape.txt")); err == nil {
		t.Fatal("archive wrote outside the data dir")
	}
}

func TestCORS(t *testing.T) {
	ts, s := newTestServer(t)
	s.cfg.CORSOrigins = []string{"https://allowed.example"}

	for origin, want := range map[string]string{
		"https://allowed.example": "https://allowed.example",
		"https://evil.example":    "",
	} {
		wantExpose := ""
		if want != "" {
			wantExpose = requestIDHeader
		}
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/ping", nil)
		req.Header.Set("Origin", origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != want {
			t.Errorf("origin %s: Allow-Origin = %q, want %q", origin, got, want)
		}
		if got := resp.Header.Get("Access-Control-Expose-Headers"); got != wantExpose {
			t.Errorf("origin %s: Expose-Headers = %q, want %q", origin, got, wantExpose)
		}
	}
}

func TestIndexPage(t *testing.T) {
	ts, _ := newTestServer(t)
	expectStatus(t, get(t, newClient(t), ts.URL+"/index"), http.StatusOK)
}
