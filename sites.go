package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/rs/xid"
)

// A site is a row in the docs table plus a directory tree under op_data:
//
//	op_data/<site>/versions/<version>/   one extracted upload per deploy
//	op_data/<site>/current               symlink to versions/<version>
//
// Deploys extract into a staging dir, move it into versions/ and then replace the
// "current" symlink with a rename, so readers see either the old or the new version,
// never a partial one. Other code should serve files from CurrentDir(site).

const (
	versionsDir = "versions"
	currentLink = "current"
)

var (
	// siteNameRe is a DNS label: lowercase letters, digits and inner hyphens, 1 to 63 chars.
	siteNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// versionRe matches version ids, which are xids.
	versionRe = regexp.MustCompile(`^[0-9a-v]{20}$`)

	errSiteExists    = errors.New("a site with that name already exists")
	errSiteNotFound  = errors.New("site not found")
	errVersionAbsent = errors.New("version not found")
	errGroupNotFound = errors.New("group not found")
)

// badArchiveError marks a deploy failure caused by the uploaded archive itself.
type badArchiveError struct{ err error }

func (e *badArchiveError) Error() string { return e.err.Error() }
func (e *badArchiveError) Unwrap() error { return e.err }

// Site is a hosted site. OwnerID and GroupID are 0 when the site has no owner or group.
type Site struct {
	ID          int64
	Name        string
	Description string
	OwnerID     int64
	GroupID     int64
}

// validSiteName reports whether name is usable as a site name (a DNS label).
func validSiteName(name string) bool { return siteNameRe.MatchString(name) }

// SiteDir returns the directory holding all versions of site, or "" for an invalid name.
func (s *Server) SiteDir(site string) string {
	if !validSiteName(site) {
		return ""
	}
	return filepath.Join(s.sites, site)
}

// CurrentDir returns the path of the site's live content (a symlink to the current
// version), or "" for an invalid name. It exists once the site has been deployed.
func (s *Server) CurrentDir(site string) string {
	dir := s.SiteDir(site)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, currentLink)
}

// nullableID maps the "none" value 0 to SQL NULL.
func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// checkGroup returns errGroupNotFound unless group is 0 (none) or an existing group.
func (s *Server) checkGroup(ctx context.Context, group int64) error {
	if group == 0 {
		return nil
	}
	var one int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM groups WHERE groupid = ?", group).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return errGroupNotFound
	}
	if err != nil {
		return fmt.Errorf("query group: %w", err)
	}
	return nil
}

// createSite registers a new site owned by owner, optionally in group (0 for none).
// It returns errSiteExists on a name clash and errGroupNotFound for an unknown group.
func (s *Server) createSite(ctx context.Context, name, description string, owner, group int64) (Site, error) {
	if !validSiteName(name) {
		return Site{}, fmt.Errorf("invalid site name %q", name)
	}
	if err := s.checkGroup(ctx, group); err != nil {
		return Site{}, err
	}
	res, err := s.db.ExecContext(ctx,
		"INSERT INTO docs (uowner, ugroup, name, description) VALUES (?, ?, ?, ?) ON CONFLICT (name) DO NOTHING",
		owner, nullableID(group), name, description)
	if err != nil {
		return Site{}, fmt.Errorf("insert site: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Site{}, errSiteExists
	}
	id, _ := res.LastInsertId()
	return Site{ID: id, Name: name, Description: description, OwnerID: owner, GroupID: group}, nil
}

// getSite loads a site by name. It returns errSiteNotFound if there is none.
func (s *Server) getSite(ctx context.Context, name string) (Site, error) {
	var (
		site  = Site{Name: name}
		desc  sql.NullString
		owner sql.NullInt64
		group sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		"SELECT docid, description, uowner, ugroup FROM docs WHERE name = ?", name).Scan(&site.ID, &desc, &owner, &group)
	if errors.Is(err, sql.ErrNoRows) {
		return Site{}, errSiteNotFound
	}
	if err != nil {
		return Site{}, fmt.Errorf("query site: %w", err)
	}
	site.Description, site.OwnerID, site.GroupID = desc.String, owner.Int64, group.Int64
	return site, nil
}

// updateSite stores the description and group (0 for none) of a site.
// It returns errGroupNotFound for an unknown group.
func (s *Server) updateSite(ctx context.Context, site Site) error {
	if err := s.checkGroup(ctx, site.GroupID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, "UPDATE docs SET description = ?, ugroup = ? WHERE docid = ?",
		site.Description, nullableID(site.GroupID), site.ID)
	if err != nil {
		return fmt.Errorf("update site: %w", err)
	}
	return nil
}

// deleteSite removes a site's files and then its database row. A failure removing the
// files leaves the row, so the delete can be retried.
func (s *Server) deleteSite(ctx context.Context, site Site) error {
	dir := s.SiteDir(site.Name)
	if dir == "" {
		return fmt.Errorf("invalid site name %q", site.Name)
	}
	s.deployMu.Lock()
	err := os.RemoveAll(dir)
	s.deployMu.Unlock()
	if err != nil {
		return fmt.Errorf("remove site files: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM docs WHERE docid = ?", site.ID); err != nil {
		return fmt.Errorf("delete site: %w", err)
	}
	return nil
}

// deploy extracts the archive in src into a new version of site and makes it current.
// Archive problems come back as *badArchiveError. Old versions beyond cfg.KeepVersions
// are pruned afterwards. The site must already exist.
func (s *Server) deploy(site string, src *os.File) (string, error) {
	dir := s.SiteDir(site)
	if dir == "" {
		return "", fmt.Errorf("invalid site name %q", site)
	}
	if err := os.MkdirAll(filepath.Join(dir, versionsDir), 0o755); err != nil { //nolint:gosec // G301: public site content
		return "", err
	}

	// Stage next to the final location so the rename stays on one filesystem.
	staging, err := os.MkdirTemp(dir, ".staging-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)

	limits := extractLimits{maxBytes: s.cfg.MaxExtractSize, maxFiles: s.cfg.MaxExtractFile}
	if err := extractArchive(src, staging, limits); err != nil {
		return "", &badArchiveError{err}
	}
	if err := os.Chmod(staging, 0o755); err != nil { //nolint:gosec // G302: public site content
		return "", err
	}

	version := xid.New().String() // time-sortable, so lexical order is deploy order
	if err := os.Rename(staging, filepath.Join(dir, versionsDir, version)); err != nil {
		return "", err
	}

	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	if err := s.switchCurrent(site, version); err != nil {
		return "", err
	}
	if err := s.pruneVersions(site); err != nil {
		slog.Warn("prune old versions", "site", site, "err", err)
	}
	return version, nil
}

// SwitchCurrent atomically points the site's current link at an existing version,
// for example to roll back. It returns errVersionAbsent if the version is not on disk.
func (s *Server) SwitchCurrent(site, version string) error {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	return s.switchCurrent(site, version)
}

// switchCurrent does the work of SwitchCurrent; the caller holds deployMu.
func (s *Server) switchCurrent(site, version string) error {
	dir := s.SiteDir(site)
	if dir == "" {
		return fmt.Errorf("invalid site name %q", site)
	}
	if !versionRe.MatchString(version) {
		return errVersionAbsent
	}
	if st, err := os.Stat(filepath.Join(dir, versionsDir, version)); err != nil || !st.IsDir() {
		return errVersionAbsent
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

// Versions lists the site's versions on disk, oldest first.
func (s *Server) Versions(site string) ([]string, error) {
	dir := s.SiteDir(site)
	if dir == "" {
		return nil, fmt.Errorf("invalid site name %q", site)
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
		if e.IsDir() && versionRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out, nil
}

// CurrentVersion returns the version the site currently serves, or "" if never deployed.
func (s *Server) CurrentVersion(site string) (string, error) {
	link := s.CurrentDir(site)
	if link == "" {
		return "", fmt.Errorf("invalid site name %q", site)
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

// pruneVersions removes the oldest versions beyond cfg.KeepVersions, never the current
// one. The caller holds deployMu.
func (s *Server) pruneVersions(site string) error {
	versions, err := s.Versions(site)
	if err != nil {
		return err
	}
	current, err := s.CurrentVersion(site)
	if err != nil {
		return err
	}
	excess := len(versions) - max(s.cfg.KeepVersions, 1)
	var errs []error
	for _, v := range versions {
		if excess <= 0 {
			break
		}
		if v == current {
			continue
		}
		if err := os.RemoveAll(filepath.Join(s.SiteDir(site), versionsDir, v)); err != nil {
			errs = append(errs, err)
			continue
		}
		excess--
	}
	return errors.Join(errs...)
}
