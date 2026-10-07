package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// userClient registers and logs in a user with the given email.
func userClient(t *testing.T, ts *httptest.Server, email string) *http.Client {
	t.Helper()
	c := newClient(t)
	creds := []byte(`{"email":"` + email + `","password":"correct horse"}`)
	expectStatus(t, post(t, c, ts.URL+"/v1/user/register", "application/json", creds), http.StatusCreated)
	expectStatus(t, post(t, c, ts.URL+"/v1/user/login", "application/json", creds), http.StatusOK)
	return c
}

// newGroupVia creates a group through the API and returns its id.
func newGroupVia(t *testing.T, ts *httptest.Server, c *http.Client, name string) int64 {
	t.Helper()
	code, out := doJSON(t, c, http.MethodPost, ts.URL+"/v1/auth/groups", `{"name":"`+name+`"}`)
	expectStatus(t, code, http.StatusCreated)
	id, ok := out["id"].(float64)
	if !ok || id <= 0 {
		t.Fatalf("bad group response: %v", out)
	}
	return int64(id)
}

func groupURL(ts *httptest.Server, id int64, rest string) string {
	return ts.URL + "/v1/auth/groups/" + strconv.FormatInt(id, 10) + rest
}

func TestGroupsRequireAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	anon := newClient(t)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/v1/auth/groups"},
		{http.MethodPost, "/v1/auth/groups"},
		{http.MethodGet, "/v1/auth/groups/1"},
		{http.MethodDelete, "/v1/auth/groups/1"},
		{http.MethodPost, "/v1/auth/groups/1/members"},
		{http.MethodDelete, "/v1/auth/groups/1/members/1"},
	} {
		code, _ := doJSON(t, anon, tc.method, ts.URL+tc.path, `{}`)
		if code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401", tc.method, tc.path, code)
		}
	}
}

func TestGroupCreateListGet(t *testing.T) {
	ts, s := newTestServer(t)
	a := loggedInClient(t, ts) // user 1
	b := secondUser(t, ts)     // user 2

	id := newGroupVia(t, ts, a, "  Engineering ")
	g, err := s.getGroup(t.Context(), id)
	if err != nil || g.Name != "Engineering" || g.OwnerID != 1 {
		t.Fatalf("group = %+v, %v", g, err)
	}
	if ok, err := s.userInGroup(t.Context(), 1, id); err != nil || !ok {
		t.Fatalf("owner must be a member: %v, %v", ok, err)
	}

	// Names are unique ignoring case; invalid names are rejected.
	url := ts.URL + "/v1/auth/groups"
	code, _ := doJSON(t, b, http.MethodPost, url, `{"name":"engineering"}`)
	expectStatus(t, code, http.StatusConflict)
	for _, body := range []string{`{"name":""}`, `{"name":"   "}`, `{"name":"a\u0000b"}`, `{"name":"` + strings.Repeat("x", 65) + `"}`, `nope`} {
		code, _ = doJSON(t, a, http.MethodPost, url, body)
		if code != http.StatusBadRequest {
			t.Errorf("body %q = %d, want 400", body, code)
		}
	}

	// Listing shows only the caller's groups.
	code, out := doJSON(t, a, http.MethodGet, url, "")
	groups, _ := out["groups"].([]any)
	if code != http.StatusOK || len(groups) != 1 {
		t.Fatalf("a's groups = %d %v", code, out)
	}
	_, out = doJSON(t, b, http.MethodGet, url, "")
	if groups, _ := out["groups"].([]any); groups == nil || len(groups) != 0 {
		t.Fatalf("b's groups = %v, want empty array", out)
	}

	// Members can read the group; others can't, and unknown ids are 404.
	code, out = doJSON(t, a, http.MethodGet, groupURL(ts, id, ""), "")
	members, _ := out["members"].([]any)
	if code != http.StatusOK || len(members) != 1 || out["owner_id"] != float64(1) {
		t.Fatalf("get group = %d %v", code, out)
	}
	code, _ = doJSON(t, b, http.MethodGet, groupURL(ts, id, ""), "")
	expectStatus(t, code, http.StatusForbidden)
	code, _ = doJSON(t, a, http.MethodGet, groupURL(ts, 999, ""), "")
	expectStatus(t, code, http.StatusNotFound)
	code, _ = doJSON(t, a, http.MethodGet, ts.URL+"/v1/auth/groups/abc", "")
	expectStatus(t, code, http.StatusNotFound)
}

func TestGroupMembers(t *testing.T) {
	ts, s := newTestServer(t)
	a := loggedInClient(t, ts) // user 1, owner
	b := secondUser(t, ts)     // user 2
	c := userClient(t, ts, "c@example.com")
	id := newGroupVia(t, ts, a, "eng")
	add := groupURL(ts, id, "/members")

	// Only the owner may add; the outsider and a plain member get 403.
	code, _ := doJSON(t, b, http.MethodPost, add, `{"email":"b@example.com"}`)
	expectStatus(t, code, http.StatusForbidden)
	if ok, _ := s.userInGroup(t.Context(), 2, id); ok {
		t.Fatal("non-owner added themselves")
	}
	code, _ = doJSON(t, a, http.MethodPost, add, `{"email":"b@example.com"}`)
	expectStatus(t, code, http.StatusCreated)
	code, _ = doJSON(t, a, http.MethodPost, add, `{"email":"b@example.com"}`)
	expectStatus(t, code, http.StatusOK) // idempotent
	code, _ = doJSON(t, b, http.MethodPost, add, `{"email":"c@example.com"}`)
	expectStatus(t, code, http.StatusForbidden)
	code, _ = doJSON(t, a, http.MethodPost, add, `{"email":"nobody@example.com"}`)
	expectStatus(t, code, http.StatusNotFound)
	code, _ = doJSON(t, a, http.MethodPost, add, `{}`)
	expectStatus(t, code, http.StatusBadRequest)
	code, _ = doJSON(t, a, http.MethodPost, groupURL(ts, 999, "/members"), `{"email":"b@example.com"}`)
	expectStatus(t, code, http.StatusNotFound)

	members, err := s.groupMembers(t.Context(), id)
	if err != nil || len(members) != 2 || members[0].Email != "a@example.com" || members[1].Email != "b@example.com" {
		t.Fatalf("members = %+v, %v", members, err)
	}
	// A member can now read the group; the outsider still can't.
	code, _ = doJSON(t, b, http.MethodGet, groupURL(ts, id, ""), "")
	expectStatus(t, code, http.StatusOK)
	code, _ = doJSON(t, c, http.MethodGet, groupURL(ts, id, ""), "")
	expectStatus(t, code, http.StatusForbidden)
	if gs, err := s.userGroups(t.Context(), 2); err != nil || len(gs) != 1 || gs[0].ID != id {
		t.Fatalf("userGroups(2) = %+v, %v", gs, err)
	}

	// Removal: the owner can't be removed, a member can't remove others, the outsider
	// can't remove anyone, and the owner can remove a member.
	code, _ = doJSON(t, a, http.MethodDelete, groupURL(ts, id, "/members/1"), "")
	expectStatus(t, code, http.StatusBadRequest)
	code, _ = doJSON(t, b, http.MethodDelete, groupURL(ts, id, "/members/1"), "")
	expectStatus(t, code, http.StatusBadRequest)
	code, _ = doJSON(t, c, http.MethodDelete, groupURL(ts, id, "/members/2"), "")
	expectStatus(t, code, http.StatusForbidden)
	code, _ = doJSON(t, a, http.MethodPost, add, `{"email":"c@example.com"}`)
	expectStatus(t, code, http.StatusCreated)
	code, _ = doJSON(t, b, http.MethodDelete, groupURL(ts, id, "/members/3"), "")
	expectStatus(t, code, http.StatusForbidden)
	if ok, _ := s.userInGroup(t.Context(), 3, id); !ok {
		t.Fatal("a plain member removed someone else")
	}
	code, _ = doJSON(t, a, http.MethodDelete, groupURL(ts, id, "/members/3"), "")
	expectStatus(t, code, http.StatusOK)
	code, _ = doJSON(t, a, http.MethodDelete, groupURL(ts, id, "/members/3"), "")
	expectStatus(t, code, http.StatusNotFound)
	// A member can leave.
	code, _ = doJSON(t, b, http.MethodDelete, groupURL(ts, id, "/members/2"), "")
	expectStatus(t, code, http.StatusOK)
	if ok, _ := s.userInGroup(t.Context(), 2, id); ok {
		t.Fatal("member did not leave")
	}
	code, _ = doJSON(t, a, http.MethodDelete, groupURL(ts, 999, "/members/2"), "")
	expectStatus(t, code, http.StatusNotFound)
}

func TestGroupDelete(t *testing.T) {
	ts, s := newTestServer(t)
	a := loggedInClient(t, ts)
	b := secondUser(t, ts)
	id := newGroupVia(t, ts, a, "eng")
	code, _ := doJSON(t, a, http.MethodPost, groupURL(ts, id, "/members"), `{"email":"b@example.com"}`)
	expectStatus(t, code, http.StatusCreated)

	// Non-owners, members included, can't delete.
	for _, c := range []*http.Client{b, userClient(t, ts, "c@example.com")} {
		code, _ = doJSON(t, c, http.MethodDelete, groupURL(ts, id, ""), "")
		expectStatus(t, code, http.StatusForbidden)
	}
	if _, err := s.getGroup(t.Context(), id); err != nil {
		t.Fatalf("group deleted by non-owner: %v", err)
	}

	// Refused while a site uses it; the site keeps its group.
	code, _ = doJSON(t, a, http.MethodPost, ts.URL+"/v1/auth/sites", fmt.Sprintf(`{"name":"blog","group":%d}`, id))
	expectStatus(t, code, http.StatusCreated)
	code, out := doJSON(t, a, http.MethodDelete, groupURL(ts, id, ""), "")
	expectStatus(t, code, http.StatusConflict)
	if site, _ := s.getSite(t.Context(), "blog"); site.GroupID != id {
		t.Fatalf("site lost its group: %+v (%v)", site, out)
	}
	if members, _ := s.groupMembers(t.Context(), id); len(members) != 2 {
		t.Fatalf("members changed by refused delete: %+v", members)
	}

	// Once the site lets go, the owner can delete; memberships go too.
	code, _ = doJSON(t, a, http.MethodPut, ts.URL+"/v1/auth/sites/blog", `{"group":0}`)
	expectStatus(t, code, http.StatusOK)
	code, _ = doJSON(t, a, http.MethodDelete, groupURL(ts, id, ""), "")
	expectStatus(t, code, http.StatusOK)
	code, _ = doJSON(t, a, http.MethodDelete, groupURL(ts, id, ""), "")
	expectStatus(t, code, http.StatusNotFound)
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM user_group WHERE gid = ?", id).Scan(&n); err != nil || n != 0 {
		t.Fatalf("memberships left: %d, %v", n, err)
	}
	if ok, err := s.userInGroup(t.Context(), 2, id); err != nil || ok {
		t.Fatalf("userInGroup after delete = %v, %v", ok, err)
	}
	// The name is free again.
	newGroupVia(t, ts, b, "eng")
}

func TestOwnerlessGroupIsReadOnly(t *testing.T) {
	ts, s := newTestServer(t)
	a := loggedInClient(t, ts)
	if _, err := s.db.Exec("INSERT INTO groups (groupid, name) VALUES (5, 'legacy')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO user_group (uid, gid) VALUES (1, 5)"); err != nil {
		t.Fatal(err)
	}
	code, _ := doJSON(t, a, http.MethodDelete, groupURL(ts, 5, ""), "")
	expectStatus(t, code, http.StatusForbidden)
	code, _ = doJSON(t, a, http.MethodPost, groupURL(ts, 5, "/members"), `{"email":"a@example.com"}`)
	expectStatus(t, code, http.StatusForbidden)
	code, _ = doJSON(t, a, http.MethodDelete, groupURL(ts, 5, "/members/2"), "")
	expectStatus(t, code, http.StatusForbidden)
	// A member can still leave, and sees the group with a null owner.
	code, out := doJSON(t, a, http.MethodGet, groupURL(ts, 5, ""), "")
	if code != http.StatusOK || out["owner_id"] != nil {
		t.Fatalf("get legacy group = %d %v", code, out)
	}
	code, _ = doJSON(t, a, http.MethodDelete, groupURL(ts, 5, "/members/1"), "")
	expectStatus(t, code, http.StatusOK)
}

func TestMigrateOldSchema(t *testing.T) {
	path := t.TempDir() + "/old.db"
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	old := []string{
		"CREATE TABLE user (userid INTEGER PRIMARY KEY, email VARCHAR(255) NOT NULL, password BINARY(60) NOT NULL)",
		"CREATE TABLE groups (groupid INTEGER PRIMARY KEY, name VARCHAR(255) NOT NULL)",
		"CREATE TABLE user_group (ugid INTEGER PRIMARY KEY, uid INTEGER NOT NULL, gid INTEGER NOT NULL)",
		"INSERT INTO groups (groupid, name) VALUES (1, 'eng')",
		"INSERT INTO user_group (uid, gid) VALUES (1, 1), (1, 1), (2, 1)",
	}
	for _, q := range old {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	// Opening twice proves the migration is idempotent.
	for range 2 {
		db, err = openDB(path)
		if err != nil {
			t.Fatal(err)
		}
		var owners, members int
		if err := db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('groups') WHERE name = 'owner'").Scan(&owners); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow("SELECT COUNT(*) FROM user_group").Scan(&members); err != nil {
			t.Fatal(err)
		}
		if owners != 1 || members != 2 {
			t.Fatalf("owner columns = %d, memberships = %d; want 1 and 2", owners, members)
		}
		db.Close()
	}
}

func TestConcurrentCreateSameName(t *testing.T) {
	_, s := newTestServer(t)
	for _, email := range []string{"a@example.com", "b@example.com", "c@example.com", "d@example.com"} {
		if _, err := s.db.Exec("INSERT INTO user (email, password) VALUES (?, 'x')", email); err != nil {
			t.Fatal(err)
		}
	}
	const n = 4
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.createGroup(t.Context(), "eng", int64(i+1))
		}()
	}
	wg.Wait()
	created := 0
	for _, err := range errs {
		switch {
		case err == nil:
			created++
		case !errors.Is(err, errGroupExists):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if created != 1 {
		t.Fatalf("%d creators won, want exactly 1", created)
	}
}

func TestConcurrentAddMember(t *testing.T) {
	_, s := newTestServer(t)
	for _, email := range []string{"a@example.com", "b@example.com"} {
		if _, err := s.db.Exec("INSERT INTO user (email, password) VALUES (?, 'x')", email); err != nil {
			t.Fatal(err)
		}
	}
	g, err := s.createGroup(t.Context(), "eng", 1)
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	added := make([]bool, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			added[i], errs[i] = s.addGroupMember(t.Context(), 1, g.ID, "b@example.com")
		}()
	}
	wg.Wait()
	wins := 0
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("add %d: %v", i, errs[i])
		}
		if added[i] {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("%d adds reported success, want 1", wins)
	}
	if members, _ := s.groupMembers(t.Context(), g.ID); len(members) != 2 {
		t.Fatalf("members = %+v", members)
	}
}

// A group must never be deleted while a site refers to it, however the delete and the
// site update interleave.
func TestConcurrentDeleteVersusSiteAssign(t *testing.T) {
	_, s := newTestServer(t)
	if _, err := s.db.Exec("INSERT INTO user (email, password) VALUES ('a@example.com', 'x')"); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		g, err := s.createGroup(t.Context(), fmt.Sprintf("g%d", i), 1)
		if err != nil {
			t.Fatal(err)
		}
		site, err := s.createSite(t.Context(), fmt.Sprintf("s%d", i), "", 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = s.deleteGroup(t.Context(), 1, g.ID)
		}()
		go func() {
			defer wg.Done()
			_, _, _ = s.updateSite(t.Context(), site.ID, nil, &g.ID)
		}()
		wg.Wait()

		got, err := s.getSite(t.Context(), site.Name)
		if err != nil {
			t.Fatal(err)
		}
		if got.GroupID != 0 {
			if _, err := s.getGroup(t.Context(), got.GroupID); err != nil {
				t.Fatalf("round %d: site refers to a deleted group: %v", i, err)
			}
		}
	}
}

func TestUserInfoShowsGroupOwner(t *testing.T) {
	ts, _ := newTestServer(t)
	a := loggedInClient(t, ts)
	newGroupVia(t, ts, a, "eng")
	_, out := doJSON(t, a, http.MethodGet, ts.URL+"/v1/auth/user", "")
	groups, _ := out["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("groups = %v", out["groups"])
	}
	if g, _ := groups[0].(map[string]any); g["owner_id"] != float64(1) || g["name"] != "eng" {
		t.Fatalf("group = %v", groups[0])
	}
}
