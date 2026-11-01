package server

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gira0/open-pages/internal/cli"
)

func TestExpiryFromDays(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	if at, err := expiryFromDays(0, now); at != 0 || err != nil {
		t.Errorf("0 days = %d, %v", at, err)
	}
	if at, err := expiryFromDays(2, now); at != now.Add(48*time.Hour).Unix() || err != nil {
		t.Errorf("2 days = %d, %v", at, err)
	}
	for _, bad := range []int{-1, maxExpiryDays + 1} {
		if _, err := expiryFromDays(bad, now); err == nil {
			t.Errorf("%d days accepted", bad)
		}
	}
}

func TestReapExpired(t *testing.T) {
	_, s := newTestServer(t)
	owner := testOwner(t, s)
	mk := func(name string) Site {
		t.Helper()
		site, err := s.createSite(t.Context(), name, "", owner, 0, visPublic)
		if err != nil {
			t.Fatal(err)
		}
		deployNamedFiles(t, s, name)
		return site
	}
	now := time.Now()
	old, fresh, forever := mk("old"), mk("fresh"), mk("forever")
	if err := setExpiry(t, s, old, now.Add(-time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := setExpiry(t, s, fresh, now.Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	_ = forever

	n, err := s.reapExpired(t.Context(), now)
	if err != nil || n != 1 {
		t.Fatalf("reaped %d, %v; want 1", n, err)
	}
	if _, err := s.getSite(t.Context(), "old"); !errors.Is(err, errSiteNotFound) {
		t.Errorf("expired site still there: %v", err)
	}
	if _, err := os.Lstat(s.SiteDir("old")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expired site's files remain: %v", err)
	}
	for _, name := range []string{"fresh", "forever"} {
		if _, err := s.getSite(t.Context(), name); err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
	}

	// Renewing before the reaper runs keeps the site.
	if err := setExpiry(t, s, fresh, now.Add(-time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := setExpiry(t, s, fresh, now.Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.reapExpired(t.Context(), now); n != 0 {
		t.Errorf("reaped %d renewed sites", n)
	}
	// Clearing the expiry works too.
	if err := setExpiry(t, s, fresh, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.getSite(t.Context(), "fresh"); got.ExpiresAt != 0 {
		t.Errorf("expiry = %d, want cleared", got.ExpiresAt)
	}
	// A stale Site value cannot touch a site that was replaced.
	if err := s.deleteSite(t.Context(), fresh); err != nil {
		t.Fatal(err)
	}
	if err := setExpiry(t, s, fresh, 1); !errors.Is(err, errSiteNotFound) {
		t.Errorf("setExpiry on deleted site = %v", err)
	}
}

// setExpiry sets site's expiry (0 clears it) the way the API does.
func setExpiry(t *testing.T, s *Server, site Site, at int64) error {
	t.Helper()
	_, err := s.updateSiteExpiring(t.Context(), site, nil, nil, nil, &at)
	return err
}

// deployNamedFiles puts a small page into the named site.
func deployNamedFiles(t *testing.T, s *Server, name string) {
	t.Helper()
	if _, err := deployNamed(t, s, name, writeZip(t, map[string]string{"index.html": name})); err != nil {
		t.Fatal(err)
	}
}

func TestExpiryOverAPI(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites", "application/json",
		[]byte(`{"name":"tmp","expires_in_days":3}`)), http.StatusCreated)
	site, err := s.getSite(t.Context(), "tmp")
	if err != nil {
		t.Fatal(err)
	}
	if d := time.Until(time.Unix(site.ExpiresAt, 0)); d < 71*time.Hour || d > 73*time.Hour {
		t.Fatalf("expires in %v, want about 72h", d)
	}
	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites", "application/json",
		[]byte(`{"name":"bad","expires_in_days":9999}`)), http.StatusBadRequest)
	if _, err := s.getSite(t.Context(), "bad"); !errors.Is(err, errSiteNotFound) {
		t.Errorf("site created despite a bad expiry: %v", err)
	}

	put := func(body string) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPut, ts.URL+"/v1/auth/sites/tmp", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var b bytes.Buffer
		_, _ = b.ReadFrom(resp.Body)
		return resp.StatusCode, b.String()
	}
	if code, body := put(`{"expires_in_days":10}`); code != http.StatusOK || !strings.Contains(body, "expires_at") {
		t.Fatalf("renew = %d %s", code, body)
	}
	if code, _ := put(`{"expires_in_days":-2}`); code != http.StatusBadRequest {
		t.Errorf("negative expiry = %d", code)
	}
	if code, body := put(`{"expires_in_days":0}`); code != http.StatusOK || strings.Contains(body, "expires_at") {
		t.Fatalf("clear = %d %s", code, body)
	}
	if got, _ := s.getSite(t.Context(), "tmp"); got.ExpiresAt != 0 {
		t.Errorf("expiry = %d, want cleared", got.ExpiresAt)
	}
	// A description-only update leaves an expiry alone.
	if code, _ := put(`{"expires_in_days":5}`); code != http.StatusOK {
		t.Fatal(code)
	}
	if code, body := put(`{"description":"x"}`); code != http.StatusOK || !strings.Contains(body, "expires_at") {
		t.Fatalf("description update = %d %s", code, body)
	}
}

func TestDeployPreview(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	token := apiToken(t, ts, c)
	dir := writeSiteTree(t, map[string]string{"index.html": "preview"})
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := cli.DeployCommand(context.Background(), args, cliEnv(token, ts.URL), &out)
		return out.String(), err
	}

	// A restricted production site: its previews must not be public.
	if _, err := run("-visibility", "restricted", "blog", dir); err != nil {
		t.Fatal(err)
	}
	out, err := run("-preview", "feature-x", "-expires-days", "3", "blog", dir)
	if err != nil || !strings.Contains(out, "deployed blog--feature-x") {
		t.Fatalf("preview deploy = %q, %v", out, err)
	}
	prev, err := s.getSite(t.Context(), "blog--feature-x")
	if err != nil {
		t.Fatal(err)
	}
	if prev.Visibility != visRestricted {
		t.Errorf("preview visibility = %s, want restricted", prev.Visibility)
	}
	if d := time.Until(time.Unix(prev.ExpiresAt, 0)); d < 71*time.Hour || d > 73*time.Hour {
		t.Errorf("preview expires in %v, want about 72h", d)
	}
	if blog, _ := s.getSite(t.Context(), "blog"); blog.ExpiresAt != 0 {
		t.Error("production site got an expiry")
	}

	// Pushing again renews the expiry.
	if _, err := s.db.ExecContext(t.Context(), "UPDATE docs SET expires_at = ? WHERE name = ?", time.Now().Add(time.Hour).Unix(), prev.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := run("-preview", "feature-x", "-expires-days", "3", "blog", dir); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.getSite(t.Context(), prev.Name); again.ExpiresAt < prev.ExpiresAt-5 {
		t.Errorf("expiry %d not renewed to about %d", again.ExpiresAt, prev.ExpiresAt)
	}

	// The server only accepts previews of sites that exist.
	if _, err := run("-preview", "main", "docsite", dir); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("preview of a missing site = %v", err)
	}
	if _, err := s.getSite(t.Context(), "docsite--main"); !errors.Is(err, errSiteNotFound) {
		t.Errorf("preview of a missing site was created: %v", err)
	}

	// Cleanup: delete is idempotent and refuses to run without -preview.
	for range 2 {
		if out, err := run("-preview", "feature-x", "-delete", "blog"); err != nil || !strings.Contains(out, "deleted blog--feature-x") {
			t.Fatalf("delete = %q, %v", out, err)
		}
	}
	if _, err := s.getSite(t.Context(), "blog--feature-x"); !errors.Is(err, errSiteNotFound) {
		t.Errorf("preview survived delete: %v", err)
	}
	if _, err := run("-delete", "blog"); err == nil {
		t.Error("-delete without -preview accepted")
	}
	if _, err := s.getSite(t.Context(), "blog"); err != nil {
		t.Errorf("production site gone: %v", err)
	}
	for _, bad := range [][]string{
		{"-preview", "///", "blog", dir},
		{"-preview", "x", "-expires-days", "0", "blog", dir},
		{"-preview", "x", "-delete", "blog", dir},
	} {
		if _, err := run(bad...); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

func TestPreviewNamespaceIsOwnedByBaseSite(t *testing.T) {
	_, s := newTestServer(t)
	owner := testOwner(t, s)
	if _, err := s.db.ExecContext(t.Context(),
		"INSERT INTO user (userid, email, password) VALUES (2, 'two@example.com', 'x'), (3, 'three@example.com', 'x')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), "INSERT INTO groups (groupid, name, owner) VALUES (7, 'eng', ?)", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), "INSERT INTO user_group (uid, gid) VALUES (?, 7), (2, 7)", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createSite(t.Context(), "blog", "", owner, 7, visAuthenticated); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name  string
		user  int64
		vis   string
		wantE error
	}{
		{"blog--main", owner, visAuthenticated, nil},
		{"blog--wip", owner, visRestricted, nil}, // narrower is fine
		{"blog--pub", owner, visPublic, errPreviewVisible},
		{"blog--squat", 3, visAuthenticated, errPreviewOwner}, // not owner, not in group
		{"blog--team", 2, visAuthenticated, nil},              // member of blog's group
		{"nothing--x", owner, visPublic, errPreviewBase},      // base does not exist
		{"blog--a--b", owner, visAuthenticated, nil},          // only the first "--" counts
	} {
		_, err := s.createSite(t.Context(), tc.name, "", tc.user, 0, tc.vis)
		if !errors.Is(err, tc.wantE) {
			t.Errorf("createSite(%s, user %d, %s) = %v, want %v", tc.name, tc.user, tc.vis, err, tc.wantE)
		}
	}
	// Reserved LDH labels ("xn--") are refused outright.
	if _, err := s.createSite(t.Context(), "xn--abc", "", owner, 0, visPublic); err == nil {
		t.Error("xn--abc accepted")
	}
}

func TestPreviewDefaultsOverAPI(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites", "application/json", []byte(`{"name":"blog","visibility":"restricted"}`)), http.StatusCreated)
	// No visibility given: the preview inherits the base site's, it does not default to public.
	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites", "application/json", []byte(`{"name":"blog--x"}`)), http.StatusCreated)
	if got, _ := s.getSite(t.Context(), "blog--x"); got.Visibility != visRestricted {
		t.Errorf("visibility = %s, want restricted", got.Visibility)
	}
	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites", "application/json", []byte(`{"name":"blog--y","visibility":"public"}`)), http.StatusBadRequest)
	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites", "application/json", []byte(`{"name":"ghost--y"}`)), http.StatusBadRequest)
}

func TestReaperAuditsReason(t *testing.T) {
	_, s := newTestServer(t)
	site, err := s.createSiteExpiring(t.Context(), "tmp", "", testOwner(t, s), 0, visPublic, time.Now().Add(-time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.reapExpired(t.Context(), time.Now()); err != nil || n != 1 {
		t.Fatalf("reaped %d, %v", n, err)
	}
	var details string
	if err := s.db.QueryRowContext(t.Context(),
		"SELECT details FROM audit_log WHERE action = 'site.delete' AND siteid = ?", site.ID).Scan(&details); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(details, `"reason":"expired"`) {
		t.Errorf("details = %s", details)
	}
}
