package storage_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gira0/open-pages/internal/storage"
	"github.com/gira0/open-pages/internal/storage/storagetest"
)

func TestLocal(t *testing.T) {
	storagetest.Run(t, func(t *testing.T) storage.Store {
		return storage.NewLocal(filepath.Join(t.TempDir(), "sites"))
	})
}

// A symlink inside a site must not lead out of it.
func TestLocalRefusesEscapingSymlink(t *testing.T) {
	ctx := t.Context()
	l := storage.NewLocal(filepath.Join(t.TempDir(), "sites"))
	if err := l.Init(ctx); err != nil {
		t.Fatal(err)
	}
	dir, cleanup, err := l.Staging(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	const v = "aaaaaaaaaaaaaaaaaaaa"
	if err := l.Publish(ctx, "blog", v, dir); err != nil {
		t.Fatal(err)
	}
	if err := l.SetCurrent(ctx, "blog", v); err != nil {
		t.Fatal(err)
	}
	root, _, err := l.OpenCurrent(ctx, "blog")
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if f, err := root.Open("link"); err == nil {
		f.Close()
		t.Fatal("opened a file outside the site through a symlink")
	}
}
