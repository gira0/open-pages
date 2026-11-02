package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/gira0/open-pages/internal/extract"
	"github.com/gira0/open-pages/internal/names"
	"github.com/gira0/open-pages/internal/storage"
	"github.com/rs/xid"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// A site is a row in the docs table plus a directory tree under op_data:
//
//	op_data/<site>/versions/<version>/   one extracted upload per deploy
//	op_data/<site>/current               symlink to versions/<version>
//
// Deploys extract into a staging dir, move it into versions/ and then replace the
// "current" symlink with a rename, so readers see either the old or the new version,
// never a partial one. That layout belongs to the local storage backend (internal/storage);
// the server reaches site files only through its storage.Store. SiteDir and CurrentDir
// exist for tests and tools that know the backend is local.

const (
	versionsDir = "versions" // local backend layout; tests look inside it
	currentLink = "current"
)

var (
	errSiteExists    = errors.New("a site with that name already exists")
	errSiteNotFound  = errors.New("site not found")
	errVersionAbsent = storage.ErrVersionAbsent
	errGroupNotFound = errors.New("group not found")

	errNotExpired = errors.New("site is not expired")

	errPreviewBase    = errors.New(`a name containing "--" is a preview and needs an existing site before the "--"`)
	errPreviewOwner   = errors.New(`a name containing "--" is a preview: only the owner of the site before the "--" (or a member of its group) may create it`)
	errPreviewVisible = errors.New("a preview may not be more public than the site it previews")
)

// visRank orders visibilities from the narrowest audience to the widest.
var visRank = map[string]int{visRestricted: 0, visAuthenticated: 1, visPublic: 2}

// badArchiveError marks a deploy failure caused by the uploaded archive itself.
type badArchiveError struct{ err error }

func (e *badArchiveError) Error() string { return e.err.Error() }
func (e *badArchiveError) Unwrap() error { return e.err }

// Site visibility. A public site is served to everyone. An authenticated site is served to
// any logged-in user (session or API token). A restricted site is served only to its owner
// and to members of its group (see canView).
const (
	visPublic        = "public"
	visAuthenticated = "authenticated"
	visRestricted    = "restricted"
)

// validVisibility reports whether v is a known visibility.
func validVisibility(v string) bool {
	return v == visPublic || v == visAuthenticated || v == visRestricted
}

// Site is a hosted site. OwnerID and GroupID are 0 when the site has no owner or group.
type Site struct {
	ID          int64
	Name        string
	Description string
	OwnerID     int64
	GroupID     int64
	Visibility  string
	ExpiresAt   int64 // Unix time after which the site is deleted; 0 for never
}

// SiteDir returns the directory holding all versions of site, or "" for an invalid name.
func (s *Server) SiteDir(site string) string {
	return s.local.SiteDir(site)
}

// CurrentDir returns the path of the site's live content (a symlink to the current
// version), or "" for an invalid name. It exists once the site has been deployed.
func (s *Server) CurrentDir(site string) string {
	return s.local.CurrentDir(site)
}

// nullableID maps the "none" value 0 to SQL NULL.
func nullableID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// checkGroup decides whether owner may put a site in group. 0 (none) is always fine. A
// group counts only if the site owner belongs to it: it returns errGroupNotFound for an
// unknown group and errNotGroupMember when the owner is not a member.
func (s *Server) checkGroup(ctx context.Context, group, owner int64) error {
	if group == 0 {
		return nil
	}
	member, err := s.userInGroup(ctx, owner, group)
	if err != nil {
		return err
	}
	if member {
		return nil
	}
	var one int
	err = s.db.QueryRowContext(ctx, "SELECT 1 FROM groups WHERE groupid = ?", group).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return errGroupNotFound
	}
	if err != nil {
		return fmt.Errorf("query group: %w", err)
	}
	return errNotGroupMember
}

// canView reports whether the user (0 for anonymous) may see the site's content and
// metadata. Public sites: everyone. Authenticated sites: any logged-in user. Restricted sites: the owner, and members of the site's
// group, but only while the owner is still a member of that group too, so a site can't be
// shared with a group its owner has no standing in.
func (s *Server) canView(ctx context.Context, site Site, user int64) (bool, error) {
	if site.Visibility == visPublic {
		return true, nil
	}
	if user == 0 {
		return false, nil
	}
	if site.Visibility == visAuthenticated {
		return true, nil
	}
	if user == site.OwnerID {
		return true, nil
	}
	if site.GroupID == 0 || site.OwnerID == 0 {
		return false, nil
	}
	member, err := s.userInGroup(ctx, user, site.GroupID)
	if err != nil || !member {
		return false, err
	}
	return s.userInGroup(ctx, site.OwnerID, site.GroupID)
}

// publicSite is one entry of the public site listing.
type publicSite struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// listPublicSites returns the public sites ordered by name. It is never nil.
func (s *Server) listPublicSites(ctx context.Context) ([]publicSite, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT name, COALESCE(description, '') FROM docs WHERE visibility = ? ORDER BY name", visPublic)
	if err != nil {
		return nil, fmt.Errorf("query public sites: %w", err)
	}
	defer rows.Close()
	out := []publicSite{}
	for rows.Next() {
		var p publicSite
		if err := rows.Scan(&p.Name, &p.Description); err != nil {
			return nil, fmt.Errorf("scan site: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query public sites: %w", err)
	}
	return out, nil
}

// groupGone reports whether err is the foreign-key failure of writing a site's group
// column because that group was deleted after checkGroup looked. group is the id being
// written, 0 meaning none.
func groupGone(err error, group int64) bool {
	var se *sqlite.Error
	return group != 0 && errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY
}

// createSite registers a new site owned by owner, optionally in group (0 for none).
// It returns errSiteExists on a name clash, errGroupNotFound for an unknown group,
// errNotGroupMember if owner is not in the group and errSiteLimit if owner already owns
// cfg.SitesPerUser sites.
func (s *Server) createSite(ctx context.Context, name, description string, owner, group int64, visibility string) (Site, error) {
	return s.createSiteExpiring(ctx, name, description, owner, group, visibility, 0)
}

// checkPreview enforces the rules for a name of the form "<base>--<rest>": it is a preview
// of <base>, so <base> must exist, only its owner (or a member of its group) may create the
// preview, and the preview may not be more public than <base>. Other names pass.
func (s *Server) checkPreview(ctx context.Context, name string, owner int64, visibility string) error {
	base, ok := names.PreviewBase(name)
	if !ok {
		return nil
	}
	parent, err := s.getSite(ctx, base)
	if errors.Is(err, errSiteNotFound) {
		return errPreviewBase
	}
	if err != nil {
		return err
	}
	allowed := owner != 0 && owner == parent.OwnerID
	if !allowed && parent.GroupID != 0 {
		if allowed, err = s.userInGroup(ctx, owner, parent.GroupID); err != nil {
			return err
		}
	}
	if !allowed {
		return errPreviewOwner
	}
	if visRank[visibility] > visRank[parent.Visibility] {
		return errPreviewVisible
	}
	return nil
}

// checkPreviewVisibility is previewCeiling on the database, for an early, cheap refusal.
// The authoritative check runs again inside the write transaction (see previewCeiling).
func (s *Server) checkPreviewVisibility(ctx context.Context, name, visibility string) error {
	return previewCeiling(ctx, s.db, name, visibility)
}

// previewCeiling returns errPreviewVisible if name is a preview whose base site exists and
// is less public than visibility. Other names pass. Called on a transaction after it has
// taken the write lock, the base's visibility cannot change before the transaction ends,
// and a base that is narrowed later narrows this preview with it (narrowPreviews).
func previewCeiling(ctx context.Context, q execQuerier, name, visibility string) error {
	base, ok := names.PreviewBase(name)
	if !ok {
		return nil
	}
	var parent string
	err := q.QueryRowContext(ctx, "SELECT visibility FROM docs WHERE name = ?", base).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // orphaned preview: nothing to compare with
	}
	if err != nil {
		return fmt.Errorf("read base visibility: %w", err)
	}
	if visRank[visibility] > visRank[parent] {
		return errPreviewVisible
	}
	return nil
}

// createSiteExpiring is createSite for a site that is deleted at expiresAt (a Unix time; 0
// for never), which is stored in the same INSERT so a preview can never exist without its
// expiry. A name of the form "<base>--<rest>" is a preview and must pass checkPreview.
func (s *Server) createSiteExpiring(ctx context.Context, name, description string, owner, group int64, visibility string, expiresAt int64) (Site, error) {
	if !names.ValidSite(name) {
		return Site{}, errors.New("invalid site name")
	}
	if names.ReservedSite(name) {
		return Site{}, errors.New("site name is reserved")
	}
	if !validVisibility(visibility) {
		return Site{}, fmt.Errorf("invalid visibility %q", visibility)
	}
	if err := s.checkGroup(ctx, group, owner); err != nil {
		return Site{}, err
	}
	if err := s.checkPreview(ctx, name, owner, visibility); err != nil {
		return Site{}, err
	}
	if s.afterPreviewCheck != nil {
		s.afterPreviewCheck() // test hook: a change landing between the check and the write
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Site{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	res, err := tx.ExecContext(ctx,
		"INSERT INTO docs (uowner, ugroup, name, description, visibility, expires_at) VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (name) DO NOTHING",
		owner, nullableID(group), name, description, visibility, nullableID(expiresAt))
	if groupGone(err, group) {
		return Site{}, errGroupNotFound
	}
	if err != nil {
		return Site{}, fmt.Errorf("insert site: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Site{}, errSiteExists
	}
	id, _ := res.LastInsertId()
	// The insert took the write lock, so the base's visibility can't change any more:
	// check it again now, because it may have been narrowed after the early check.
	if err := previewCeiling(ctx, tx, name, visibility); err != nil {
		return Site{}, err
	}
	// The insert took the write lock, so this count is exact: two creates cannot both pass.
	if limit := s.cfg.SitesPerUser; limit > 0 {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM docs WHERE uowner = ?", owner).Scan(&n); err != nil {
			return Site{}, fmt.Errorf("count sites: %w", err)
		}
		if n > limit {
			return Site{}, errSiteLimit
		}
	}
	// The create entry marks where this site's history starts (a reused row id must not
	// expose a predecessor's entries), so it is written in the same transaction as the
	// row and is the one audit write that fails the request: no marker, no site.
	emit, err := s.auditInsert(ctx, tx, auditEvent{
		action: auditSiteCreate, targetKind: targetSite, target: name, siteID: id,
		details: createDetails(description, group, visibility, expiresAt),
	})
	if err != nil {
		return Site{}, fmt.Errorf("audit site create: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Site{}, fmt.Errorf("commit: %w", err)
	}
	emit()
	return Site{ID: id, Name: name, Description: description, OwnerID: owner, GroupID: group, Visibility: visibility, ExpiresAt: expiresAt}, nil
}

// createDetails is the audit detail object of site.create.
func createDetails(description string, group int64, visibility string, expiresAt int64) map[string]any {
	d := map[string]any{"description": description, "group": group, "visibility": visibility}
	if expiresAt != 0 {
		d["expires_at"] = expiresAt
	}
	return d
}

// getSite loads a site by name. It returns errSiteNotFound if there is none.
func (s *Server) getSite(ctx context.Context, name string) (Site, error) {
	var (
		site    = Site{Name: name}
		desc    sql.NullString
		owner   sql.NullInt64
		group   sql.NullInt64
		expires sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		"SELECT docid, description, uowner, ugroup, visibility, expires_at FROM docs WHERE name = ?", name).Scan(&site.ID, &desc, &owner, &group, &site.Visibility, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Site{}, errSiteNotFound
	}
	if err != nil {
		return Site{}, fmt.Errorf("query site: %w", err)
	}
	site.Description, site.OwnerID, site.GroupID, site.ExpiresAt = desc.String, owner.Int64, group.Int64, expires.Int64
	return site, nil
}

// updateSite changes the description, group (0 for none) and/or visibility of site,
// writing only the columns whose argument is non-nil, in a single statement. It returns the
// site with its stored values, errGroupNotFound for an unknown group, errNotGroupMember if
// the owner is not in the new group and errSiteNotFound if the site has been deleted meanwhile.
func (s *Server) updateSite(ctx context.Context, site Site, description *string, group *int64, visibility *string) (Site, error) {
	return s.updateSiteExpiring(ctx, site, description, group, visibility, nil)
}

// updateSiteExpiring is updateSite that can also set the expiry (a Unix time; 0 removes it)
// in the same statement and audit entry. Its "from" value is read under the write lock.
func (s *Server) updateSiteExpiring(ctx context.Context, site Site, description *string, group *int64, visibility *string, expiresAt *int64) (Site, error) {
	var (
		sets []string
		args []any
	)
	if visibility != nil {
		// A preview may not be made more public than the site it previews.
		if err := s.checkPreviewVisibility(ctx, site.Name, *visibility); err != nil {
			return Site{}, err
		}
		if s.afterPreviewCheck != nil {
			s.afterPreviewCheck() // test hook
		}
	}
	if description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *description)
	}
	if group != nil {
		if err := s.checkGroup(ctx, *group, site.OwnerID); err != nil {
			return Site{}, err
		}
		sets = append(sets, "ugroup = ?")
		args = append(args, nullableID(*group))
	}
	if visibility != nil {
		if !validVisibility(*visibility) {
			return Site{}, fmt.Errorf("invalid visibility %q", *visibility)
		}
		sets = append(sets, "visibility = ?")
		args = append(args, *visibility)
	}
	if expiresAt != nil {
		sets = append(sets, "expires_at = ?")
		args = append(args, nullableID(*expiresAt))
	}
	if len(sets) == 0 {
		sets = append(sets, "docid = docid") // no-op write, so RETURNING reads the row
	}
	// Every statement below matches the site's id, name and owner, not just the id: the
	// caller authorized this exact site earlier, and if it has been deleted since, its
	// row id may belong to a new site by now, which must stay untouched and unseen.
	args = append(args, site.ID, site.Name, nullableID(site.OwnerID))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Site{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	// Take the write lock before reading the old values: a transaction that reads first
	// and then writes fails with SQLITE_BUSY when another writer got in between. With the
	// lock held, the old and new values below are an exact pair, whatever else is updating.
	res, err := tx.ExecContext(ctx, "UPDATE docs SET docid = docid WHERE docid = ? AND name = ? AND uowner IS ?",
		site.ID, site.Name, nullableID(site.OwnerID))
	if err != nil {
		return Site{}, fmt.Errorf("lock site: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return Site{}, errSiteNotFound
	}
	if visibility != nil {
		// With the write lock held, check the base again: it may have been narrowed since.
		if err := previewCeiling(ctx, tx, site.Name, *visibility); err != nil {
			return Site{}, err
		}
	}
	var (
		before = site
		desc   sql.NullString
		grp    sql.NullInt64
		exp    sql.NullInt64
	)
	err = tx.QueryRowContext(ctx, "SELECT description, ugroup, visibility, expires_at FROM docs WHERE docid = ? AND name = ? AND uowner IS ?",
		site.ID, site.Name, nullableID(site.OwnerID)).
		Scan(&desc, &grp, &before.Visibility, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return Site{}, errSiteNotFound
	}
	if err != nil {
		return Site{}, fmt.Errorf("read site: %w", err)
	}
	before.Description, before.GroupID, before.ExpiresAt = desc.String, grp.Int64, exp.Int64
	out := site
	err = tx.QueryRowContext(ctx,
		"UPDATE docs SET "+strings.Join(sets, ", ")+" WHERE docid = ? AND name = ? AND uowner IS ? RETURNING description, ugroup, visibility, expires_at",
		args...).Scan(&desc, &grp, &out.Visibility, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return Site{}, errSiteNotFound
	}
	if group != nil && groupGone(err, *group) {
		return Site{}, errGroupNotFound
	}
	if err != nil {
		return Site{}, fmt.Errorf("update site: %w", err)
	}
	out.Description, out.GroupID, out.ExpiresAt = desc.String, grp.Int64, exp.Int64

	// Only real changes are recorded, each with its old and new value.
	changes := map[string]any{}
	if out.Description != before.Description {
		changes["description"] = map[string]any{"from": before.Description, "to": out.Description}
	}
	if out.GroupID != before.GroupID {
		changes["group"] = map[string]any{"from": before.GroupID, "to": out.GroupID}
	}
	if out.Visibility != before.Visibility {
		changes["visibility"] = map[string]any{"from": before.Visibility, "to": out.Visibility}
	}
	if out.ExpiresAt != before.ExpiresAt {
		changes["expires_at"] = map[string]any{"from": before.ExpiresAt, "to": out.ExpiresAt}
	}
	emits := []func(){}
	if len(changes) > 0 {
		emits = append(emits, s.auditRecord(ctx, tx, auditEvent{
			action: auditSiteUpdate, targetKind: targetSite, target: site.Name, siteID: site.ID,
			details: map[string]any{"changes": changes},
		}))
	}
	// Narrowing a site narrows its previews with it, in the same transaction, so a preview
	// never stays more public than its base.
	if _, isPreview := names.PreviewBase(site.Name); !isPreview && out.Visibility != before.Visibility {
		narrowed, err := s.narrowPreviews(ctx, tx, site.Name, out.Visibility)
		if err != nil {
			return Site{}, err
		}
		emits = append(emits, narrowed...)
	}
	if err := tx.Commit(); err != nil {
		return Site{}, fmt.Errorf("commit: %w", err)
	}
	for _, emit := range emits {
		emit()
	}
	return out, nil
}

// narrowPreviews lowers the visibility of every preview of base ("<base>--...") that is more
// public than visibility, in tx, and returns the functions that log each audit entry.
func (s *Server) narrowPreviews(ctx context.Context, tx *sql.Tx, base, visibility string) ([]func(), error) {
	type preview struct {
		id         int64
		name, from string
	}
	rows, err := tx.QueryContext(ctx, "SELECT docid, name, visibility FROM docs WHERE substr(name, 1, ?) = ?",
		len(base)+2, base+"--")
	if err != nil {
		return nil, fmt.Errorf("list previews: %w", err)
	}
	var wide []preview
	for rows.Next() {
		var p preview
		if err := rows.Scan(&p.id, &p.name, &p.from); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan preview: %w", err)
		}
		if visRank[p.from] > visRank[visibility] {
			wide = append(wide, p)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return nil, fmt.Errorf("list previews: %w", err)
	}
	var emits []func()
	for _, p := range wide {
		if _, err := tx.ExecContext(ctx, "UPDATE docs SET visibility = ? WHERE docid = ?", visibility, p.id); err != nil {
			return nil, fmt.Errorf("narrow preview: %w", err)
		}
		emits = append(emits, s.auditRecord(ctx, tx, auditEvent{
			action: auditSiteUpdate, targetKind: targetSite, target: p.name, siteID: p.id,
			details: map[string]any{
				"changes": map[string]any{"visibility": map[string]any{"from": p.from, "to": visibility}},
				"reason":  "base site " + base + " was narrowed",
			},
		}))
	}
	return emits, nil
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
	return s.deleteSiteWith(ctx, site, nil, 0)
}

// deleteSiteWith is deleteSite with details added to the site.delete audit entry, for
// example the reason for a delete nobody asked for. A non-zero expiredBy makes the delete
// conditional: the site must still have an expiry at or before that Unix time, checked
// under the database write lock that is then held until the delete commits, so a renewal
// that lands first keeps the site (errNotExpired, nothing removed) and one that comes
// later waits and finds the site gone.
func (s *Server) deleteSiteWith(ctx context.Context, site Site, details map[string]any, expiredBy int64) error {
	if !names.ValidSite(site.Name) {
		return errors.New("invalid site name")
	}
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	if err := s.checkSiteLive(ctx, site); err != nil {
		return err
	}
	// The audit entry goes in the same transaction as the delete (and under deployMu), so
	// it always precedes the site.create of a site that reuses the name and row id.
	var tx *sql.Tx
	if expiredBy != 0 {
		var err error
		if tx, err = s.db.BeginTx(ctx, nil); err != nil {
			return fmt.Errorf("begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }() // no-op after Commit
		res, err := tx.ExecContext(ctx,
			"UPDATE docs SET docid = docid WHERE docid = ? AND name = ? AND uowner IS ? AND expires_at IS NOT NULL AND expires_at <= ?",
			site.ID, site.Name, nullableID(site.OwnerID), expiredBy)
		if err != nil {
			return fmt.Errorf("lock site: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errNotExpired
		}
	}
	if err := s.store.DeleteSite(ctx, site.Name); err != nil {
		return fmt.Errorf("remove site files: %w", err)
	}
	if tx == nil {
		var err error
		if tx, err = s.db.BeginTx(ctx, nil); err != nil {
			return fmt.Errorf("begin: %w", err)
		}
		defer func() { _ = tx.Rollback() }() // no-op after Commit
	}
	res, err := tx.ExecContext(ctx, "DELETE FROM docs WHERE docid = ? AND name = ? AND uowner IS ?",
		site.ID, site.Name, nullableID(site.OwnerID))
	if err != nil {
		return fmt.Errorf("delete site: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errSiteNotFound
	}
	emit := s.auditRecord(ctx, tx, auditEvent{action: auditSiteDelete, targetKind: targetSite, target: site.Name, siteID: site.ID, details: details})
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	emit()
	// The site is gone whether or not the client is still there.
	s.refreshStoredBytes(context.WithoutCancel(ctx))
	return nil
}

// deploy extracts the archive in src into a new version of site and makes it current.
// Archive problems come back as *badArchiveError and a deploy that would break a quota as
// *quotaError. Old versions beyond cfg.KeepVersions are pruned afterwards. The site must already exist; deploy returns errSiteNotFound if
// it was deleted (or replaced by a different site of the same name) before publishing.
func (s *Server) deploy(ctx context.Context, site Site, src *os.File) (string, error) {
	if !names.ValidSite(site.Name) {
		return "", errors.New("invalid site name")
	}

	// Staging is local disk outside any site's own directory, so that nothing here can
	// recreate a site that deleteSite removed.
	staging, cleanup, err := s.store.Staging(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()

	limits := extract.Limits{MaxBytes: s.cfg.MaxExtractSize, MaxFiles: s.cfg.MaxExtractFile}
	extractCtx, cancel := context.WithTimeout(ctx, s.cfg.DeployTimeout)
	defer cancel()
	if err := extract.Archive(extractCtx, src, staging, limits); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err() // the client went away; nobody reads the response
		}
		if extractCtx.Err() != nil {
			return "", &badArchiveError{fmt.Errorf("extraction took longer than %d seconds", int(s.cfg.DeployTimeout.Seconds()))}
		}
		return "", &badArchiveError{err}
	}
	size, err := storage.DirSize(staging)
	if err != nil {
		return "", err
	}

	// Publishing, switching and pruning happen under one lock, so a concurrent deploy's
	// prune can never remove a version that this deploy has published but not yet made
	// current. Extraction above stays outside the lock. The quota check is inside it, so two
	// deploys cannot both fit into the same headroom.
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	if err := s.checkSiteLive(ctx, site); err != nil {
		return "", err
	}
	if err := s.checkQuota(ctx, site, size); err != nil {
		// checkQuota may have pruned versions and reconciled rows even when it rejects.
		s.refreshStoredBytes(context.WithoutCancel(ctx))
		return "", err
	}
	version := xid.New().String() // time-sortable, so lexical order is deploy order
	if err := s.store.Publish(ctx, site.Name, version, staging); err != nil {
		return "", err
	}
	// The accounting below must not stop half-way because the client went away.
	acct := context.WithoutCancel(ctx)
	if err := s.recordVersionSize(acct, site, version, size); err != nil {
		// Not fatal: the next quota check or restart measures the directory instead.
		ctxLogger(ctx).Warn("record version size", "site", site.Name, "err", err)
	}
	if err := s.switchCurrent(site.Name, version); err != nil {
		return "", err
	}
	if err := s.pruneVersions(site.Name); err != nil {
		ctxLogger(ctx).Warn("prune old versions", "site", site.Name, "err", err)
	}
	if _, err := s.syncVersionSizes(acct, site); err != nil {
		ctxLogger(ctx).Warn("sync version sizes", "site", site.Name, "err", err)
	}
	s.refreshStoredBytes(acct)
	// Recorded while deployMu is held, so a delete and re-create of the site cannot slip
	// in first, and independent of whether the client is still connected.
	s.audit(ctx, s.db, auditEvent{
		action: auditSiteDeploy, targetKind: targetSite, target: site.Name, siteID: site.ID,
		details: map[string]any{"version": version},
	})
	return version, nil
}

// SwitchCurrent atomically points the site's current link at an existing version,
// for example to roll back. It returns errVersionAbsent if the version is not on disk.
func (s *Server) SwitchCurrent(site, version string) error {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	return s.switchCurrent(site, version)
}

// rollbackSite makes version the live one of site and records it, all under deployMu so
// the entry cannot land after a delete and re-create of the site. It returns
// errSiteNotFound if the site is gone and errVersionAbsent if the version is not on disk.
func (s *Server) rollbackSite(ctx context.Context, site Site, version string) error {
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	if err := s.checkSiteLive(ctx, site); err != nil {
		return err
	}
	if err := s.switchCurrent(site.Name, version); err != nil {
		return err
	}
	s.audit(ctx, s.db, auditEvent{
		action: auditSiteRollback, targetKind: targetSite, target: site.Name, siteID: site.ID,
		details: map[string]any{"version": version},
	})
	return nil
}

// switchCurrent does the work of SwitchCurrent; the caller holds deployMu.
func (s *Server) switchCurrent(site, version string) error {
	if !names.ValidSite(site) {
		return errors.New("invalid site name")
	}
	return s.store.SetCurrent(context.Background(), site, version)
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

// Versions lists the site's versions in the store, oldest first.
func (s *Server) Versions(site string) ([]string, error) {
	if !names.ValidSite(site) {
		return nil, errors.New("invalid site name")
	}
	return s.store.Versions(context.Background(), site)
}

// CurrentVersion returns the version the site currently serves, or "" if never deployed.
func (s *Server) CurrentVersion(site string) (string, error) {
	if !names.ValidSite(site) {
		return "", errors.New("invalid site name")
	}
	return s.store.Current(context.Background(), site)
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
		if err := s.store.DeleteVersion(context.Background(), site, v); err != nil {
			errs = append(errs, err)
			continue
		}
		excess--
	}
	return errors.Join(errs...)
}
