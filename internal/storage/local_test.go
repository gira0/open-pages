package storage_test

import (
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
