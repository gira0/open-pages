package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gira0/open-pages/internal/names"
)

// Custom domains. A site can answer to host names of its own, such as handbook.corp, in
// addition to <site>.<content_domain>. open-pages never terminates TLS for them: a reverse
// proxy (nginx, Traefik, the OpenShift router) holds the certificate, routes the host to
// open-pages and passes the Host header through unchanged. The server maps the Host header to
// a site, so everything a site host does (read-only GET and HEAD, per-site login, the
// reserved /__open-pages/ prefix) applies on a custom domain too.
//
// A custom domain is a host of its own, so it is an origin of its own. Site cookies are
// host-only, which keeps every domain's session apart from the app and from every other
// host. What stops a user from claiming a host name they should not have is
// sites.custom_domain_suffixes: a domain must be a strict subdomain of one of them.

var (
	errDomainTaken   = errors.New("that domain is already used by another site")
	errDomainLimit   = errors.New("the site already has the most custom domains it may have")
	errDomainsClosed = errors.New("custom domains are not enabled")
)

// errBadDomain is a domain the rules refuse, with the reason in its message.
type errBadDomain string

func (e errBadDomain) Error() string { return string(e) }

// normalizeDomain lowercases a user-supplied domain and drops a trailing dot.
func normalizeDomain(raw string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
}

// checkDomain returns nil if domain, already normalized, may be claimed: a valid host name
// that is neither a host of the app nor of the sites, and a strict subdomain of one of the
// allowed suffixes.
func (s *Server) checkDomain(domain string) error {
	cd := s.cfg.CustomDomains
	if !cd.Enabled {
		return errDomainsClosed
	}
	if !names.ValidDomain(domain) {
		return errBadDomain("domain must be a host name of at least two DNS labels in lowercase letters, digits and hyphens, without port, path or IP address, and without xn-- labels")
	}
	for _, own := range []string{s.cfg.AppHost, s.cfg.ContentDomain} {
		if domain == own || strings.HasSuffix(domain, "."+own) {
			return errBadDomain("that domain belongs to open-pages itself (the app host or the content domain)")
		}
	}
	for _, suffix := range cd.AllowedSuffixes {
		if strings.HasSuffix(domain, "."+suffix) {
			return nil
		}
	}
	return errBadDomain("domain must be a subdomain of one of: " + strings.Join(cd.AllowedSuffixes, ", "))
}

// listDomains returns the custom domains of the site with the given id, ordered by name. It
// is never nil.
func (s *Server) listDomains(ctx context.Context, siteID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT domain FROM site_domain WHERE siteid = ? ORDER BY domain", siteID)
	if err != nil {
		return nil, fmt.Errorf("query domains: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, fmt.Errorf("scan domain: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// addDomain attaches domain (already normalized and checked) to site. created is false when
// the site already had it. It returns errDomainTaken if another site has it, errDomainLimit
// at the per-site maximum and errSiteNotFound if the site was deleted meanwhile.
func (s *Server) addDomain(ctx context.Context, site Site, domain string) (created bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	// The insert is conditional on the site still being this row. It also takes the write
	// lock, so the count below is exact and two requests cannot both pass the limit.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO site_domain (siteid, domain, created)
		 SELECT docid, ?, ? FROM docs WHERE docid = ? AND name = ?
		 ON CONFLICT (domain) DO NOTHING`,
		domain, time.Now().Unix(), site.ID, site.Name)
	if err != nil {
		return false, fmt.Errorf("insert domain: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var owner int64
		err := tx.QueryRowContext(ctx, "SELECT siteid FROM site_domain WHERE domain = ?", domain).Scan(&owner)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return false, errSiteNotFound
		case err != nil:
			return false, fmt.Errorf("read domain: %w", err)
		case owner == site.ID:
			return false, nil
		default:
			return false, errDomainTaken
		}
	}
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM site_domain WHERE siteid = ?", site.ID).Scan(&n); err != nil {
		return false, fmt.Errorf("count domains: %w", err)
	}
	if n > s.cfg.CustomDomains.PerSite {
		return false, errDomainLimit
	}
	emit := s.auditRecord(ctx, tx, auditEvent{
		action: auditDomainAdd, targetKind: targetSite, target: site.Name, siteID: site.ID,
		details: map[string]any{"domain": domain},
	})
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	emit()
	return true, nil
}

// removeDomain detaches domain from site. It reports false if the site did not have it.
func (s *Server) removeDomain(ctx context.Context, site Site, domain string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit
	res, err := tx.ExecContext(ctx, "DELETE FROM site_domain WHERE siteid = ? AND domain = ?", site.ID, domain)
	if err != nil {
		return false, fmt.Errorf("delete domain: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	emit := s.auditRecord(ctx, tx, auditEvent{
		action: auditDomainRemove, targetKind: targetSite, target: site.Name, siteID: site.ID,
		details: map[string]any{"domain": domain},
	})
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	emit()
	return true, nil
}

// siteForDomain returns the name of the site that owns the custom domain host (normalized),
// or errSiteNotFound.
func (s *Server) siteForDomain(ctx context.Context, host string) (string, error) {
	var site string
	err := s.db.QueryRowContext(ctx,
		"SELECT docs.name FROM site_domain JOIN docs ON docs.docid = site_domain.siteid WHERE site_domain.domain = ?",
		host).Scan(&site)
	if errors.Is(err, sql.ErrNoRows) {
		return "", errSiteNotFound
	}
	if err != nil {
		return "", fmt.Errorf("look up domain: %w", err)
	}
	return site, nil
}

// siteHasDomain reports whether host is a custom domain of site.
func (s *Server) siteHasDomain(ctx context.Context, site, host string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		"SELECT 1 FROM site_domain JOIN docs ON docs.docid = site_domain.siteid WHERE docs.name = ? AND site_domain.domain = ?",
		site, host).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// siteFromCustomHost returns the site a normalized host is a custom domain of. ok is false
// for any other host, including when the feature is off or the lookup fails (logged).
func (s *Server) siteFromCustomHost(r *http.Request, host string) (site string, ok bool) {
	if !s.cfg.CustomDomains.Enabled || !names.ValidDomain(host) {
		return "", false
	}
	site, err := s.siteForDomain(r.Context(), host)
	if err != nil {
		if !errors.Is(err, errSiteNotFound) {
			slog.Error("look up custom domain", "err", err)
		}
		return "", false
	}
	return site, true
}

// handleDomainList lists the custom domains of a site and what may be added. Owner only.
func (s *Server) handleDomainList(w http.ResponseWriter, r *http.Request) {
	site, ok := s.authorizeSite(w, r)
	if !ok {
		return
	}
	domains, err := s.listDomains(r.Context(), site.ID)
	if err != nil {
		internalError(w, "list domains", err)
		return
	}
	cd := s.cfg.CustomDomains
	writeJSON(w, http.StatusOK, map[string]any{
		"site": site.Name, "domains": domains, "allowed_suffixes": cd.AllowedSuffixes, "limit": cd.PerSite,
	})
}

// handleDomainAdd attaches a custom domain to a site. Owner only. It answers 201 for a new
// domain and 200 when the site already had it, so a retry is harmless.
func (s *Server) handleDomainAdd(w http.ResponseWriter, r *http.Request) {
	site, ok := s.authorizeSite(w, r)
	if !ok {
		return
	}
	domain := normalizeDomain(r.PathValue("domain"))
	if err := s.checkDomain(domain); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := s.addDomain(r.Context(), site, domain)
	switch {
	case errors.Is(err, errDomainTaken):
		writeError(w, http.StatusConflict, err.Error())
		return
	case errors.Is(err, errDomainLimit):
		writeError(w, http.StatusForbidden, fmt.Sprintf("a site may have at most %d custom domains", s.cfg.CustomDomains.PerSite))
		return
	case errors.Is(err, errSiteNotFound):
		writeError(w, http.StatusNotFound, err.Error())
		return
	case err != nil:
		internalError(w, "add domain", err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, map[string]string{"site": site.Name, "domain": domain})
}

// handleDomainRemove detaches a custom domain from a site. Owner only.
func (s *Server) handleDomainRemove(w http.ResponseWriter, r *http.Request) {
	site, ok := s.authorizeSite(w, r)
	if !ok {
		return
	}
	domain := normalizeDomain(r.PathValue("domain"))
	removed, err := s.removeDomain(r.Context(), site, domain)
	if err != nil {
		internalError(w, "remove domain", err)
		return
	}
	if !removed {
		writeError(w, http.StatusNotFound, "the site has no such custom domain")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "domain removed", "site": site.Name, "domain": domain})
}
