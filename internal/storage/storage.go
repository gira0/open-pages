// Package storage holds the bytes of deployed sites. The server decides which sites exist
// and who may see them (that is the database's job); a Store only keeps versions of a
// site's files, knows which version is current, and opens files of the current version.
//
// Callers serialize writes per store (the server's deploy lock). Reads may run at any
// time and must see either the old or the new current version, never a partial one.
package storage

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"regexp"
)

var (
	// ErrVersionAbsent means the version does not exist in the store.
	ErrVersionAbsent = errors.New("version not found")
	// ErrNotDeployed means the site has no current version.
	ErrNotDeployed = errors.New("site has no current version")
	// ErrInvalid means a site name or version id is malformed.
	ErrInvalid = errors.New("invalid site name or version")
)

// VersionRe matches version ids, which are xids: time-sortable, so lexical order is
// deploy order.
var VersionRe = regexp.MustCompile(`^[0-9a-v]{20}$`)

// File is an open file of a site.
type File interface {
	io.Reader
	io.Seeker
	io.Closer
	Stat() (fs.FileInfo, error)
}

// Root gives read access to the files of one version of one site. Names are slash
// separated, cleaned and relative ("." for the root). An implementation must refuse
// anything that leads outside the version (symlinks included) with an error.
type Root interface {
	Open(name string) (File, error)
	Close() error
}

// Store keeps the versions of every site.
type Store interface {
	// Init prepares the store for use (creates directories, checks the connection).
	Init(ctx context.Context) error
	// Check reports whether the store can currently be written; it backs /readyz.
	Check(ctx context.Context) error

	// Staging returns a new empty local directory to extract an archive into, and a
	// function that removes it (safe to call after Publish). It is always on local
	// disk, whatever the backend, because extraction needs a real file system.
	Staging(ctx context.Context) (dir string, cleanup func(), err error)
	// Publish moves the staged directory into the store as a new version of site. The
	// version is not live until SetCurrent. Staging is consumed on success.
	Publish(ctx context.Context, site, version, stagingDir string) error

	// Versions lists the site's versions, oldest first.
	Versions(ctx context.Context, site string) ([]string, error)
	// Current returns the live version, or "" if the site was never deployed.
	Current(ctx context.Context, site string) (string, error)
	// SetCurrent atomically makes version live. It returns ErrVersionAbsent if the
	// version does not exist.
	SetCurrent(ctx context.Context, site, version string) error
	// VersionSize returns the total bytes of the version's files.
	VersionSize(ctx context.Context, site, version string) (int64, error)

	// DeleteVersion removes one version. Removing a missing one is not an error.
	DeleteVersion(ctx context.Context, site, version string) error
	// DeleteSite removes every version and the current pointer.
	DeleteSite(ctx context.Context, site string) error

	// OpenCurrent opens the live version for reading. It returns ErrNotDeployed if the
	// site has none. The Root keeps showing the version it opened even if a deploy
	// switches the pointer meanwhile.
	OpenCurrent(ctx context.Context, site string) (Root, string, error)
}
