package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestConcurrentDeploysKeepOne(t *testing.T) {
	_, s := newTestServer(t)
	s.cfg.KeepVersions = 1

	const n = 8
	archives := make([]*os.File, n)
	for i := range archives {
		f, err := os.CreateTemp(t.TempDir(), "a-*")
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.Write(zipArchive(t, map[string]string{"index.html": "x"})); err != nil {
			t.Fatal(err)
		}
		archives[i] = f
	}

	var wg sync.WaitGroup
	errs := make([]error, n)
	for i, f := range archives {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.deploy("blog", f)
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("deploy %d: %v", i, err)
		}
	}
	versions, err := s.Versions("blog")
	if err != nil || len(versions) != 1 {
		t.Fatalf("versions = %v, %v; want exactly 1", versions, err)
	}
	if cur, _ := s.CurrentVersion("blog"); cur != versions[0] {
		t.Fatalf("current = %q, versions = %v", cur, versions)
	}
}

func TestValidSiteName(t *testing.T) {
	good := []string{"a", "blog", "my-site", "a1", "0day", strings.Repeat("a", 63)}
	bad := []string{"", "-a", "a-", "My", "a_b", "a.b", "../x", "a/b", "ü", " a", strings.Repeat("a", 64)}
	for _, n := range good {
		if !validSiteName(n) {
			t.Errorf("%q should be valid", n)
		}
	}
	for _, n := range bad {
		if validSiteName(n) {
			t.Errorf("%q should be invalid", n)
		}
	}
	srv := &Server{sites: "/x"}
	if srv.CurrentDir("../etc") != "" || srv.SiteDir("A") != "" {
		t.Error("invalid names must not map to a path")
	}
}

func TestSiteCreate(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts)
	url := ts.URL + "/v1/auth/sites"

	expectStatus(t, post(t, c, url, "application/json", []byte(`{"name":"handbook","description":"Team handbook"}`)), http.StatusCreated)
	expectStatus(t, post(t, c, url, "application/json", []byte(`{"name":"handbook"}`)), http.StatusConflict)
	for _, name := range []string{"../bad", "Upper", "-x", "has_underscore", ""} {
		expectStatus(t, post(t, c, url, "application/json", []byte(`{"name":"`+name+`"}`)), http.StatusBadRequest)
	}
	expectStatus(t, post(t, newClient(t), url, "application/json", []byte(`{"name":"anon"}`)), http.StatusUnauthorized)
}

func TestUploadSiteChecks(t *testing.T) {
	ts, s := newTestServer(t)
	owner := loggedInClient(t, ts)
	createSiteVia(t, ts, owner, "blog")
	zipBody := zipArchive(t, map[string]string{"index.html": "x"})

	expectStatus(t, post(t, owner, ts.URL+"/v1/auth/sites/missing/upload", "application/zip", zipBody), http.StatusNotFound)
	expectStatus(t, post(t, owner, ts.URL+"/v1/auth/sites/Bad_Name/upload", "application/zip", zipBody), http.StatusNotFound)

	// A second user may not deploy to someone else's site.
	other := newClient(t)
	creds := []byte(`{"email":"b@example.com","password":"correct horse"}`)
	expectStatus(t, post(t, other, ts.URL+"/v1/user/register", "application/json", creds), http.StatusCreated)
	expectStatus(t, post(t, other, ts.URL+"/v1/user/login", "application/json", creds), http.StatusOK)
	expectStatus(t, post(t, other, ts.URL+"/v1/auth/sites/blog/upload", "application/zip", zipBody), http.StatusForbidden)

	if v, _ := s.Versions("blog"); len(v) != 0 {
		t.Fatalf("rejected uploads created versions %v", v)
	}
}

func deployFiles(t *testing.T, s *Server, site string, files map[string]string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "a-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(zipArchive(t, files)); err != nil {
		t.Fatal(err)
	}
	v, err := s.deploy(site, f)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestDeployVersionsAndPrune(t *testing.T) {
	_, s := newTestServer(t)
	s.cfg.KeepVersions = 3

	var made []string
	for _, body := range []string{"v1", "v2", "v3", "v4", "v5"} {
		v := deployFiles(t, s, "blog", map[string]string{"index.html": body})
		made = append(made, v)

		got, err := os.ReadFile(filepath.Join(s.CurrentDir("blog"), "index.html"))
		if err != nil || string(got) != body {
			t.Fatalf("after deploying %s, current = %q, %v", body, got, err)
		}
		if cur, _ := s.CurrentVersion("blog"); cur != v {
			t.Fatalf("CurrentVersion = %q, want %q", cur, v)
		}
	}

	versions, err := s.Versions("blog")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 3 || versions[0] != made[2] || versions[2] != made[4] {
		t.Fatalf("versions = %v, want the newest 3 of %v", versions, made)
	}

	// Rolling back to a kept version works; a pruned or bogus one does not.
	if err := s.SwitchCurrent("blog", made[2]); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(s.CurrentDir("blog"), "index.html")); string(got) != "v3" {
		t.Fatalf("after rollback current = %q", got)
	}
	for _, bad := range []string{made[0], "../../etc", ""} {
		if err := s.SwitchCurrent("blog", bad); err == nil {
			t.Errorf("SwitchCurrent(%q) succeeded", bad)
		}
	}

	// Pruning never removes the current version, even when it is the oldest.
	if err := s.SwitchCurrent("blog", made[2]); err != nil {
		t.Fatal(err)
	}
	s.cfg.KeepVersions = 1
	s.deployMu.Lock()
	err = s.pruneVersions("blog")
	s.deployMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	versions, _ = s.Versions("blog")
	if cur, _ := s.CurrentVersion("blog"); cur != made[2] || !strings.Contains(strings.Join(versions, ","), made[2]) {
		t.Fatalf("pruning removed the current version: cur=%q versions=%v", cur, versions)
	}
}

func TestCurrentNeverMissingDuringDeploy(t *testing.T) {
	_, s := newTestServer(t)
	deployFiles(t, s, "blog", map[string]string{"index.html": "first"})

	done := make(chan struct{})
	errs := make(chan error, 1)
	go func() {
		defer close(done)
		for range 200 {
			got, err := os.ReadFile(filepath.Join(s.CurrentDir("blog"), "index.html"))
			if err != nil || (string(got) != "first" && string(got) != "second") {
				errs <- err
				return
			}
		}
	}()
	deployFiles(t, s, "blog", map[string]string{"index.html": "second"})
	<-done
	select {
	case err := <-errs:
		t.Fatalf("reader saw a missing or partial site: %v", err)
	default:
	}
}

func TestKeepVersionsConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.ini")
	if err := os.WriteFile(path, []byte("[sites]\nkeep_versions = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil || cfg.KeepVersions != 1 {
		t.Fatalf("KeepVersions = %d, %v; want clamp to 1", cfg.KeepVersions, err)
	}
}
