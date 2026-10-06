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

func post(t *testing.T, c *http.Client, url, ctype string, body []byte) *http.Response {
	t.Helper()
	resp, err := c.Post(url, ctype, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func expectStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Fatalf("%s %s: got status %d, want %d", resp.Request.Method, resp.Request.URL.Path, resp.StatusCode, want)
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

func tarGzArchive(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// siteDirs lists the extracted uploads, ignoring staging leftovers.
func siteDirs(t *testing.T, s *Server) []string {
	entries, err := os.ReadDir(s.sites)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestAuthRequired(t *testing.T) {
	ts, s := newTestServer(t)
	c := newClient(t)

	resp, err := c.Get(ts.URL + "/v1/auth/user")
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp, http.StatusUnauthorized)

	expectStatus(t, post(t, c, ts.URL+"/v1/auth/docs/upload", "application/zip",
		zipArchive(t, map[string]string{"index.html": "hi"})), http.StatusUnauthorized)
	if dirs := siteDirs(t, s); len(dirs) != 0 {
		t.Fatalf("unauthenticated upload wrote %v", dirs)
	}

	c.Jar.SetCookies(resp.Request.URL, []*http.Cookie{{Name: sessionCookie, Value: "bogus"}})
	resp, err = c.Get(ts.URL + "/v1/auth/user")
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp, http.StatusUnauthorized)
}

func TestRegisterLoginLogout(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts)

	resp, err := c.Get(ts.URL + "/v1/auth/user")
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp, http.StatusOK)

	creds := []byte(`{"email":"a@example.com","password":"correct horse"}`)
	expectStatus(t, post(t, c, ts.URL+"/v1/user/register", "application/json", creds), http.StatusConflict)

	other := newClient(t)
	form := []byte("email=a%40example.com&password=wrong+password")
	expectStatus(t, post(t, other, ts.URL+"/v1/user/login", "application/x-www-form-urlencoded", form), http.StatusUnauthorized)
	form = []byte("email=a%40example.com&password=correct+horse")
	expectStatus(t, post(t, other, ts.URL+"/v1/user/login", "application/x-www-form-urlencoded", form), http.StatusOK)

	expectStatus(t, post(t, c, ts.URL+"/v1/auth/logout", "", nil), http.StatusOK)
	resp, err = c.Get(ts.URL + "/v1/auth/user")
	if err != nil {
		t.Fatal(err)
	}
	expectStatus(t, resp, http.StatusUnauthorized)
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
	files := map[string]string{"index.html": "<h1>hi</h1>", "css/site.css": "body{}"}

	expectStatus(t, post(t, c, ts.URL+"/v1/auth/docs/upload", "application/zip", zipArchive(t, files)), http.StatusOK)
	expectStatus(t, post(t, c, ts.URL+"/v1/auth/docs/upload", "application/gzip", tarGzArchive(t, files)), http.StatusOK)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("name", "site")
	fw, _ := mw.CreateFormFile("file", "site.zip")
	fw.Write(zipArchive(t, files))
	mw.Close()
	expectStatus(t, post(t, c, ts.URL+"/v1/auth/docs/formupload", mw.FormDataContentType(), body.Bytes()), http.StatusOK)

	dirs := siteDirs(t, s)
	if len(dirs) != 3 {
		t.Fatalf("got site dirs %v, want 3", dirs)
	}
	for _, d := range dirs {
		got, err := os.ReadFile(filepath.Join(s.sites, d, "css", "site.css"))
		if err != nil || string(got) != "body{}" {
			t.Fatalf("%s: css/site.css = %q, %v", d, got, err)
		}
	}
}

func TestUploadRejectsBadArchives(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	url := ts.URL + "/v1/auth/docs/upload"

	cases := map[string]struct {
		body []byte
		want int
	}{
		"zip traversal":   {zipArchive(t, map[string]string{"../escape.txt": "x"}), http.StatusBadRequest},
		"tar traversal":   {tarGzArchive(t, map[string]string{"../../escape.txt": "x"}), http.StatusBadRequest},
		"absolute path":   {tarGzArchive(t, map[string]string{"/tmp/escape.txt": "x"}), http.StatusBadRequest},
		"not an archive":  {[]byte("hello world"), http.StatusBadRequest},
		"too large":       {bytes.Repeat([]byte("x"), 2<<20), http.StatusRequestEntityTooLarge},
		"expands too far": {zipArchive(t, map[string]string{"big.txt": strings.Repeat("x", 2<<20)}), http.StatusBadRequest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			expectStatus(t, post(t, c, url, "application/octet-stream", tc.body), tc.want)
		})
	}

	if dirs := siteDirs(t, s); len(dirs) != 0 {
		t.Fatalf("rejected uploads left %v behind", dirs)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.sites), "escape.txt")); err == nil {
		t.Fatal("archive wrote outside the data dir")
	}
}

func TestDocCreate(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts)
	url := ts.URL + "/v1/auth/docs/create"

	expectStatus(t, post(t, c, url, "application/json", []byte(`{"name":"handbook","description":"Team handbook"}`)), http.StatusCreated)
	expectStatus(t, post(t, c, url, "application/json", []byte(`{"name":"handbook"}`)), http.StatusConflict)
	expectStatus(t, post(t, c, url, "application/json", []byte(`{"name":"../bad"}`)), http.StatusBadRequest)
}

func TestCORS(t *testing.T) {
	ts, s := newTestServer(t)
	s.cfg.CORSOrigins = []string{"https://allowed.example"}

	for origin, want := range map[string]string{
		"https://allowed.example": "https://allowed.example",
		"https://evil.example":    "",
	} {
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
	}
}
