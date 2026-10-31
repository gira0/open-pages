package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
)

// Quotas limit the bytes a site and its owner keep on disk. A site's usage is the sum of
// its kept versions, taken after pruning; the size of each version is stored in the
// site_version table when it is deployed, so usage is a cheap query. Rows follow the version
// directories: versions that predate the table are measured once, by walking the directory
// (at startup, or at the site's next deploy), and rows of removed versions are dropped.
//
// Checks run under deployMu, the lock every deploy and delete holds, so two deploys cannot
// both pass on the same headroom.

// quotaError marks a deploy refused because it would exceed a quota.
type quotaError struct{ msg string }

func (e *quotaError) Error() string { return e.msg }

// errSiteLimit is returned by createSite when the owner already owns sites_per_user sites.
var errSiteLimit = errors.New("site limit reached")

// dirSize returns the total size in bytes of the regular files under root.
func dirSize(root string) (int64, error) {
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

// humanSize renders a byte count for error messages.
func humanSize(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	}
	return fmt.Sprintf("%d bytes", n)
}

// syncVersionSizes makes the site_version rows of site match the versions on disk and
// returns the size of each version on disk. The caller holds deployMu.
func (s *Server) syncVersionSizes(ctx context.Context, site Site) (map[string]int64, error) {
	versions, err := s.Versions(site.Name)
	if err != nil {
		return nil, err
	}
	known := map[string]int64{}
	rows, err := s.db.QueryContext(ctx, "SELECT version, bytes FROM site_version WHERE docid = ?", site.ID)
	if err != nil {
		return nil, fmt.Errorf("query version sizes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var v string
		var n int64
		if err := rows.Scan(&v, &n); err != nil {
			return nil, fmt.Errorf("scan version size: %w", err)
		}
		known[v] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("query version sizes: %w", err)
	}
	_ = rows.Close()

	sizes := make(map[string]int64, len(versions))
	missing := map[string]int64{}
	for _, v := range versions {
		n, ok := known[v]
		if !ok {
			if n, err = dirSize(filepath.Join(s.SiteDir(site.Name), versionsDir, v)); err != nil {
				return nil, fmt.Errorf("measure version %s: %w", v, err)
			}
			missing[v] = n
		}
		sizes[v] = n
		delete(known, v)
	}
	if len(missing) == 0 && len(known) == 0 {
		return sizes, nil
	}
	// known now holds the rows of versions that are gone. The first statement is a write,
	// so the transaction holds the write lock before anything else happens in it.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	for v := range known {
		if _, err := tx.ExecContext(ctx, "DELETE FROM site_version WHERE docid = ? AND version = ?", site.ID, v); err != nil {
			return nil, fmt.Errorf("delete version size: %w", err)
		}
	}
	for v, n := range missing {
		if _, err := tx.ExecContext(ctx, "INSERT INTO site_version (docid, version, bytes) VALUES (?, ?, ?)", site.ID, v, n); err != nil {
			return nil, fmt.Errorf("insert version size: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit: %w", err)
	}
	return sizes, nil
}

// recordVersionSize stores the size of a version that was just published.
func (s *Server) recordVersionSize(ctx context.Context, site Site, version string, size int64) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT OR REPLACE INTO site_version (docid, version, bytes) VALUES (?, ?, ?)", site.ID, version, size)
	return err
}

// otherSitesBytes returns the bytes held by the sites owner owns besides the one with the
// given id.
func (s *Server) otherSitesBytes(ctx context.Context, owner, except int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(v.bytes), 0) FROM site_version v JOIN docs d ON d.docid = v.docid
		 WHERE d.uowner = ? AND d.docid != ?`, owner, except).Scan(&n)
	return n, err
}

// syncOtherSites reconciles the version sizes of the other sites site's owner has. The
// caller holds deployMu.
func (s *Server) syncOtherSites(ctx context.Context, site Site) error {
	rows, err := s.db.QueryContext(ctx, "SELECT docid, name FROM docs WHERE uowner = ? AND docid != ?", site.OwnerID, site.ID)
	if err != nil {
		return fmt.Errorf("list sites: %w", err)
	}
	defer rows.Close()
	var others []Site
	for rows.Next() {
		o := Site{OwnerID: site.OwnerID}
		if err := rows.Scan(&o.ID, &o.Name); err != nil {
			return fmt.Errorf("scan site: %w", err)
		}
		others = append(others, o)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list sites: %w", err)
	}
	_ = rows.Close()
	for _, o := range others {
		if _, err := s.syncVersionSizes(ctx, o); err != nil {
			return fmt.Errorf("measure site %s: %w", o.Name, err)
		}
	}
	return nil
}

// checkQuota decides whether deploying a version of size bytes to site fits the site and
// owner quotas. It counts the state after the deploy and the prune that follows it, so a
// site at its limit can still replace its oldest version: the one version that prune will
// remove is not counted. If the site holds more than keep_versions versions, because an
// earlier prune failed, it first tries to prune; whatever is left over is counted in full,
// with no credit. A failure to measure anything is an error, never a pass. It returns a
// *quotaError if the deploy does not fit. The caller holds deployMu.
func (s *Server) checkQuota(ctx context.Context, site Site, size int64) error {
	if s.cfg.SiteQuota == 0 && s.cfg.UserQuota == 0 {
		return nil
	}
	keep := max(s.cfg.KeepVersions, 1)
	versions, err := s.Versions(site.Name)
	if err != nil {
		return err
	}
	if len(versions) > keep {
		if err := s.pruneVersions(site.Name); err != nil {
			ctxLogger(ctx).Warn("prune old versions", "site", site.Name, "err", err)
		}
	}
	sizes, err := s.syncVersionSizes(ctx, site)
	if err != nil {
		return err
	}
	if versions, err = s.Versions(site.Name); err != nil {
		return err
	}
	after := size
	for _, v := range versions {
		after += sizes[v]
	}
	if len(versions) == keep { // the deploy's prune removes the oldest
		after -= sizes[versions[0]]
	}
	if q := s.cfg.SiteQuota; q > 0 && after > q {
		return &quotaError{fmt.Sprintf("site quota exceeded: %s would hold %s after this deploy, over the limit of %s per site",
			site.Name, humanSize(after), humanSize(q))}
	}
	if q := s.cfg.UserQuota; q > 0 && site.OwnerID != 0 {
		// The other sites' rows may be missing (a failed backfill or interrupted accounting),
		// so measure them again before trusting the sum.
		if err := s.syncOtherSites(ctx, site); err != nil {
			return err
		}
		others, err := s.otherSitesBytes(ctx, site.OwnerID, site.ID)
		if err != nil {
			return fmt.Errorf("query user usage: %w", err)
		}
		if others+after > q {
			return &quotaError{fmt.Sprintf("user quota exceeded: your sites would hold %s after this deploy, over the limit of %s in total",
				humanSize(others+after), humanSize(q))}
		}
	}
	return nil
}

// siteBytes returns the bytes held by the kept versions of the site with the given id.
func (s *Server) siteBytes(ctx context.Context, id int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(bytes), 0) FROM site_version WHERE docid = ?", id).Scan(&n)
	return n, err
}

// userUsage returns the bytes held by all sites the user owns and how many sites that is.
func (s *Server) userUsage(ctx context.Context, user int64) (bytes int64, sites int, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE((SELECT SUM(v.bytes) FROM site_version v JOIN docs d ON d.docid = v.docid WHERE d.uowner = ?), 0),
		        (SELECT COUNT(*) FROM docs WHERE uowner = ?)`, user, user).Scan(&bytes, &sites)
	return bytes, sites, err
}

// refreshStoredBytes recounts the bytes held by all sites for the metrics gauge.
func (s *Server) refreshStoredBytes(ctx context.Context) {
	var n int64
	if err := s.db.QueryRowContext(ctx, "SELECT COALESCE(SUM(bytes), 0) FROM site_version").Scan(&n); err != nil {
		ctxLogger(ctx).Warn("count stored bytes", "err", err)
		return
	}
	s.metrics.SetStoredBytes(n)
}

// backfillVersionSizes measures the versions of existing sites that have no size yet, so
// usage is complete from the first request. A site that fails is logged and retried at its
// next deploy.
func (s *Server) backfillVersionSizes(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, "SELECT docid, name, uowner FROM docs")
	if err != nil {
		return fmt.Errorf("list sites: %w", err)
	}
	defer rows.Close()
	var all []Site
	for rows.Next() {
		var site Site
		var owner sql.NullInt64
		if err := rows.Scan(&site.ID, &site.Name, &owner); err != nil {
			return fmt.Errorf("scan site: %w", err)
		}
		site.OwnerID = owner.Int64
		all = append(all, site)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("list sites: %w", err)
	}
	_ = rows.Close()
	s.deployMu.Lock()
	defer s.deployMu.Unlock()
	for _, site := range all {
		if _, err := s.syncVersionSizes(ctx, site); err != nil {
			ctxLogger(ctx).Warn("measure site", "site", site.Name, "err", err)
		}
	}
	s.refreshStoredBytes(ctx)
	return nil
}
