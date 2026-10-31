package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// payload is an archive entry of n bytes, so a version's size is easy to predict.
func payload(n int) map[string]string {
	return map[string]string{"index.html": strings.Repeat("x", n)}
}

// dbBytes returns the bytes recorded for the site "blog" in the database.
func dbBytes(t *testing.T, s *Server) int64 {
	t.Helper()
	site, err := s.getSite(t.Context(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	n, err := s.siteBytes(t.Context(), site.ID)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// noStaging fails the test if a staging directory is left in the sites root.
func noStaging(t *testing.T, s *Server) {
	t.Helper()
	left, _ := filepath.Glob(filepath.Join(s.sites, ".staging-*"))
	if len(left) != 0 {
		t.Fatalf("staging left behind: %v", left)
	}
}

func TestSiteQuota(t *testing.T) {
	_, s := newTestServer(t)
	s.cfg.SiteQuota = 1000
	s.cfg.KeepVersions = 2
	ensureBlogSite(t, s)

	v1 := deployFiles(t, s, payload(400))
	if got := dbBytes(t, s); got != 400 {
		t.Fatalf("usage = %d, want 400", got)
	}
	// 400 + 700 is over the limit and both versions would be kept.
	_, err := s.deploy(t.Context(), mustSite(t, s, "blog"), writeZip(t, payload(700)))
	var qe *quotaError
	if !errors.As(err, &qe) || !strings.Contains(qe.Error(), "site quota") || !strings.Contains(qe.Error(), "1100 bytes") {
		t.Fatalf("deploy over quota = %v, want a site *quotaError naming the numbers", err)
	}
	noStaging(t, s)
	if cur, _ := s.CurrentVersion("blog"); cur != v1 {
		t.Fatalf("rejected deploy changed current to %q", cur)
	}
	if vs, _ := s.Versions("blog"); len(vs) != 1 {
		t.Fatalf("rejected deploy left versions %v", vs)
	}
	// A deploy that fits is accepted, up to exactly the limit.
	deployFiles(t, s, payload(600))
	if got := dbBytes(t, s); got != 1000 {
		t.Fatalf("usage = %d, want 1000", got)
	}
}

// The check counts the state after pruning, so a site at its limit can replace versions.
func TestSiteQuotaCountsPrunedState(t *testing.T) {
	_, s := newTestServer(t)
	s.cfg.SiteQuota = 1000
	s.cfg.KeepVersions = 2
	deployFiles(t, s, payload(500))
	deployFiles(t, s, payload(500)) // at the limit: 500 + 500
	// The oldest version is pruned by this deploy, leaving 500 + 500 again.
	deployFiles(t, s, payload(500))
	if got := dbBytes(t, s); got != 1000 {
		t.Fatalf("usage after replacing = %d, want 1000", got)
	}
	vs, _ := s.Versions("blog")
	if len(vs) != 2 {
		t.Fatalf("versions = %v, want 2", vs)
	}
	// With one version kept, a single big version may use the whole quota.
	s.cfg.KeepVersions = 1
	deployFiles(t, s, payload(1000))
	if got := dbBytes(t, s); got != 1000 {
		t.Fatalf("usage = %d, want 1000", got)
	}
	if _, err := s.deploy(t.Context(), mustSite(t, s, "blog"), writeZip(t, payload(1001))); err == nil {
		t.Fatal("deploy over the limit succeeded")
	}
	noStaging(t, s)
}

func mustSite(t *testing.T, s *Server, name string) Site {
	t.Helper()
	site, err := s.getSite(t.Context(), name)
	if err != nil {
		t.Fatal(err)
	}
	return site
}

func TestUserQuotaSpansSites(t *testing.T) {
	_, s := newTestServer(t)
	s.cfg.UserQuota = 1000
	s.cfg.KeepVersions = 1
	owner := testOwner(t, s)
	for _, name := range []string{"blog", "notes"} {
		if _, err := s.createSite(t.Context(), name, "", owner, 0, visPublic); err != nil {
			t.Fatal(err)
		}
	}
	deploy := func(name string, n int) error {
		_, err := s.deploy(t.Context(), mustSite(t, s, name), writeZip(t, payload(n)))
		return err
	}
	if err := deploy("blog", 600); err != nil {
		t.Fatal(err)
	}
	var qe *quotaError
	if err := deploy("notes", 500); !errors.As(err, &qe) || !strings.Contains(qe.Error(), "user quota") {
		t.Fatalf("second site over the user quota = %v", err)
	}
	noStaging(t, s)
	if err := deploy("notes", 400); err != nil {
		t.Fatal(err)
	}
	// Replacing a site's own version only counts the other sites besides the new version.
	if err := deploy("blog", 600); err != nil {
		t.Fatalf("replacing within the user quota: %v", err)
	}
	if err := deploy("blog", 601); !errors.As(err, &qe) {
		t.Fatalf("replacing over the user quota = %v", err)
	}
	// Another user's sites do not count.
	if _, err := s.db.ExecContext(t.Context(),
		"INSERT INTO user (userid, email, password) VALUES (2, 'two@example.com', 'x')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createSite(t.Context(), "other", "", 2, 0, visPublic); err != nil {
		t.Fatal(err)
	}
	if err := deploy("other", 900); err != nil {
		t.Fatalf("other user's deploy: %v", err)
	}
}

func TestSitesPerUser(t *testing.T) {
	ts, s := newTestServer(t)
	s.cfg.SitesPerUser = 2
	c := loggedInClient(t, ts)
	createSiteVia(t, ts, c, "one")
	createSiteVia(t, ts, c, "two")
	url := ts.URL + "/v1/auth/sites"
	resp := post(t, c, url, "application/json", []byte(`{"name":"three"}`))
	expectStatus(t, resp, http.StatusForbidden)
	if _, err := s.getSite(t.Context(), "three"); !errors.Is(err, errSiteNotFound) {
		t.Fatalf("site created over the limit: %v", err)
	}
	// A name clash is still a conflict, and deleting a site frees a slot.
	expectStatus(t, post(t, c, url, "application/json", []byte(`{"name":"one"}`)), http.StatusConflict)
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodDelete, url+"/one", http.NoBody)
	del, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	del.Body.Close()
	expectStatus(t, del.StatusCode, http.StatusOK)
	expectStatus(t, post(t, c, url, "application/json", []byte(`{"name":"three"}`)), http.StatusCreated)
}

func TestUploadOverQuotaIs413(t *testing.T) {
	ts, s := newTestServer(t)
	s.cfg.SiteQuota = 500
	c := loggedInClient(t, ts)
	createSiteVia(t, ts, c, "blog")
	up := ts.URL + "/v1/auth/sites/blog/upload"

	resp, err := c.Post(up, "application/zip", strings.NewReader(string(zipArchive(t, payload(900)))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp.StatusCode, http.StatusRequestEntityTooLarge)
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || !strings.Contains(body["error"], "site quota exceeded") {
		t.Fatalf("body = %v, %v", body, err)
	}
	noStaging(t, s)
	expectStatus(t, post(t, c, up, "application/zip", zipArchive(t, payload(100))), http.StatusOK)
}

func TestUsageIsReported(t *testing.T) {
	ts, s := newTestServer(t)
	s.cfg.SiteQuota, s.cfg.UserQuota, s.cfg.SitesPerUser = 5000, 9000, 3
	c := loggedInClient(t, ts)
	createSiteVia(t, ts, c, "blog")
	expectStatus(t, post(t, c, ts.URL+"/v1/auth/sites/blog/upload", "application/zip", zipArchive(t, payload(300))), http.StatusOK)

	var user userInfo
	getJSON(t, c, ts.URL+"/v1/auth/user", &user)
	want := userQuota{UsageBytes: 300, LimitBytes: 9000, SiteLimitBytes: 5000, Sites: 1, SiteLimit: 3}
	if user.Quota != want {
		t.Fatalf("quota = %+v, want %+v", user.Quota, want)
	}
	if len(user.Docs.Owned) != 1 || user.Docs.Owned[0].Bytes != 300 {
		t.Fatalf("owned = %+v", user.Docs.Owned)
	}
	var site struct {
		Bytes      int64 `json:"bytes"`
		QuotaBytes int64 `json:"quota_bytes"`
	}
	getJSON(t, c, ts.URL+"/v1/auth/sites/blog", &site)
	if site.Bytes != 300 || site.QuotaBytes != 5000 {
		t.Fatalf("site = %+v", site)
	}
}

func getJSON(t *testing.T, c *http.Client, url string, v any) {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp.StatusCode, http.StatusOK)
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatal(err)
	}
}

// Sites deployed before the site_version table existed are measured from their directories,
// at startup and, failing that, when the next deploy checks a quota.
func TestVersionSizesBackfill(t *testing.T) {
	_, s := newTestServer(t)
	s.cfg.KeepVersions = 3
	deployFiles(t, s, map[string]string{"index.html": strings.Repeat("x", 100), "sub/a.css": strings.Repeat("y", 50)})
	v2 := deployFiles(t, s, payload(30))
	if got := dbBytes(t, s); got != 180 {
		t.Fatalf("usage = %d, want 180", got)
	}

	if _, err := s.db.ExecContext(t.Context(), "DELETE FROM site_version"); err != nil {
		t.Fatal(err)
	}
	if got := dbBytes(t, s); got != 0 {
		t.Fatalf("usage without rows = %d", got)
	}
	if err := s.backfillVersionSizes(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := dbBytes(t, s); got != 180 {
		t.Fatalf("usage after backfill = %d, want 180", got)
	}

	// A row for a version that is gone is dropped; a missing row is measured at the next check.
	if _, err := s.db.ExecContext(t.Context(), "INSERT INTO site_version (docid, version, bytes) VALUES (?, 'zzzzzzzzzzzzzzzzzzzz', 999)", mustSite(t, s, "blog").ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), "DELETE FROM site_version WHERE version = ?", v2); err != nil {
		t.Fatal(err)
	}
	s.cfg.SiteQuota = 1 << 20
	deployFiles(t, s, payload(20))
	if got := dbBytes(t, s); got != 200 {
		t.Fatalf("usage after lazy sync = %d, want 200", got)
	}
}

func TestStoredBytesMetric(t *testing.T) {
	_, s := newTestServer(t)
	stored := func() string {
		var b strings.Builder
		s.metrics.Write(&b)
		for _, line := range strings.Split(b.String(), "\n") {
			if v, ok := strings.CutPrefix(line, "openpages_stored_bytes "); ok {
				return v
			}
		}
		return ""
	}
	deployFiles(t, s, payload(70))
	if got := stored(); got != "70" {
		t.Fatalf("stored bytes = %q, want 70", got)
	}
	if err := s.deleteSite(t.Context(), mustSite(t, s, "blog")); err != nil {
		t.Fatal(err)
	}
	if got := stored(); got != "0" {
		t.Fatalf("stored bytes after delete = %q, want 0", got)
	}
}

func TestDirSizeAndHumanSize(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, n := range map[string]int{"a": 10, "d/b": 5} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, n), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := dirSize(dir); err != nil || n != 15 {
		t.Fatalf("dirSize = %d, %v", n, err)
	}
	if _, err := dirSize(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("dirSize of a missing dir succeeded")
	}
	if humanSize(512) != "512 bytes" || humanSize(3<<19) != "1.5 MiB" {
		t.Fatalf("humanSize = %q, %q", humanSize(512), humanSize(3<<19))
	}
}

// Concurrent deploys cannot all pass on the same headroom.
func TestConcurrentDeploysRespectQuota(t *testing.T) {
	_, s := newTestServer(t)
	s.cfg.SiteQuota = 1000
	s.cfg.KeepVersions = 10
	ensureBlogSite(t, s)
	site := mustSite(t, s, "blog")
	const n = 6
	files := make([]*os.File, n)
	for i := range files {
		files[i] = writeZip(t, payload(400))
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, f := range files {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.deploy(t.Context(), site, f)
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		var qe *quotaError
		switch {
		case err == nil:
			ok++
		case !errors.As(err, &qe):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 2 || dbBytes(t, s) != 800 {
		t.Fatalf("%d deploys passed, usage %d; want 2 and 800", ok, dbBytes(t, s))
	}
	noStaging(t, s)
}

// Rows missing for another site are measured again, not trusted as zero.
func TestUserQuotaMeasuresOtherSitesMissingRows(t *testing.T) {
	_, s := newTestServer(t)
	s.cfg.UserQuota = 1000
	owner := testOwner(t, s)
	for _, name := range []string{"blog", "notes"} {
		if _, err := s.createSite(t.Context(), name, "", owner, 0, visPublic); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.deploy(t.Context(), mustSite(t, s, "blog"), writeZip(t, payload(600))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), "DELETE FROM site_version"); err != nil {
		t.Fatal(err)
	}
	_, err := s.deploy(t.Context(), mustSite(t, s, "notes"), writeZip(t, payload(500)))
	var qe *quotaError
	if !errors.As(err, &qe) || !strings.Contains(qe.Error(), "user quota") {
		t.Fatalf("deploy with unrecorded bytes elsewhere = %v, want a user quota error", err)
	}
	// The rejection also repaired the rows.
	if got := dbBytes(t, s); got != 600 {
		t.Fatalf("usage after sync = %d, want 600", got)
	}
}

// A version that cannot be deleted stays counted: it earns no credit for later deploys.
func TestQuotaDoesNotCreditUndeletableVersion(t *testing.T) {
	_, s := newTestServer(t)
	s.cfg.SiteQuota = 1000
	s.cfg.KeepVersions = 1
	v1 := deployFiles(t, s, payload(600))
	stuck := filepath.Join(s.SiteDir("blog"), versionsDir, v1)
	orig := removeAll
	removeAll = func(p string) error {
		if p == stuck {
			return errors.New("device busy")
		}
		return orig(p)
	}
	t.Cleanup(func() { removeAll = orig })

	// 600 + 400 fits only by crediting v1, whose removal then fails.
	deployFiles(t, s, payload(400))
	if vs, _ := s.Versions("blog"); len(vs) != 2 {
		t.Fatalf("versions = %v, want the stuck one kept", vs)
	}
	if _, err := s.deploy(t.Context(), mustSite(t, s, "blog"), writeZip(t, payload(400))); err == nil {
		t.Fatal("deploy passed by crediting an undeletable version")
	}
	noStaging(t, s)

	removeAll = orig
	deployFiles(t, s, payload(400))
	if vs, _ := s.Versions("blog"); len(vs) != 1 {
		t.Fatalf("versions after the delete works again = %v", vs)
	}
}
