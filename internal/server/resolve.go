package server

import (
	"net"
	"net/http"
	"strings"

	"github.com/gira0/open-pages/internal/names"
)

// normalizeHost turns a Host header into a bare lowercase host name: no port, no
// trailing dot.
func normalizeHost(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.TrimSuffix(host, "."))
}

// siteFromHost returns the site a normalized host names, which is the label in front of
// content_domain. ok is false for any other host, including labels that aren't valid
// site names (for example "a.b").
func (s *Server) siteFromHost(host string) (site string, ok bool) {
	label, under := strings.CutSuffix(host, "."+s.cfg.ContentDomain)
	if !under || !names.ValidSite(label) {
		return "", false
	}
	return label, true
}

// dispatch sends a request to the part of the server its host belongs to:
//   - app_host: the API, the UI and static assets, never site content;
//   - <site>.<content_domain>: that site, read-only, and nothing else;
//   - any other host: only GET /livez, /readyz and /healthz, so probes by IP or localhost keep working;
//     everything else is a 404 and a stray Host header never reaches the API.
func (s *Server) dispatch(app, probes http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := normalizeHost(r.Host)
		if host == s.cfg.AppHost {
			app.ServeHTTP(w, r)
			return
		}
		if site, ok := s.siteFromHost(host); ok {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				w.Header().Set("Allow", "GET, HEAD")
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if reservedPath(r.URL.Path) {
				s.serveReserved(w, r, site)
				return
			}
			s.serveSite(w, r, site, r.URL.Path)
			return
		}
		probes.ServeHTTP(w, r)
	})
}
