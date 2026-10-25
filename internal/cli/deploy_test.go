package cli

import (
	"archive/zip"
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestDeployDoesNotFollowRedirects(t *testing.T) {
	var leaked atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(target.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)

	dir := writeTree(t, map[string]string{"index.html": "hi"})
	err := DeployCommand(context.Background(), []string{"blog", dir}, env("opt_secret", origin.URL), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "redirect") {
		t.Fatalf("got error %v, want a redirect failure", err)
	}
	if n := leaked.Load(); n != 0 {
		t.Fatalf("the redirect target received %d requests carrying the token", n)
	}
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func zipNames(t *testing.T, data []byte) []string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	slices.Sort(names)
	return names
}

func TestZipDir(t *testing.T) {
	dir := writeTree(t, map[string]string{"index.html": "hi", "css/site.css": "body{}", ".well-known/x": "y"})
	var buf bytes.Buffer
	if err := zipDir(&buf, dir); err != nil {
		t.Fatal(err)
	}
	want := []string{".well-known/", ".well-known/x", "css/", "css/site.css", "index.html"}
	if got := zipNames(t, buf.Bytes()); !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestZipDirRejects(t *testing.T) {
	if err := zipDir(&bytes.Buffer{}, t.TempDir()); err == nil {
		t.Error("empty directory was accepted")
	}
	if err := zipDir(&bytes.Buffer{}, filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing directory was accepted")
	}
	if runtime.GOOS == "windows" {
		return
	}
	dir := writeTree(t, map[string]string{"index.html": "hi"})
	secret := writeTree(t, map[string]string{"secret.txt": "s3cret"})
	if err := os.Symlink(filepath.Join(secret, "secret.txt"), filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := zipDir(&buf, dir); err == nil || strings.Contains(buf.String(), "s3cret") {
		t.Fatalf("symlink was not rejected: %v", err)
	}
}

func env(token, server string) func(string) string {
	return func(k string) string {
		switch k {
		case envToken:
			return token
		case envServerURL:
			return server
		}
		return ""
	}
}
