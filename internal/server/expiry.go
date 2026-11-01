package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// Expiring sites. A site can carry an expiry time (docs.expires_at); a background task
// deletes expired sites with the same code path as an owner's delete. This is what preview
// deployments rely on: CI deploys a branch to "<site>--<branch>" with an expiry, renews it
// on every push and deletes it when the branch closes, and the reaper catches whatever CI
// never cleaned up.

const (
	// maxExpiryDays is the longest expiry a request may set.
	maxExpiryDays = 365
	// expiryReapEvery is how often expired sites are deleted. Expiry is therefore
	// accurate to this interval; an expired site may be served until the next run.
	expiryReapEvery = 10 * time.Minute
)

var errBadExpiry = fmt.Errorf("expires_in_days must be a whole number from 0 (never) to %d", maxExpiryDays)

// expiryFromDays turns a number of days from now into a Unix time; 0 means never (0).
func expiryFromDays(days int, now time.Time) (int64, error) {
	if days < 0 || days > maxExpiryDays {
		return 0, errBadExpiry
	}
	if days == 0 {
		return 0, nil
	}
	return now.Add(time.Duration(days) * 24 * time.Hour).Unix(), nil
}

// setExpiry sets the site's expiry (0 clears it) and records the change in the audit log.
// Like updateSite it matches the site's id, name and owner, and returns errSiteNotFound if
// the site was deleted or replaced meanwhile.
func (s *Server) setExpiry(ctx context.Context, site Site, expiresAt int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	res, err := tx.ExecContext(ctx, "UPDATE docs SET expires_at = ? WHERE docid = ? AND name = ? AND uowner IS ?",
		nullableID(expiresAt), site.ID, site.Name, nullableID(site.OwnerID))
	if err != nil {
		return fmt.Errorf("set expiry: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errSiteNotFound
	}
	emit := s.auditRecord(ctx, tx, auditEvent{
		action: auditSiteUpdate, targetKind: targetSite, target: site.Name, siteID: site.ID,
		details: map[string]any{"changes": map[string]any{"expires_at": map[string]any{"from": site.ExpiresAt, "to": expiresAt}}},
	})
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	emit()
	return nil
}

// reapExpired deletes every site whose expiry has passed and returns how many it removed.
// A site that fails to delete is logged and retried at the next run.
func (s *Server) reapExpired(ctx context.Context, now time.Time) (int, error) {
	rows, err := s.db.QueryContext(ctx,
		"SELECT docid, name, uowner FROM docs WHERE expires_at IS NOT NULL AND expires_at <= ?", now.Unix())
	if err != nil {
		return 0, fmt.Errorf("list expired sites: %w", err)
	}
	var expired []Site
	for rows.Next() {
		var (
			site  Site
			owner *int64
		)
		if err := rows.Scan(&site.ID, &site.Name, &owner); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("scan expired site: %w", err)
		}
		if owner != nil {
			site.OwnerID = *owner
		}
		expired = append(expired, site)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("list expired sites: %w", err)
	}
	_ = rows.Close()

	removed := 0
	for _, site := range expired {
		if ctx.Err() != nil {
			break
		}
		// The site may have been renewed since the listing; deleteSite only matches the
		// exact id, name and owner, so re-check the expiry under the same conditions.
		cur, getErr := s.getSite(ctx, site.Name)
		if errors.Is(getErr, errSiteNotFound) {
			continue
		}
		if getErr != nil {
			slog.Error("load expired site", "site", site.Name, "err", getErr)
			continue
		}
		if cur.ID != site.ID || cur.ExpiresAt == 0 || cur.ExpiresAt > now.Unix() {
			continue
		}
		switch delErr := s.deleteSite(ctx, cur); {
		case delErr == nil:
			removed++
			slog.Info("deleted expired site", "site", site.Name)
		case errors.Is(delErr, errSiteNotFound):
		default:
			slog.Error("delete expired site", "site", site.Name, "err", delErr)
		}
	}
	return removed, nil
}

// startExpiryReaper reaps once now and then every expiryReapEvery until Close.
func (s *Server) startExpiryReaper(ctx context.Context) {
	run := func() {
		if _, err := s.reapExpired(ctx, time.Now()); err != nil && ctx.Err() == nil {
			slog.Error("reap expired sites", "err", err)
		}
	}
	run()
	s.background.Go(func() {
		t := time.NewTicker(expiryReapEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				run()
			}
		}
	})
}
