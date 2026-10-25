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
		`id="create-form"`, `id="settings-form"`, `id="deploy-form"`, `id="versions-table"`, `id="delete-site"`,
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
