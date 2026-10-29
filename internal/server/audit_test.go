package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gira0/open-pages/internal/config"
)

// auditPageVia fetches one page of an audit endpoint and returns its status and body.
func auditPageVia(t *testing.T, c *http.Client, url string) (int, []map[string]any, any) {
	t.Helper()
	code, out := doJSON(t, c, http.MethodGet, url, "")
	var entries []map[string]any
	if raw, ok := out["entries"].([]any); ok {
		for _, e := range raw {
			entries = append(entries, e.(map[string]any))
		}
	}
	return code, entries, out["next_before"]
}

// auditActions returns the actions of the caller's own audit entries, newest first.
func auditActions(t *testing.T, ts *httptest.Server, c *http.Client) []string {
	t.Helper()
	code, entries, _ := auditPageVia(t, c, ts.URL+"/v1/auth/user/audit")
	expectStatus(t, code, http.StatusOK)
	var out []string
	for _, e := range entries {
		out = append(out, e["action"].(string))
	}
	return out
}

func wantActions(t *testing.T, got []string, want ...string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("audit actions = %v, want %v", got, want)
	}
}

// logBuffer is a goroutine-safe buffer for capturing log output.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestAuditSiteActions(t *testing.T) {
	logs := &logBuffer{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts)
	createSiteVia(t, ts, c, "blog")
	base := ts.URL + "/v1/auth/sites/blog"
	v1 := uploadVia(t, ts, c, "blog", "one")
	v2 := uploadVia(t, ts, c, "blog", "two")
	_ = v2
	code, _ := doJSON(t, c, http.MethodPost, base+"/rollback", `{"version":"`+v1+`"}`)
	expectStatus(t, code, http.StatusOK)
	code, _ = doJSON(t, c, http.MethodPut, base, `{"description":"hi","visibility":"authenticated"}`)
	expectStatus(t, code, http.StatusOK)
	// An update that changes nothing, and one that fails, leave no entry.
	code, _ = doJSON(t, c, http.MethodPut, base, `{"description":"hi"}`)
	expectStatus(t, code, http.StatusOK)
	code, _ = doJSON(t, c, http.MethodPut, base, `{"visibility":"bogus"}`)
	expectStatus(t, code, http.StatusBadRequest)
	code, _ = doJSON(t, c, http.MethodPost, base+"/rollback", `{"version":"00000000000000000000"}`)
	expectStatus(t, code, http.StatusNotFound)

	code, entries, next := auditPageVia(t, c, base+"/audit")
	expectStatus(t, code, http.StatusOK)
	if next != nil {
		t.Fatalf("next_before = %v on a short list", next)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e["action"].(string))
	}
	wantActions(t, got, "site.update", "site.rollback", "site.deploy", "site.deploy", "site.create")

	upd := entries[0]
	changes := upd["details"].(map[string]any)["changes"].(map[string]any)
	if len(changes) != 2 ||
		changes["visibility"].(map[string]any)["from"] != "public" || changes["visibility"].(map[string]any)["to"] != "authenticated" ||
		changes["description"].(map[string]any)["to"] != "hi" {
		t.Errorf("update details = %v", upd["details"])
	}
	if entries[1]["details"].(map[string]any)["version"] != v1 || entries[3]["details"].(map[string]any)["version"] != v1 {
		t.Errorf("rollback/deploy details = %v / %v", entries[1]["details"], entries[3]["details"])
	}
	first := entries[4]
	if first["actor_email"] != "a@example.com" || first["actor_via"] != "session" || first["client_ip"] != "127.0.0.1" ||
		first["target_kind"] != "site" || first["target"] != "blog" || first["actor"] == nil {
		t.Errorf("create entry = %v", first)
	}
	if _, err := time.Parse(time.RFC3339, first["time"].(string)); err != nil {
		t.Errorf("time %v: %v", first["time"], err)
	}

	code, _ = doJSON(t, c, http.MethodDelete, base, "")
	expectStatus(t, code, http.StatusOK)
	wantActions(t, auditActions(t, ts, c), "site.delete", "site.update", "site.rollback", "site.deploy", "site.deploy", "site.create")

	// Each entry also went to the log.
	for _, a := range []string{"site.create", "site.update", "site.deploy", "site.rollback", "site.delete"} {
		if !strings.Contains(logs.String(), `"msg":"audit"`) || !strings.Contains(logs.String(), `"action":"`+a+`"`) {
			t.Errorf("no audit log line for %s", a)
		}
	}
	if !strings.Contains(logs.String(), `"actor_email":"a@example.com"`) {
		t.Error("audit log line lacks the actor email")
	}
}

func TestAuditGroupAndTokenActions(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	b := secondUser(t, ts)
	bID := int64(2)

	gid := newGroupVia(t, ts, c, "Eng")
	code, _ := doJSON(t, c, http.MethodPost, groupURL(ts, gid, "/members"), `{"email":"b@example.com"}`)
	expectStatus(t, code, http.StatusCreated)
	// Adding a member twice, and a refused removal, record nothing more.
	code, _ = doJSON(t, c, http.MethodPost, groupURL(ts, gid, "/members"), `{"email":"b@example.com"}`)
	expectStatus(t, code, http.StatusOK)
	code, _ = doJSON(t, b, http.MethodDelete, groupURL(ts, gid, "")+"/members/1", "")
	expectStatus(t, code, http.StatusBadRequest) // the owner can't be removed
	code, _ = doJSON(t, c, http.MethodDelete, groupURL(ts, gid, "/members/"+strconv.FormatInt(bID, 10)), "")
	expectStatus(t, code, http.StatusOK)
	code, _ = doJSON(t, b, http.MethodDelete, groupURL(ts, gid, ""), "")
	expectStatus(t, code, http.StatusForbidden)
	code, _ = doJSON(t, c, http.MethodDelete, groupURL(ts, gid, ""), "")
	expectStatus(t, code, http.StatusOK)

	m := mintToken(t, ts, c, `{"name":"ci","expires_in_days":30}`)
	tokenID := int64(m["id"].(float64))
	token := m["token"].(string)
	code, _ = doJSON(t, c, http.MethodDelete, ts.URL+"/v1/auth/tokens/"+strconv.FormatInt(tokenID, 10), "")
	expectStatus(t, code, http.StatusOK)
	code, _ = doJSON(t, c, http.MethodDelete, ts.URL+"/v1/auth/tokens/"+strconv.FormatInt(tokenID, 10), "")
	expectStatus(t, code, http.StatusNotFound)

	wantActions(t, auditActions(t, ts, c), "token.revoke", "token.create",
		"group.delete", "group.member_remove", "group.member_add", "group.create")
	if got := auditActions(t, ts, b); len(got) != 0 {
		t.Errorf("second user's audit = %v, want none", got)
	}

	_, entries, _ := auditPageVia(t, c, ts.URL+"/v1/auth/user/audit")
	revoke, create := entries[0], entries[1]
	if revoke["target_kind"] != "token" || revoke["target"] != "ci" || revoke["details"].(map[string]any)["prefix"] != token[:12] {
		t.Errorf("revoke entry = %v", revoke)
	}
	if d := create["details"].(map[string]any); d["token_id"] != float64(tokenID) || d["expires_in_days"] != float64(30) || strings.Contains(create["details"].(map[string]any)["prefix"].(string), token[12:]) {
		t.Errorf("create entry details = %v", d)
	}
	del, rm, add, mk := entries[2], entries[3], entries[4], entries[5]
	for _, e := range []map[string]any{del, rm, add, mk} {
		if e["target_kind"] != "group" || e["target"] != "Eng" {
			t.Errorf("group entry = %v", e)
		}
	}
	if d := add["details"].(map[string]any); d["email"] != "b@example.com" {
		t.Errorf("member_add details = %v", d)
	}
	if d := rm["details"].(map[string]any); d["email"] != "b@example.com" || d["user_id"] != float64(bID) {
		t.Errorf("member_remove details = %v", d)
	}

	// Snapshots survive their targets: the group is gone but its name is still on record.
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE groupid = ? AND target = 'Eng'", gid).Scan(&n); err != nil || n != 4 {
		t.Errorf("group entries = %d, err %v", n, err)
	}

	// A call made with an API token is attributed to it.
	createReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/auth/groups", strings.NewReader(`{"name":"viatoken"}`))
	m2 := mintToken(t, ts, c, `{"name":"ci2"}`)
	tok2 := m2["token"].(string)
	createReq.Header.Set("Authorization", "Bearer "+tok2)
	createReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(createReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	expectStatus(t, resp.StatusCode, http.StatusCreated)
	_, entries, _ = auditPageVia(t, c, ts.URL+"/v1/auth/user/audit")
	if entries[0]["action"] != "group.create" || entries[0]["actor_via"] != "token:"+tok2[:12] {
		t.Errorf("token-made entry = %v", entries[0])
	}
}

func TestSiteAuditAccess(t *testing.T) {
	ts, _ := newTestServer(t)
	owner := loggedInClient(t, ts)
	other := secondUser(t, ts)
	anon := newClient(t)
	createSiteVia(t, ts, owner, "blog")
	url := ts.URL + "/v1/auth/sites/blog/audit"

	code, _, _ := auditPageVia(t, anon, url)
	expectStatus(t, code, http.StatusUnauthorized)
	code, _, _ = auditPageVia(t, anon, ts.URL+"/v1/auth/user/audit")
	expectStatus(t, code, http.StatusUnauthorized)
	code, _, _ = auditPageVia(t, other, url)
	expectStatus(t, code, http.StatusForbidden)
	code, _, _ = auditPageVia(t, owner, ts.URL+"/v1/auth/sites/nope/audit")
	expectStatus(t, code, http.StatusNotFound)
	code, _, _ = auditPageVia(t, owner, ts.URL+"/v1/auth/sites/Bad_Name/audit")
	expectStatus(t, code, http.StatusNotFound)
	code, entries, _ := auditPageVia(t, owner, url)
	expectStatus(t, code, http.StatusOK)
	if len(entries) != 1 {
		t.Fatalf("entries = %v", entries)
	}

	// A site deleted and recreated by someone else starts with a clean history, even if
	// SQLite reuses the row id.
	code, _ = doJSON(t, owner, http.MethodDelete, ts.URL+"/v1/auth/sites/blog", "")
	expectStatus(t, code, http.StatusOK)
	createSiteVia(t, ts, other, "blog")
	code, entries, _ = auditPageVia(t, other, url)
	expectStatus(t, code, http.StatusOK)
	if len(entries) != 1 || entries[0]["action"] != "site.create" || entries[0]["actor_email"] != "b@example.com" {
		t.Fatalf("new owner sees %v", entries)
	}
	code, _, _ = auditPageVia(t, owner, url)
	expectStatus(t, code, http.StatusForbidden)
}

func TestAuditPagination(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts)
	createSiteVia(t, ts, c, "blog")
	for i := range 6 {
		code, _ := doJSON(t, c, http.MethodPut, ts.URL+"/v1/auth/sites/blog", `{"description":"d`+strconv.Itoa(i)+`"}`)
		expectStatus(t, code, http.StatusOK)
	}
	url := ts.URL + "/v1/auth/sites/blog/audit"

	var ids []float64
	cursor := ""
	for page := 0; ; page++ {
		code, entries, next := auditPageVia(t, c, url+"?limit=3"+cursor)
		expectStatus(t, code, http.StatusOK)
		for _, e := range entries {
			ids = append(ids, e["id"].(float64))
		}
		if next == nil {
			if page != 2 || len(entries) != 1 {
				t.Fatalf("last page %d has %d entries", page, len(entries))
			}
			break
		}
		if len(entries) != 3 || next != entries[2]["id"] {
			t.Fatalf("page %d: %d entries, next %v", page, len(entries), next)
		}
		cursor = "&before=" + strconv.FormatFloat(next.(float64), 'f', 0, 64)
	}
	if len(ids) != 7 {
		t.Fatalf("collected %d entries, want 7", len(ids))
	}
	for i := 1; i < len(ids); i++ {
		if ids[i] >= ids[i-1] {
			t.Fatalf("ids not strictly descending: %v", ids)
		}
	}

	// An exact multiple of the limit has no next page; defaults and clamping are accepted.
	code, entries, next := auditPageVia(t, c, url+"?limit=7")
	if code != http.StatusOK || len(entries) != 7 || next != nil {
		t.Errorf("limit=7: %d, %d entries, next %v", code, len(entries), next)
	}
	code, entries, _ = auditPageVia(t, c, url+"?limit=100000")
	if code != http.StatusOK || len(entries) != 7 {
		t.Errorf("limit=100000: %d, %d entries", code, len(entries))
	}
	for _, q := range []string{"limit=0", "limit=-1", "limit=x", "before=0", "before=x", "before=-3"} {
		if code, _, _ := auditPageVia(t, c, url+"?"+q); code != http.StatusBadRequest {
			t.Errorf("?%s = %d, want 400", q, code)
		}
	}
	code, entries, _ = auditPageVia(t, c, url+"?before=1")
	if code != http.StatusOK || len(entries) != 0 {
		t.Errorf("before=1: %d, %v", code, entries)
	}
}

func TestAuditRetention(t *testing.T) {
	cfg := config.Default()
	cfg.DataPath, cfg.TmpPath = t.TempDir(), t.TempDir()
	cfg.AppHost, cfg.ContentDomain = "127.0.0.1", testContentDomain
	cfg.AuditRetention = 30
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(age time.Duration) {
		t.Helper()
		if _, err := s.db.Exec(`INSERT INTO audit_log (time, action, target_kind, target) VALUES (?, 'site.create', 'site', 'x')`,
			time.Now().Add(-age).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	count := func() (n int) {
		t.Helper()
		if err := s.db.QueryRow("SELECT COUNT(*) FROM audit_log").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	day := 24 * time.Hour
	insert(40 * day)
	insert(31 * day)
	insert(29 * day)
	insert(0)
	if err := s.pruneAudit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 2 {
		t.Fatalf("after prune %d entries, want 2", n)
	}

	s.cfg.AuditRetention = 0
	insert(3000 * day)
	if err := s.pruneAudit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if n := count(); n != 3 {
		t.Fatalf("retention 0 pruned: %d entries, want 3", n)
	}

	// Close stops the background pruner and returns.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// New prunes at startup.
	cfg.AuditRetention = 1
	s2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	var n int
	if err := s2.db.QueryRow("SELECT COUNT(*) FROM audit_log").Scan(&n); err != nil || n != 1 {
		t.Fatalf("after restart with retention 1: %d entries, err %v; want 1", n, err)
	}
}

func TestAuditClientIPAndBadRemoteAddr(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "[2001:db8::1]:5555"
	if got := clientIP(r); got != "2001:db8::1" {
		t.Errorf("clientIP = %q", got)
	}
	r.RemoteAddr = "pipe"
	if got := clientIP(r); got != "pipe" {
		t.Errorf("clientIP = %q", got)
	}
}

// A failing insert is logged and does not break the audited change.
func TestAuditWriteFailureDoesNotFailRequest(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	if _, err := s.db.Exec("DROP TABLE audit_log"); err != nil {
		t.Fatal(err)
	}
	gid := newGroupVia(t, ts, c, "Eng")
	if gid == 0 {
		t.Fatal("no group")
	}
	// Other changes go through; the group was created.
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM groups").Scan(&n); err != nil || n != 1 {
		t.Fatalf("groups = %d, err %v", n, err)
	}
}

// The site.create entry is the start of a site's history and is written in the same
// transaction as the site row: if it cannot be written there is no site.
func TestSiteCreateNeedsAuditMarker(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	if _, err := s.db.Exec(`CREATE TRIGGER no_audit BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT, 'no audit'); END`); err != nil {
		t.Fatal(err)
	}
	code, out := doJSON(t, c, http.MethodPost, ts.URL+"/v1/auth/sites", `{"name":"blog"}`)
	expectStatus(t, code, http.StatusInternalServerError)
	if out["error"] != "internal error" {
		t.Errorf("body = %v", out)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM docs").Scan(&n); err != nil || n != 0 {
		t.Fatalf("site rows after failed create = %d, err %v; want 0", n, err)
	}
	if _, err := s.db.Exec("DROP TRIGGER no_audit"); err != nil {
		t.Fatal(err)
	}
	createSiteVia(t, ts, c, "blog")
	if err := s.db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action = 'site.create'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("create markers = %d, err %v", n, err)
	}
}

// The group name in the delete entry is that of the row deleted, also when the id was
// used by another group before.
func TestAuditGroupDeleteUsesDeletedName(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	first := newGroupVia(t, ts, c, "Old")
	code, _ := doJSON(t, c, http.MethodDelete, groupURL(ts, first, ""), "")
	expectStatus(t, code, http.StatusOK)
	second := newGroupVia(t, ts, c, "New")
	if second != first {
		t.Skipf("group id not reused (%d, %d)", first, second)
	}
	code, _ = doJSON(t, c, http.MethodDelete, groupURL(ts, second, ""), "")
	expectStatus(t, code, http.StatusOK)
	var name string
	if err := s.db.QueryRow("SELECT target FROM audit_log WHERE action = 'group.delete' ORDER BY auditid DESC LIMIT 1").Scan(&name); err != nil || name != "New" {
		t.Fatalf("delete entry target = %q, err %v", name, err)
	}
}

// The delete entry is written in the delete's own transaction, under the deploy lock, so it
// always has a lower id than the create entry of a site that takes over the name (and,
// SQLite permitting, the row id). The new owner never sees it.
func TestAuditDeleteOrderedBeforeRecreate(t *testing.T) {
	ts, s := newTestServer(t)
	owner := loggedInClient(t, ts)
	other := secondUser(t, ts)
	createSiteVia(t, ts, owner, "blog")
	site, err := s.getSite(t.Context(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(t.Context(), userIDKey, int64(1))
	if err := s.deleteSite(ctx, site); err != nil {
		t.Fatal(err)
	}
	createSiteVia(t, ts, other, "blog")
	again, err := s.getSite(t.Context(), "blog")
	if err != nil {
		t.Fatal(err)
	}

	var del, create int64
	if err := s.db.QueryRow("SELECT MAX(auditid) FROM audit_log WHERE action = 'site.delete' AND siteid = ?", site.ID).Scan(&del); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT MAX(auditid) FROM audit_log WHERE action = 'site.create' AND siteid = ?", again.ID).Scan(&create); err != nil {
		t.Fatal(err)
	}
	if again.ID != site.ID {
		t.Fatalf("row id not reused (%d, %d); the test needs the reuse case", site.ID, again.ID)
	}
	if del == 0 || del >= create {
		t.Fatalf("delete entry %d, re-create entry %d: want delete first", del, create)
	}
	_, entries, _ := auditPageVia(t, other, ts.URL+"/v1/auth/sites/blog/audit")
	if len(entries) != 1 || entries[0]["action"] != "site.create" {
		t.Fatalf("new owner sees %v", entries)
	}
}

// Concurrent updates each record the exact before/after pair they made, so the entries
// chain: every "from" is the previous entry's "to".
func TestAuditUpdateDiffsChain(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts)
	createSiteVia(t, ts, c, "blog")
	const n = 8
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			doJSON(t, c, http.MethodPut, ts.URL+"/v1/auth/sites/blog", `{"description":"d`+strconv.Itoa(i)+`"}`)
		})
	}
	wg.Wait()
	_, entries, _ := auditPageVia(t, c, ts.URL+"/v1/auth/sites/blog/audit?limit=100")
	// Newest first; the oldest entry is the create.
	prev := ""
	updates := 0
	for i := len(entries) - 2; i >= 0; i-- {
		ch := entries[i]["details"].(map[string]any)["changes"].(map[string]any)["description"].(map[string]any)
		if ch["from"] != prev {
			t.Fatalf("entry %d from %q, want %q", i, ch["from"], prev)
		}
		prev = ch["to"].(string)
		updates++
	}
	if updates == 0 {
		t.Fatal("no updates recorded")
	}
}

func TestRollbackOfDeletedSite(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	createSiteVia(t, ts, c, "blog")
	v := uploadVia(t, ts, c, "blog", "one")
	site, _ := s.getSite(t.Context(), "blog")
	code, _ := doJSON(t, c, http.MethodDelete, ts.URL+"/v1/auth/sites/blog", "")
	expectStatus(t, code, http.StatusOK)
	if err := s.rollbackSite(t.Context(), site, v); !errors.Is(err, errSiteNotFound) {
		t.Fatalf("rollback of deleted site = %v, want errSiteNotFound", err)
	}
}

// reusedSite deletes "blog" (owned by user 1) and lets user 2 take the name and row id.
// It returns the stale view of the old site and the new one.
func reusedSite(t *testing.T, ts *httptest.Server, s *Server) (stale, fresh Site) {
	t.Helper()
	owner := loggedInClient(t, ts)
	other := secondUser(t, ts)
	createSiteVia(t, ts, owner, "blog")
	stale, err := s.getSite(t.Context(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	code, _ := doJSON(t, owner, http.MethodDelete, ts.URL+"/v1/auth/sites/blog", "")
	expectStatus(t, code, http.StatusOK)
	code, _ = doJSON(t, other, http.MethodPost, ts.URL+"/v1/auth/sites", `{"name":"blog","description":"secret"}`)
	expectStatus(t, code, http.StatusCreated)
	if fresh, err = s.getSite(t.Context(), "blog"); err != nil {
		t.Fatal(err)
	}
	if fresh.ID != stale.ID {
		t.Fatalf("row id not reused (%d, %d)", stale.ID, fresh.ID)
	}
	return stale, fresh
}

// A request authorized for the old site must not read the audit log of its successor.
func TestSiteAuditStaleAuthorization(t *testing.T) {
	ts, s := newTestServer(t)
	stale, _ := reusedSite(t, ts, s)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/auth/sites/blog/audit", nil)
	s.writeSiteAudit(rec, req, stale, stale.OwnerID)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"entries":[]`) {
		t.Fatalf("stale owner got %d %s", rec.Code, rec.Body.String())
	}
}

// An update authorized for the old site must not land on, or reveal, the successor.
func TestUpdateSiteStaleAuthorization(t *testing.T) {
	ts, s := newTestServer(t)
	stale, fresh := reusedSite(t, ts, s)
	desc := "defaced"
	if _, err := s.updateSite(t.Context(), stale, &desc, nil, nil); !errors.Is(err, errSiteNotFound) {
		t.Fatalf("updateSite = %v, want errSiteNotFound", err)
	}
	got, err := s.getSite(t.Context(), "blog")
	if err != nil || got.Description != fresh.Description {
		t.Fatalf("successor changed: %+v, %v", got, err)
	}
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM audit_log WHERE action = 'site.update'").Scan(&n); err != nil || n != 0 {
		t.Fatalf("update entries = %d, err %v", n, err)
	}
	if err := s.deleteSite(t.Context(), stale); !errors.Is(err, errSiteNotFound) {
		t.Fatalf("deleteSite = %v, want errSiteNotFound", err)
	}
}

// Member entries carry the name of the group the change was made in, also when the group
// id was used by another group before.
func TestAuditMemberEntriesUseCurrentGroupName(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	secondUser(t, ts)
	old := newGroupVia(t, ts, c, "Old")
	code, _ := doJSON(t, c, http.MethodDelete, groupURL(ts, old, ""), "")
	expectStatus(t, code, http.StatusOK)
	gid := newGroupVia(t, ts, c, "New")
	if gid != old {
		t.Fatalf("group id not reused (%d, %d)", old, gid)
	}
	code, _ = doJSON(t, c, http.MethodPost, groupURL(ts, gid, "/members"), `{"email":"b@example.com"}`)
	expectStatus(t, code, http.StatusCreated)
	code, _ = doJSON(t, c, http.MethodDelete, groupURL(ts, gid, "/members/2"), "")
	expectStatus(t, code, http.StatusOK)
	rows, err := s.db.Query("SELECT target FROM audit_log WHERE action LIKE 'group.member_%'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil || target != "New" {
			t.Fatalf("target = %q, err %v", target, err)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("member entries = %d, want 2", n)
	}
}
