package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestUserInfo(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts) // a@example.com, userid 1
	other := newClient(t)
	creds := []byte(`{"email":"b@example.com","password":"correct horse"}`)
	expectStatus(t, post(t, other, ts.URL+"/v1/user/register", "application/json", creds), http.StatusCreated)

	mustExec := func(q string) {
		t.Helper()
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	mustExec("INSERT INTO groups (groupid, name) VALUES (1, 'eng'), (2, 'ops')")
	mustExec("INSERT INTO user_group (uid, gid) VALUES (1, 1)")
	mustExec("INSERT INTO docs (uowner, ugroup, name, description) VALUES (1, NULL, 'mine', 'my doc')")
	mustExec("INSERT INTO docs (uowner, ugroup, name) VALUES (2, 1, 'shared')")
	mustExec("INSERT INTO docs (uowner, ugroup, name) VALUES (2, 2, 'hidden')")
	mustExec("INSERT INTO docs (uowner, ugroup, name) VALUES (NULL, NULL, 'orphan')")

	resp, err := c.Get(ts.URL + "/v1/auth/user")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	expectStatus(t, resp.StatusCode, http.StatusOK)
	var raw map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(raw)
	if strings.Contains(strings.ToLower(string(b)), "password") || strings.Contains(string(b), "$2a$") {
		t.Fatalf("response leaks credentials: %s", b)
	}
	var got userInfo
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.UserID != 1 || got.Email != "a@example.com" {
		t.Fatalf("bad account data: %+v", got)
	}
	if len(got.Groups) != 1 || got.Groups[0].Name != "eng" {
		t.Fatalf("groups = %+v", got.Groups)
	}
	if len(got.Docs.Owned) != 1 || got.Docs.Owned[0].Name != "mine" || got.Docs.Owned[0].Description != "my doc" {
		t.Fatalf("owned = %+v", got.Docs.Owned)
	}
	var names []string
	for _, d := range got.Docs.Viewable {
		names = append(names, d.Name)
	}
	if strings.Join(names, ",") != "mine,shared" {
		t.Fatalf("viewable = %v", names)
	}
}

func TestUserInfoEmptyListsAreArrays(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts)
	resp, err := c.Get(ts.URL + "/v1/auth/user")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var raw struct {
		Groups []any `json:"groups"`
		Docs   struct {
			Owned    []any `json:"owned"`
			Viewable []any `json:"viewable"`
		} `json:"docs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if raw.Groups == nil || raw.Docs.Owned == nil || raw.Docs.Viewable == nil {
		t.Fatalf("expected empty arrays, got %+v", raw)
	}
}
