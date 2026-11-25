package server

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestManagePage(t *testing.T) {
	ts, s := newTestServer(t)
	resp, err := http.Get(ts.URL + "/manage")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp.StatusCode, http.StatusOK)
	b, _ := io.ReadAll(resp.Body)
	body := string(b)
	for _, want := range []string{
		`id="manage"`,
		`data-site-origin="http://{site}.` + s.cfg.ContentDomain, // placeholder survives escaping
		`id="create-form"`, `id="domains-section"`, `id="settings-form"`, `id="deploy-form"`, `id="versions-table"`, `id="delete-site"`,
		`href="/manage" class="pf-v6-c-nav__link pf-m-current"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if ct := resp.Header.Get("Content-Security-Policy"); !strings.Contains(ct, "default-src 'self'") {
		t.Errorf("CSP = %q", ct)
	}
	if strings.Contains(body, "<script>") {
		t.Error("page has an inline script, which the CSP blocks")
	}
}

// The bare app host serves the UI, not a 404.
func TestRootServesIndexPage(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp.StatusCode, http.StatusOK)
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `id="upload-form"`) {
		t.Error("root page is not the index page")
	}
	// Only the exact root: other unknown paths stay 404.
	other, err := http.Get(ts.URL + "/nope")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Body.Close()
	expectStatus(t, other.StatusCode, http.StatusNotFound)
}

// Every page has the signed-in user and sign-out controls (shown by app.js), and the manage
// page's dropdowns carry an arrow.
func TestPagesHaveUserMenuAndSelectArrows(t *testing.T) {
	ts, _ := newTestServer(t)
	get := func(path string) string {
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	for _, path := range []string{"/index", "/manage"} {
		body := get(path)
		for _, want := range []string{`id="user-menu"`, `id="user-email"`, `id="sign-out"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", path, want)
			}
		}
	}
	manage := get("/manage")
	if n, arrows := strings.Count(manage, "<select "), strings.Count(manage, "form-control__toggle-icon"); n == 0 || arrows != n {
		t.Errorf("manage page has %d selects and %d arrows", n, arrows)
	}
}
