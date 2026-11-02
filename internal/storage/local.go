package storage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/gira0/open-pages/internal/names"
	"github.com/rs/xid"
)

const (
	versionsDir = "versions"
	currentLink = "current"
)

// Local keeps sites on the local file system:
//
//	<root>/<site>/versions/<version>/   one extracted upload per deploy
//	<root>/<site>/current               symlink to versions/<version>
//
// Publishing renames a staging directory into versions/ and switching replaces the
// symlink with a rename, so readers see either the old or the new version, never a
// partial one. Staging directories live in root itself so the rename stays on one file
// system; site names never start with a dot.
type Local struct {
	root string

	// RemoveAll deletes a directory tree. Tests replace it to simulate a failing delete.
	RemoveAll func(string) error
}

// NewLocal returns a store rooted at dir.
func NewLocal(dir string) *Local { return &Local{root: dir, RemoveAll: os.RemoveAll} }

// Root returns the directory holding the sites.
func (l *Local) Root() string { return l.root }

// SiteDir returns the directory of a site, or "" for an invalid name.
func (l *Local) SiteDir(site string) string {
	if !names.ValidSite(site) {
		return ""
	}
	return filepath.Join(l.root, site)
}

// CurrentDir returns the path of the live content (a symlink to the current version), or
// "" for an invalid name. It exists once the site has been deployed.
func (l *Local) CurrentDir(site string) string {
	dir := l.SiteDir(site)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, currentLink)
}

// Init creates the sites directory and repairs the group permissions of an older volume.
func (l *Local) Init(context.Context) error {
	// Sites are world-readable so a reverse proxy can serve them.
	if err := os.MkdirAll(l.root, 0o775); err != nil { //nolint:gosec // G301: public site content, group-writable for arbitrary UIDs in group 0
		return fmt.Errorf("create %s: %w", l.root, err)
	}
	GroupWritableDirs(l.root)
	return nil
}

// Check proves the sites directory accepts new files.
func (l *Local) Check(context.Context) error { return checkWritable(l.root) }

func checkWritable(dir string) error {
	f, err := os.CreateTemp(dir, ".healthz-*")
	if err != nil {
		return err
	}
	return errors.Join(f.Close(), os.Remove(f.Name()))
}

// Staging creates an empty staging directory in the sites root, on the same file system as the final location.
func (l *Local) Staging(context.Context) (string, func(), error) {
	if err := os.MkdirAll(l.root, 0o775); err != nil { //nolint:gosec // G301: public site content, group-writable for arbitrary UIDs in group 0
		return "", nil, err
	}
	dir, err := os.MkdirTemp(l.root, ".staging-*")
	if err != nil {
		return "", nil, err
	}
	// extract writes files and directories into dir; the final mode is set at Publish.
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// Publish renames the staged directory into the site's versions directory.
func (l *Local) Publish(_ context.Context, site, version, stagingDir string) error {
	dir := l.SiteDir(site)
	if dir == "" || !VersionRe.MatchString(version) {
		return ErrInvalid
	}
	if err := os.Chmod(stagingDir, 0o775); err != nil { //nolint:gosec // G302: public site content, group-writable for arbitrary UIDs in group 0
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, versionsDir), 0o775); err != nil { //nolint:gosec // G301: public site content, group-writable for arbitrary UIDs in group 0
		return err
	}
	return os.Rename(stagingDir, filepath.Join(dir, versionsDir, version))
}

// Versions lists the version directories of site, oldest first.
func (l *Local) Versions(_ context.Context, site string) ([]string, error) {
	dir := l.SiteDir(site)
	if dir == "" {
		return nil, ErrInvalid
	}
	entries, err := os.ReadDir(filepath.Join(dir, versionsDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && VersionRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out, nil
}

// Current reads the target of the current symlink.
func (l *Local) Current(_ context.Context, site string) (string, error) {
	link := l.CurrentDir(site)
	if link == "" {
		return "", ErrInvalid
	}
	target, err := os.Readlink(link)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return filepath.Base(target), nil
}

// SetCurrent replaces the current symlink with one to version, atomically.
func (l *Local) SetCurrent(_ context.Context, site, version string) error {
	dir := l.SiteDir(site)
	if dir == "" {
		return ErrInvalid
	}
	if !VersionRe.MatchString(version) {
		return ErrVersionAbsent
	}
	if st, err := os.Stat(filepath.Join(dir, versionsDir, version)); err != nil || !st.IsDir() {
		return ErrVersionAbsent
	}
	// Build the new link beside the old one, then rename over it: rename(2) is atomic.
	tmp := filepath.Join(dir, ".link-"+xid.New().String())
	if err := os.Symlink(filepath.Join(versionsDir, version), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, currentLink)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// VersionSize sums the regular files of the version directory.
func (l *Local) VersionSize(_ context.Context, site, version string) (int64, error) {
	dir := l.SiteDir(site)
	if dir == "" || !VersionRe.MatchString(version) {
		return 0, ErrInvalid
	}
	return DirSize(filepath.Join(dir, versionsDir, version))
}

// DeleteVersion removes the version directory.
func (l *Local) DeleteVersion(_ context.Context, site, version string) error {
	dir := l.SiteDir(site)
	if dir == "" || !VersionRe.MatchString(version) {
		return ErrInvalid
	}
	return l.RemoveAll(filepath.Join(dir, versionsDir, version))
}

// DeleteSite removes the site directory with all its versions.
func (l *Local) DeleteSite(_ context.Context, site string) error {
	dir := l.SiteDir(site)
	if dir == "" {
		return ErrInvalid
	}
	return l.RemoveAll(dir)
}

// OpenCurrent opens the live version through os.Root. The version is read from the link
// once and the root is opened by that version's directory, so the returned version is
// always the one the root shows, even if a deploy switches the link meanwhile.
func (l *Local) OpenCurrent(ctx context.Context, site string) (Root, string, error) {
	dir := l.SiteDir(site)
	if dir == "" {
		return nil, "", ErrInvalid
	}
	// A deploy can switch the link and prune the old version between the two steps; one
	// retry sees the new link.
	for range 2 {
		version, err := l.Current(ctx, site)
		if err != nil {
			return nil, "", err
		}
		if version == "" {
			return nil, "", ErrNotDeployed
		}
		root, err := os.OpenRoot(filepath.Join(dir, versionsDir, version))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, "", err
		}
		return localRoot{root}, version, nil
	}
	return nil, "", ErrNotDeployed
}

type localRoot struct{ r *os.Root }

func (lr localRoot) Open(name string) (File, error) { return lr.r.Open(name) }
func (lr localRoot) Close() error                   { return lr.r.Close() }

// DirSize returns the total size in bytes of the regular files under root.
func DirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// GroupWritableDirs gives group the owner's permissions on every directory under root that
// lacks group write, so a volume written by an older version (directories 0755, staging
// 0700) can still be changed after the container moves to another UID in group 0, as
// OpenShift's arbitrary UIDs do. Files need no change: they are only read in place, and
// replacing or removing them needs write on the directory. It is best effort, since only
// the owner may chmod, and it skips files, so the cost is one lstat per directory.
func GroupWritableDirs(root string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil //nolint:nilerr // best effort: skip what can't be read
		}
		if fi, err := d.Info(); err == nil && fi.Mode().Perm()&0o020 == 0 {
			perm := fi.Mode().Perm()
			keep := fi.Mode() & (os.ModeSetgid | os.ModeSticky)
			_ = os.Chmod(path, keep|perm|(perm&0o700)>>3) //nolint:gosec // G302: group 0 shares the volume under OpenShift arbitrary UIDs
		}
		return nil
	})
}
