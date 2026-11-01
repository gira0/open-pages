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
		if err := ctx.Err(); err != nil {
			return removed, err // shutting down; the rest waits for the next run
		}
		if s.reapOne(ctx, site, now) {
			removed++
		}
	}
	return removed, nil
}

// reapOne deletes site if it is still the same site and still expired; the site may have
// been renewed, deleted or replaced since it was listed. Failures are logged and retried
// at the next run. It reports whether it deleted the site.
func (s *Server) reapOne(ctx context.Context, site Site, now time.Time) bool {
	cur, err := s.getSite(ctx, site.Name)
	if errors.Is(err, errSiteNotFound) {
		return false
	}
	if err != nil {
		slog.Error("load expired site", "site", site.Name, "err", err)
		return false
	}
	if cur.ID != site.ID || cur.ExpiresAt == 0 || cur.ExpiresAt > now.Unix() {
		return false
	}
	// deleteSiteWith re-checks the expiry under the database lock: a renewal that commits
	// after the getSite above must keep the site.
	if err := s.deleteSiteWith(ctx, cur, map[string]any{"reason": "expired", "expires_at": cur.ExpiresAt}, now.Unix()); err != nil {
		if !errors.Is(err, errSiteNotFound) && !errors.Is(err, errNotExpired) {
			slog.Error("delete expired site", "site", site.Name, "err", err)
		}
		return false
	}
	slog.Info("deleted expired site", "site", site.Name)
	return true
}

// startExpiryReaper reaps once at start and then every expiryReapEvery until Close.
func (s *Server) startExpiryReaper(ctx context.Context) {
	run := func() {
		if _, err := s.reapExpired(ctx, time.Now()); err != nil && ctx.Err() == nil {
			slog.Error("reap expired sites", "err", err)
		}
	}
	s.background.Go(func() {
		// The first run is here too, not in New, so a long backlog of expired previews
		// (after downtime) does not delay the listener.
		run()
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
