package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fetch sends a GET, with an optional bearer token, and returns status, body and headers.
func fetch(t *testing.T, c *http.Client, url, bearer string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b), resp.Header
}

// accessWorld is a server with three users, a group holding the owner and the member, a
// restricted site "secret" in that group and a public site "open".
type accessWorld struct {
	ts                      *httptest.Server
	s                       *Server
	owner, member, outsider *http.Client
	anon                    *http.Client
	memberTok, outsiderTok  string
	group                   int64
	index                   string
}

func newAccessWorld(t *testing.T) *accessWorld {
	t.Helper()
	ts, s := newTestServer(t)
	w := &accessWorld{ts: ts, s: s, anon: &http.Client{}}
	w.owner = userClient(t, ts, "owner@example.com")
	w.member = userClient(t, ts, "member@example.com")
	w.outsider = userClient(t, ts, "outsider@example.com")
	w.memberTok, _ = mintToken(t, ts, w.member, `{"name":"ci"}`)["token"].(string)
	w.outsiderTok, _ = mintToken(t, ts, w.outsider, `{"name":"ci"}`)["token"].(string)
	if w.memberTok == "" || w.outsiderTok == "" {
		t.Fatal("no token minted")
	}

	w.group = newGroupVia(t, ts, w.owner, "eng")
	code, _ := doJSON(t, w.owner, http.MethodPost, groupURL(ts, w.group, "/members"), `{"email":"member@example.com"}`)
	expectStatus(t, code, http.StatusCreated)

	w.index = "<h1>secret</h1>"
	code, out := doJSON(t, w.owner, http.MethodPost, ts.URL+"/v1/auth/sites",
		fmt.Sprintf(`{"name":"secret","group":%d,"visibility":"restricted"}`, w.group))
	expectStatus(t, code, http.StatusCreated)
	if out["visibility"] != visRestricted {
		t.Fatalf("create response = %v", out)
	}
	createSiteVia(t, ts, w.owner, "open")
	for site, html := range map[string]string{"secret": w.index, "open": "<h1>open</h1>"} {
		arch := zipArchive(t, map[string]string{"index.html": html})
		expectStatus(t, post(t, w.owner, ts.URL+"/v1/auth/sites/"+site+"/upload", "application/zip", arch), http.StatusOK)
	}
	return w
}

func TestRestrictedSiteServingPathMode(t *testing.T) {
	w := newAccessWorld(t)
	url := w.ts.URL + "/secret/"

	_, missing, _ := fetch(t, w.anon, w.ts.URL+"/nonexistent/", "")
	cases := []struct {
		name   string
		client *http.Client
		bearer string
		status int
	}{
		{"owner session", w.owner, "", http.StatusOK},
		{"group member session", w.member, "", http.StatusOK},
		{"group member token", w.anon, w.memberTok, http.StatusOK},
		{"non-member session", w.outsider, "", http.StatusNotFound},
		{"non-member token", w.anon, w.outsiderTok, http.StatusNotFound},
		{"anonymous", w.anon, "", http.StatusNotFound},
		{"bogus token", w.anon, "opt_nope", http.StatusNotFound},
		// A bearer header never falls back to the cookie, even a valid one.
		{"bogus token with valid cookie", w.member, "opt_nope", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, body, h := fetch(t, tc.client, url, tc.bearer)
			if status != tc.status {
				t.Fatalf("status %d, want %d", status, tc.status)
			}
			if status == http.StatusOK {
				if strings.TrimSpace(body) != w.index {
					t.Errorf("body %q", body)
				}
				if cc := h.Get("Cache-Control"); !strings.Contains(cc, "private") {
					t.Errorf("Cache-Control %q should be private", cc)
				}
				return
			}
			// A refusal looks exactly like a site that does not exist.
			if body != missing {
				t.Errorf("refusal body %q differs from missing-site body %q", body, missing)
			}
		})
	}

	// Public sites stay open to everyone and are not marked private.
	status, body, h := fetch(t, w.anon, w.ts.URL+"/open/", "")
	if status != http.StatusOK || strings.TrimSpace(body) != "<h1>open</h1>" {
		t.Fatalf("public site: %d %q", status, body)
	}
	if strings.Contains(h.Get("Cache-Control"), "private") {
		t.Errorf("public site Cache-Control = %q", h.Get("Cache-Control"))
	}
	// A broken token on a public site is ignored.
	if status, _, _ := fetch(t, w.anon, w.ts.URL+"/open/", "opt_nope"); status != http.StatusOK {
		t.Errorf("public site with bad token: %d", status)
	}
}

func TestVisibilityChangesAccess(t *testing.T) {
	w := newAccessWorld(t)
	url := w.ts.URL + "/secret/"

	// Restricted without a group is private to the owner.
	code, _ := doJSON(t, w.owner, http.MethodPut, w.ts.URL+"/v1/auth/sites/secret", `{"group":0}`)
	expectStatus(t, code, http.StatusOK)
	if status, _, _ := fetch(t, w.member, url, ""); status != http.StatusNotFound {
		t.Errorf("member of no group on private site: %d", status)
	}
	if status, _, _ := fetch(t, w.owner, url, ""); status != http.StatusOK {
		t.Errorf("owner on private site: %d", status)
	}

	// Public again: everyone, anonymous included.
	code, out := doJSON(t, w.owner, http.MethodPut, w.ts.URL+"/v1/auth/sites/secret", `{"visibility":"public"}`)
	expectStatus(t, code, http.StatusOK)
	if out["visibility"] != visPublic {
		t.Fatalf("update response = %v", out)
	}
	if status, _, _ := fetch(t, w.anon, url, ""); status != http.StatusOK {
		t.Errorf("anonymous on public site: %d", status)
	}

	// Unknown visibilities are rejected on create and update, and change nothing.
	code, _ = doJSON(t, w.owner, http.MethodPut, w.ts.URL+"/v1/auth/sites/secret", `{"visibility":"friends"}`)
	expectStatus(t, code, http.StatusBadRequest)
	code, _ = doJSON(t, w.owner, http.MethodPost, w.ts.URL+"/v1/auth/sites", `{"name":"x","visibility":"friends"}`)
	expectStatus(t, code, http.StatusBadRequest)
	// Only the owner may change it.
	code, _ = doJSON(t, w.member, http.MethodPut, w.ts.URL+"/v1/auth/sites/secret", `{"visibility":"restricted"}`)
	expectStatus(t, code, http.StatusForbidden)
}

func TestSiteGroupMustIncludeOwner(t *testing.T) {
	w := newAccessWorld(t)
	sites := w.ts.URL + "/v1/auth/sites"

	// The outsider is not in "eng": creating or moving a site into it is refused (403),
	// and an unknown group stays 400.
	code, _ := doJSON(t, w.outsider, http.MethodPost, sites, fmt.Sprintf(`{"name":"mine","group":%d}`, w.group))
	expectStatus(t, code, http.StatusForbidden)
	if _, err := w.s.getSite(t.Context(), "mine"); err == nil {
		t.Fatal("site was created despite the refused group")
	}
	code, _ = doJSON(t, w.outsider, http.MethodPost, sites, `{"name":"mine","group":999}`)
	expectStatus(t, code, http.StatusBadRequest)

	createSiteVia(t, w.ts, w.outsider, "mine")
	code, _ = doJSON(t, w.outsider, http.MethodPut, sites+"/mine", fmt.Sprintf(`{"group":%d}`, w.group))
	expectStatus(t, code, http.StatusForbidden)
	if site, _ := w.s.getSite(t.Context(), "mine"); site.GroupID != 0 {
		t.Fatalf("site moved into a foreign group: %+v", site)
	}

	// Members may use the group, and may leave a site's group in place only while they stay members.
	code, _ = doJSON(t, w.member, http.MethodPost, sites, fmt.Sprintf(`{"name":"team","group":%d,"visibility":"restricted"}`, w.group))
	expectStatus(t, code, http.StatusCreated)
	arch := zipArchive(t, map[string]string{"index.html": "team"})
	expectStatus(t, post(t, w.member, sites+"/team/upload", "application/zip", arch), http.StatusOK)
	if status, _, _ := fetch(t, w.owner, w.ts.URL+"/team/", ""); status != http.StatusOK {
		t.Fatalf("group owner on member's site: %d", status)
	}

	// The site owner leaves the group: the group stops counting for the site, so the other
	// members lose access while the owner keeps it.
	var memberID int64
	if err := w.s.db.QueryRowContext(t.Context(), "SELECT userid FROM user WHERE email = 'member@example.com'").Scan(&memberID); err != nil {
		t.Fatal(err)
	}
	code, _ = doJSON(t, w.member, http.MethodDelete, groupURL(w.ts, w.group, fmt.Sprintf("/members/%d", memberID)), "")
	expectStatus(t, code, http.StatusOK)
	if status, _, _ := fetch(t, w.owner, w.ts.URL+"/team/", ""); status != http.StatusNotFound {
		t.Errorf("group owner after site owner left the group: %d, want 404", status)
	}
	if status, _, _ := fetch(t, w.member, w.ts.URL+"/team/", ""); status != http.StatusOK {
		t.Errorf("site owner after leaving the group: %d, want 200", status)
	}
}

func TestSiteMetadataAccess(t *testing.T) {
	w := newAccessWorld(t)
	meta := w.ts.URL + "/v1/auth/sites/secret"

	for _, tc := range []struct {
		name   string
		client *http.Client
		bearer string
		status int
	}{
		{"owner", w.owner, "", http.StatusOK},
		{"member", w.member, "", http.StatusOK},
		{"member token", w.anon, w.memberTok, http.StatusOK},
		{"non-member", w.outsider, "", http.StatusForbidden},
		{"non-member token", w.anon, w.outsiderTok, http.StatusForbidden},
		{"anonymous", w.anon, "", http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if status, _, _ := fetch(t, tc.client, meta, tc.bearer); status != tc.status {
				t.Fatalf("status %d, want %d", status, tc.status)
			}
		})
	}
	code, out := doJSON(t, w.member, http.MethodGet, meta, "")
	expectStatus(t, code, http.StatusOK)
	if out["visibility"] != visRestricted || out["group"] != float64(w.group) || out["current"] == "" {
		t.Fatalf("metadata = %v", out)
	}
	code, _ = doJSON(t, w.member, http.MethodGet, w.ts.URL+"/v1/auth/sites/nope", "")
	expectStatus(t, code, http.StatusNotFound)
	// Public sites are visible to every logged-in user.
	code, _ = doJSON(t, w.outsider, http.MethodGet, w.ts.URL+"/v1/auth/sites/open", "")
	expectStatus(t, code, http.StatusOK)

	// Version listing stays owner-only, group members included.
	for _, c := range []*http.Client{w.member, w.outsider} {
		code, _ = doJSON(t, c, http.MethodGet, meta+"/versions", "")
		expectStatus(t, code, http.StatusForbidden)
	}
	code, _ = doJSON(t, w.owner, http.MethodGet, meta+"/versions", "")
	expectStatus(t, code, http.StatusOK)
}

func TestPublicSiteListing(t *testing.T) {
	w := newAccessWorld(t)
	list := func(c *http.Client, bearer string) []string {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, w.ts.URL+"/v1/sites", nil)
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		expectStatus(t, resp.StatusCode, http.StatusOK)
		var body struct {
			Sites []publicSite `json:"sites"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		names := []string{}
		for _, s := range body.Sites {
			names = append(names, s.Name)
		}
		return names
	}

	// Same answer for everyone: no login needed, and restricted sites never show up.
	for name, got := range map[string][]string{
		"anonymous": list(w.anon, ""),
		"owner":     list(w.owner, ""),
		"member":    list(w.member, ""),
		"token":     list(w.anon, w.memberTok),
	} {
		if strings.Join(got, ",") != "open" {
			t.Errorf("%s sees %v, want [open]", name, got)
		}
	}

	code, _ := doJSON(t, w.owner, http.MethodPut, w.ts.URL+"/v1/auth/sites/open", `{"description":"front page","visibility":"restricted"}`)
	expectStatus(t, code, http.StatusOK)
	code, _ = doJSON(t, w.owner, http.MethodPut, w.ts.URL+"/v1/auth/sites/secret", `{"description":"now open","visibility":"public"}`)
	expectStatus(t, code, http.StatusOK)
	if got := list(w.anon, ""); strings.Join(got, ",") != "secret" {
		t.Errorf("after flipping visibility: %v, want [secret]", got)
	}
	_, body, _ := fetch(t, w.anon, w.ts.URL+"/v1/sites", "")
	if !strings.Contains(body, `"description":"now open"`) || strings.Contains(body, "owner") {
		t.Errorf("listing body = %s", body)
	}
}

func TestRestrictedSiteSubdomainMode(t *testing.T) {
	s := newSubdomainServer(t)
	ctx := t.Context()
	ids := map[string]int64{}
	for _, email := range []string{"owner@example.com", "member@example.com", "outsider@example.com"} {
		res, err := s.db.ExecContext(ctx, "INSERT INTO user (email, password) VALUES (?, 'x')", email)
		if err != nil {
			t.Fatal(err)
		}
		ids[email], _ = res.LastInsertId()
	}
	g, err := s.createGroup(ctx, "eng", ids["owner@example.com"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO user_group (uid, gid) VALUES (?, ?)", ids["member@example.com"], g.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createSite(ctx, "secret", "", ids["owner@example.com"], g.ID, visRestricted); err != nil {
		t.Fatal(err)
	}
	f := writeZip(t, map[string]string{"index.html": "<h1>secret</h1>"})
	if _, err := deployNamed(t, s, "secret", f); err != nil {
		t.Fatal(err)
	}

	// Credentials: a session cookie or an API token per user.
	for email, id := range ids {
		tok := "tok-" + email
		exp := time.Now().Add(time.Hour).Unix()
		if _, err := s.db.ExecContext(ctx, "INSERT INTO session (userid, token, expires) VALUES (?, ?, ?)", id, tok, exp); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.ExecContext(ctx,
			"INSERT INTO api_token (userid, name, prefix, hash, created) VALUES (?, 'ci', 'opt_', ?, ?)",
			id, hashToken("opt_"+email), time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		who, header, value string
		status             int
	}{
		{"owner cookie", "Cookie", sessionCookie + "=tok-owner@example.com", 200},
		{"member cookie", "Cookie", sessionCookie + "=tok-member@example.com", 200},
		{"member token", "Authorization", "Bearer opt_member@example.com", 200},
		{"outsider cookie", "Cookie", sessionCookie + "=tok-outsider@example.com", 404},
		{"outsider token", "Authorization", "Bearer opt_outsider@example.com", 404},
		{"anonymous", "X-Anything", "x", 404},
	} {
		t.Run(tc.who, func(t *testing.T) {
			if w := do(s, "GET", "secret.pages.corp", "/", tc.header, tc.value); w.Code != tc.status {
				t.Fatalf("status %d, want %d", w.Code, tc.status)
			}
		})
	}
	// HEAD is checked the same way.
	if w := do(s, "HEAD", "secret.pages.corp", "/"); w.Code != 404 {
		t.Errorf("anonymous HEAD: %d", w.Code)
	}
}

// A database from before visibility existed gains the column, and its sites stay public.
func TestMigrateAddsVisibility(t *testing.T) {
	path := t.TempDir() + "/old.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE docs (docid INTEGER PRIMARY KEY, uowner INTEGER NULL, ugroup INTEGER NULL, name VARCHAR(256) NOT NULL UNIQUE, description VARCHAR(512) NULL, path VARCHAR(256) NULL UNIQUE)",
		"INSERT INTO docs (name) VALUES ('old')",
	} {
		if _, err := db.ExecContext(t.Context(), q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	for range 2 {
		db, err = openDB(path)
		if err != nil {
			t.Fatal(err)
		}
		var vis string
		if err := db.QueryRowContext(t.Context(), "SELECT visibility FROM docs WHERE name = 'old'").Scan(&vis); err != nil || vis != visPublic {
			t.Fatalf("visibility = %q, %v", vis, err)
		}
		db.Close()
	}
}
