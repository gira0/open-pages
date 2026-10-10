package main

import (
	"errors"
	"os"
	"testing"
)

// ensureBlogSite registers the site "blog", owned by the test owner, unless it already exists.
func ensureBlogSite(t *testing.T, s *Server) {
	t.Helper()
	if _, err := s.getSite(t.Context(), "blog"); err == nil {
		return
	}
	if _, err := s.createSite(t.Context(), "blog", "", testOwner(t, s), 0, visPublic); err != nil {
		t.Fatal(err)
	}
}

// deployNamed deploys src to the existing site called name.
func deployNamed(t *testing.T, s *Server, name string, src *os.File) (string, error) {
	site, err := s.getSite(t.Context(), name)
	if err != nil {
		return "", err
	}
	return s.deploy(t.Context(), site, src)
}

// An upload authorized before a delete must not bring the site back afterwards, even
// if the name is registered again by someone else.
func TestDeployAfterDeleteIsRejected(t *testing.T) {
	_, s := newTestServer(t)
	ensureBlogSite(t, s)
	stale, err := s.getSite(t.Context(), "blog")
	if err != nil {
		t.Fatal(err)
	}
	deployFiles(t, s, map[string]string{"index.html": "mine"})

	if err := s.deleteSite(t.Context(), stale); err != nil {
		t.Fatal(err)
	}
	if err := s.deleteSite(t.Context(), stale); !errors.Is(err, errSiteNotFound) {
		t.Fatalf("second delete = %v, want errSiteNotFound", err)
	}
	late := map[string]string{"index.html": "late"}
	if _, err := s.deploy(t.Context(), stale, writeZip(t, late)); !errors.Is(err, errSiteNotFound) {
		t.Fatalf("deploy after delete = %v, want errSiteNotFound", err)
	}
	if _, err := os.Lstat(s.SiteDir("blog")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("site dir exists after delete + stale deploy: %v", err)
	}

	// A new owner takes the name; the old owner's stale upload still must not land.
	if _, err := s.db.ExecContext(t.Context(),
		"INSERT OR IGNORE INTO user (userid, email, password) VALUES (2, 'two@example.com', 'x')"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.createSite(t.Context(), "blog", "", 2, 0, visPublic); err != nil {
		t.Fatal(err)
	}
	if _, err := s.deploy(t.Context(), stale, writeZip(t, late)); !errors.Is(err, errSiteNotFound) {
		t.Fatalf("stale deploy into reused name = %v, want errSiteNotFound", err)
	}
	if v, _ := s.Versions("blog"); len(v) != 0 {
		t.Fatalf("stale deploy created versions %v", v)
	}
}

// Updates touch only the fields they carry, so one never reverts the other.
func TestUpdateSiteWritesOnlyGivenColumns(t *testing.T) {
	_, s := newTestServer(t)
	owner := testOwner(t, s)
	if _, err := s.db.ExecContext(t.Context(), "INSERT INTO groups (groupid, name, owner) VALUES (7, 'eng', ?)", owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(t.Context(), "INSERT INTO user_group (uid, gid) VALUES (?, 7)", owner); err != nil {
		t.Fatal(err)
	}
	ensureBlogSite(t, s)
	site, err := s.getSite(t.Context(), "blog")
	if err != nil {
		t.Fatal(err)
	}

	desc, grp := "hello", int64(7)
	restricted := visRestricted
	if _, err := s.updateSite(t.Context(), site, &desc, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.updateSite(t.Context(), site, nil, &grp, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.updateSite(t.Context(), site, nil, nil, &restricted); err != nil {
		t.Fatal(err)
	}
	got, err := s.updateSite(t.Context(), site, nil, nil, nil)
	if err != nil || got.Description != "hello" || got.GroupID != 7 || got.Visibility != visRestricted {
		t.Fatalf("after partial updates: %+v %v", got, err)
	}
	zero := int64(0)
	if got, _ = s.updateSite(t.Context(), site, nil, &zero, nil); got.GroupID != 0 {
		t.Fatalf("group not cleared: %d", got.GroupID)
	}
	gone := site
	gone.ID += 100
	if _, err := s.updateSite(t.Context(), gone, &desc, nil, nil); !errors.Is(err, errSiteNotFound) {
		t.Fatalf("update of missing site = %v", err)
	}
}

func TestVersionsAndCurrentSnapshot(t *testing.T) {
	_, s := newTestServer(t)
	v1 := deployFiles(t, s, map[string]string{"index.html": "1"})
	versions, current, err := s.versionsAndCurrent("blog")
	if err != nil || current != v1 || len(versions) != 1 || versions[0] != v1 {
		t.Fatalf("snapshot = %v %q %v", versions, current, err)
	}
}
