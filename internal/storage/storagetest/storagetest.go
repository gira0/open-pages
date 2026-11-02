// Package storagetest is the conformance suite every storage.Store backend must pass.
package storagetest

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gira0/open-pages/internal/storage"
)

// Run exercises a freshly made, empty store.
func Run(t *testing.T, newStore func(t *testing.T) storage.Store) {
	ctx := context.Background()
	const v1, v2 = "aaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbb"

	publish := func(t *testing.T, s storage.Store, site, version string, files map[string]string) {
		t.Helper()
		dir, cleanup, err := s.Staging(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()
		for name, body := range files {
			p := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Publish(ctx, site, version, dir); err != nil {
			t.Fatal(err)
		}
	}
	read := func(t *testing.T, r storage.Root, name string) string {
		t.Helper()
		f, err := r.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		b, _ := io.ReadAll(f)
		return string(b)
	}

	t.Run("empty site", func(t *testing.T) {
		s := newStore(t)
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		if err := s.Check(ctx); err != nil {
			t.Fatal(err)
		}
		if vs, err := s.Versions(ctx, "blog"); err != nil || len(vs) != 0 {
			t.Fatalf("Versions = %v, %v", vs, err)
		}
		if cur, err := s.Current(ctx, "blog"); err != nil || cur != "" {
			t.Fatalf("Current = %q, %v", cur, err)
		}
		if _, _, err := s.OpenCurrent(ctx, "blog"); !errors.Is(err, storage.ErrNotDeployed) {
			t.Fatalf("OpenCurrent = %v, want ErrNotDeployed", err)
		}
		if err := s.SetCurrent(ctx, "blog", v1); !errors.Is(err, storage.ErrVersionAbsent) {
			t.Fatalf("SetCurrent of a missing version = %v", err)
		}
	})

	t.Run("publish switch and rollback", func(t *testing.T) {
		s := newStore(t)
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		publish(t, s, "blog", v1, map[string]string{"index.html": "one", "css/a.css": "abc"})
		if cur, _ := s.Current(ctx, "blog"); cur != "" {
			t.Fatalf("published version is live before SetCurrent: %q", cur)
		}
		if err := s.SetCurrent(ctx, "blog", v1); err != nil {
			t.Fatal(err)
		}
		old, ver, err := s.OpenCurrent(ctx, "blog")
		if err != nil || ver != v1 {
			t.Fatalf("OpenCurrent = %q, %v", ver, err)
		}
		defer old.Close()

		publish(t, s, "blog", v2, map[string]string{"index.html": "two"})
		if err := s.SetCurrent(ctx, "blog", v2); err != nil {
			t.Fatal(err)
		}
		if got := read(t, old, "index.html"); got != "one" {
			t.Fatalf("open root changed under the reader: %q", got)
		}
		now, ver, err := s.OpenCurrent(ctx, "blog")
		if err != nil || ver != v2 {
			t.Fatalf("OpenCurrent = %q, %v", ver, err)
		}
		defer now.Close()
		if got := read(t, now, "index.html"); got != "two" {
			t.Fatalf("index.html = %q", got)
		}
		if _, err := now.Open("css/a.css"); err == nil {
			t.Fatal("file of the old version visible in the new one")
		}
		if vs, _ := s.Versions(ctx, "blog"); !slices.Equal(vs, []string{v1, v2}) {
			t.Fatalf("Versions = %v", vs)
		}
		if n, err := s.VersionSize(ctx, "blog", v1); err != nil || n != 6 {
			t.Fatalf("VersionSize = %d, %v, want 6", n, err)
		}
		if err := s.SetCurrent(ctx, "blog", v1); err != nil { // rollback
			t.Fatal(err)
		}
		if cur, _ := s.Current(ctx, "blog"); cur != v1 {
			t.Fatalf("Current after rollback = %q", cur)
		}
	})

	t.Run("delete", func(t *testing.T) {
		s := newStore(t)
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		publish(t, s, "blog", v1, map[string]string{"index.html": "one"})
		publish(t, s, "blog", v2, map[string]string{"index.html": "two"})
		if err := s.SetCurrent(ctx, "blog", v2); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteVersion(ctx, "blog", v1); err != nil {
			t.Fatal(err)
		}
		if err := s.DeleteVersion(ctx, "blog", v1); err != nil {
			t.Fatalf("deleting a missing version: %v", err)
		}
		if vs, _ := s.Versions(ctx, "blog"); !slices.Equal(vs, []string{v2}) {
			t.Fatalf("Versions = %v", vs)
		}
		if err := s.DeleteSite(ctx, "blog"); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.OpenCurrent(ctx, "blog"); !errors.Is(err, storage.ErrNotDeployed) {
			t.Fatalf("OpenCurrent after delete = %v", err)
		}
		if err := s.DeleteSite(ctx, "blog"); err != nil {
			t.Fatalf("deleting a missing site: %v", err)
		}
	})

	t.Run("invalid names", func(t *testing.T) {
		s := newStore(t)
		if err := s.Init(ctx); err != nil {
			t.Fatal(err)
		}
		for _, site := range []string{"", "../etc", "A", ".staging-x"} {
			if _, err := s.Versions(ctx, site); !errors.Is(err, storage.ErrInvalid) {
				t.Errorf("Versions(%q) = %v, want ErrInvalid", site, err)
			}
		}
		if err := s.SetCurrent(ctx, "blog", "../x"); !errors.Is(err, storage.ErrVersionAbsent) {
			t.Errorf("SetCurrent with a bad version = %v", err)
		}
	})
}
