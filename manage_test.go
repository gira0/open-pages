package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// doJSON sends a request with an optional JSON body and returns the status and decoded JSON.
func doJSON(t *testing.T, c *http.Client, method, url, body string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// secondUser registers and logs in a different user.
func secondUser(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	c := newClient(t)
	creds := []byte(`{"email":"b@example.com","password":"correct horse"}`)
	expectStatus(t, post(t, c, ts.URL+"/v1/user/register", "application/json", creds), http.StatusCreated)
	expectStatus(t, post(t, c, ts.URL+"/v1/user/login", "application/json", creds), http.StatusOK)
	return c
}

func uploadVia(t *testing.T, ts *httptest.Server, c *http.Client, site, body string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/auth/sites/"+site+"/upload",
		bytes.NewReader(zipArchive(t, map[string]string{"index.html": body})))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp.StatusCode, http.StatusOK)
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out["version"]
}

func TestRedeployRollbackAndVersions(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	createSiteVia(t, ts, c, "blog")
	base := ts.URL + "/v1/auth/sites/blog"

	v1 := uploadVia(t, ts, c, "blog", "one")
	v2 := uploadVia(t, ts, c, "blog", "two") // redeploy
	if v1 == v2 {
		t.Fatal("redeploy reused a version id")
	}
	read := func() string {
		b, _ := os.ReadFile(filepath.Join(s.CurrentDir("blog"), "index.html"))
		return string(b)
	}
	if read() != "two" {
		t.Fatalf("redeploy not live: %q", read())
	}

	code, out := doJSON(t, c, http.MethodGet, base+"/versions", "")
	versions, _ := out["versions"].([]any)
	if code != http.StatusOK || out["current"] != v2 || len(versions) != 2 || versions[0] != v2 || versions[1] != v1 {
		t.Fatalf("versions = %d %v", code, out)
	}

	code, _ = doJSON(t, c, http.MethodPost, base+"/rollback", `{"version":"`+v1+`"}`)
	if code != http.StatusOK || read() != "one" {
		t.Fatalf("rollback: status %d, current %q", code, read())
	}
	// Rolling back to the version that is already live is a no-op success.
	code, _ = doJSON(t, c, http.MethodPost, base+"/rollback", `{"version":"`+v1+`"}`)
	expectStatus(t, code, http.StatusOK)

	for body, want := range map[string]int{
		`{"version":"00000000000000000000"}`: http.StatusNotFound,
		`{"version":"../../etc"}`:            http.StatusNotFound,
		`{"version":""}`:                     http.StatusBadRequest,
		`not json`:                           http.StatusBadRequest,
	} {
		code, _ := doJSON(t, c, http.MethodPost, base+"/rollback", body)
		if code != want {
			t.Errorf("rollback %s: status %d, want %d", body, code, want)
		}
	}
	if read() != "one" {
		t.Fatalf("failed rollbacks changed current: %q", read())
	}
}

func TestDeleteSite(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	createSiteVia(t, ts, c, "wiki")
	uploadVia(t, ts, c, "wiki", "one")
	base := ts.URL + "/v1/auth/sites/wiki"

	code, _ := doJSON(t, c, http.MethodDelete, base, "")
	expectStatus(t, code, http.StatusOK)
	if _, err := os.Stat(s.SiteDir("wiki")); !os.IsNotExist(err) {
		t.Fatalf("site files remain: %v", err)
	}
	if _, err := s.getSite(t.Context(), "wiki"); !errors.Is(err, errSiteNotFound) {
		t.Fatalf("db row remains: %v", err)
	}
	code, _ = doJSON(t, c, http.MethodDelete, base, "")
	expectStatus(t, code, http.StatusNotFound)

	// The name can be reused and starts empty.
	createSiteVia(t, ts, c, "wiki")
	if v, _ := s.Versions("wiki"); len(v) != 0 {
		t.Fatalf("recreated site has versions %v", v)
	}
}

func TestOwnerOnlyMutations(t *testing.T) {
	ts, s := newTestServer(t)
	owner := loggedInClient(t, ts)
	createSiteVia(t, ts, owner, "blog")
	v1 := uploadVia(t, ts, owner, "blog", "one")
	uploadVia(t, ts, owner, "blog", "two")
	other := secondUser(t, ts)
	anon := newClient(t)
	base := ts.URL + "/v1/auth/sites/blog"

	cases := []struct{ method, path, body string }{
		{http.MethodPut, "", `{"description":"hijacked"}`},
		{http.MethodDelete, "", ""},
		{http.MethodGet, "/versions", ""},
		{http.MethodPost, "/rollback", `{"version":"` + v1 + `"}`},
	}
	for _, tc := range cases {
		if code, _ := doJSON(t, other, tc.method, base+tc.path, tc.body); code != http.StatusForbidden {
			t.Errorf("non-owner %s %s: status %d, want 403", tc.method, tc.path, code)
		}
		if code, _ := doJSON(t, anon, tc.method, base+tc.path, tc.body); code != http.StatusUnauthorized {
			t.Errorf("anonymous %s %s: status %d, want 401", tc.method, tc.path, code)
		}
		missing := ts.URL + "/v1/auth/sites/nope" + tc.path
		if code, _ := doJSON(t, other, tc.method, missing, tc.body); code != http.StatusNotFound {
			t.Errorf("unknown site %s %s: status %d, want 404", tc.method, tc.path, code)
		}
	}

	site, err := s.getSite(t.Context(), "blog")
	if err != nil || site.Description != "" {
		t.Fatalf("site after rejected requests: %+v, %v", site, err)
	}
	if cur, _ := s.CurrentVersion("blog"); cur == v1 {
		t.Fatal("non-owner rolled the site back")
	}
}

func TestSiteGroup(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	if _, err := s.db.Exec("INSERT INTO groups (groupid, name) VALUES (7, 'eng')"); err != nil {
		t.Fatal(err)
	}
	sites := ts.URL + "/v1/auth/sites"

	// Set on create; unknown groups are rejected.
	code, _ := doJSON(t, c, http.MethodPost, sites, `{"name":"blog","group":7}`)
	expectStatus(t, code, http.StatusCreated)
	code, _ = doJSON(t, c, http.MethodPost, sites, `{"name":"wiki","group":99}`)
	expectStatus(t, code, http.StatusBadRequest)
	if _, err := s.getSite(t.Context(), "wiki"); !errors.Is(err, errSiteNotFound) {
		t.Fatalf("site with bad group was created: %v", err)
	}
	if site, _ := s.getSite(t.Context(), "blog"); site.GroupID != 7 {
		t.Fatalf("GroupID = %d, want 7", site.GroupID)
	}

	// Update: absent fields stay, 0 clears the group, unknown groups are rejected.
	code, _ = doJSON(t, c, http.MethodPut, sites+"/blog", `{"description":"Team blog"}`)
	expectStatus(t, code, http.StatusOK)
	if site, _ := s.getSite(t.Context(), "blog"); site.Description != "Team blog" || site.GroupID != 7 {
		t.Fatalf("after description update: %+v", site)
	}
	code, _ = doJSON(t, c, http.MethodPut, sites+"/blog", `{"group":99}`)
	expectStatus(t, code, http.StatusBadRequest)
	code, _ = doJSON(t, c, http.MethodPut, sites+"/blog", `{"group":0}`)
	expectStatus(t, code, http.StatusOK)
	if site, _ := s.getSite(t.Context(), "blog"); site.GroupID != 0 || site.Description != "Team blog" {
		t.Fatalf("after clearing group: %+v", site)
	}
	code, _ = doJSON(t, c, http.MethodPut, sites+"/blog", `not json`)
	expectStatus(t, code, http.StatusBadRequest)
}
