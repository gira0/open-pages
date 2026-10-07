package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

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
	if reservedSiteName(name) {
		return Site{}, fmt.Errorf("site name %q is reserved", name)
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

// updateSite changes the description and/or group (0 for none) of the site with the
// given id, writing only the columns whose argument is non-nil, in a single statement.
// It returns the stored values, errGroupNotFound for an unknown group and errSiteNotFound
// if the site has been deleted meanwhile.
func (s *Server) updateSite(ctx context.Context, id int64, description *string, group *int64) (string, int64, error) {
	var (
		sets []string
		args []any
	)
	if description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *description)
	}
	if group != nil {
		if err := s.checkGroup(ctx, *group); err != nil {
			return "", 0, err
		}
		sets = append(sets, "ugroup = ?")
		args = append(args, nullableID(*group))
	}
	if len(sets) == 0 {
		sets = append(sets, "docid = docid") // no-op write, so RETURNING reads the row
	}
	args = append(args, id)
	var (
		desc sql.NullString
		grp  sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		"UPDATE docs SET "+strings.Join(sets, ", ")+" WHERE docid = ? RETURNING description, ugroup",
		args...).Scan(&desc, &grp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, errSiteNotFound
	}
	if err != nil {
		return "", 0, fmt.Errorf("update site: %w", err)
	}
	return desc.String, grp.Int64, nil
}

// checkSiteLive returns errSiteNotFound unless the database still has this exact site:
// same id, name and owner. The caller holds deployMu, which deleteSite also takes, so
// the answer stays true until the caller releases it.
func (s *Server) checkSiteLive(ctx context.Context, site Site) error {
	var one int
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM docs WHERE docid = ? AND name = ? AND uowner IS ?",
		site.ID, site.Name, nullableID(site.OwnerID)).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return errSiteNotFound
	}
	if err != nil {
		return fmt.Errorf("check site: %w", err)
	}
	return nil
}

// deleteSite removes a site's files and then its database row, under deployMu so it
// cannot interleave with a deploy's publish step. It returns errSiteNotFound if the
// site was already deleted or replaced by another with the same name. A failure removing
// the files leaves the row, so the delete can be retried.
func (s *Server) deleteSite(ctx context.Context, site Site) error {
	dir := s.SiteDir(site.Name)
	if dir == "" {
		return fmt.Errorf("invalid site name %q", site.Name)
	}
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	if err := s.checkSiteLive(ctx, site); err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove site files: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM docs WHERE docid = ?", site.ID); err != nil {
		return fmt.Errorf("delete site: %w", err)
	}
	return nil
}

// deploy extracts the archive in src into a new version of site and makes it current.
// Archive problems come back as *badArchiveError. Old versions beyond cfg.KeepVersions
// are pruned afterwards. The site must already exist; deploy returns errSiteNotFound if
// it was deleted (or replaced by a different site of the same name) before publishing.
func (s *Server) deploy(ctx context.Context, site Site, src *os.File) (string, error) {
	dir := s.SiteDir(site.Name)
	if dir == "" {
		return "", fmt.Errorf("invalid site name %q", site.Name)
	}
	if err := os.MkdirAll(s.sites, 0o755); err != nil { //nolint:gosec // G301: public site content
		return "", err
	}

	// Stage in the sites root, not in the site's own directory, so that nothing here can
	// recreate a site directory that deleteSite removed. Site names never start with a
	// dot, and the rename into place stays on one filesystem.
	staging, err := os.MkdirTemp(s.sites, ".staging-*")
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

	// Publishing, switching and pruning happen under one lock, so a concurrent deploy's
	// prune can never remove a version that this deploy has published but not yet made
	// current. Extraction above stays outside the lock.
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	if err := s.checkSiteLive(ctx, site); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, versionsDir), 0o755); err != nil { //nolint:gosec // G301: public site content
		return "", err
	}
	version := xid.New().String() // time-sortable, so lexical order is deploy order
	if err := os.Rename(staging, filepath.Join(dir, versionsDir, version)); err != nil {
		return "", err
	}
	if err := s.switchCurrent(site.Name, version); err != nil {
		return "", err
	}
	if err := s.pruneVersions(site.Name); err != nil {
		ctxLogger(ctx).Warn("prune old versions", "site", site.Name, "err", err)
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

// versionsAndCurrent reads the version list and the live version as one snapshot,
// under deployMu so no deploy or rollback can land between the two reads.
func (s *Server) versionsAndCurrent(site string) (versions []string, current string, err error) {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	if versions, err = s.Versions(site); err != nil {
		return nil, "", err
	}
	if current, err = s.CurrentVersion(site); err != nil {
		return nil, "", err
	}
	return versions, current, nil
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
