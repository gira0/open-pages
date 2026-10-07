package main

import (
	"net"
	"net/http"
	"strings"
)

// reservedSiteNames can't be used as site names: in path mode they would shadow the
// API and UI routes that live on the same host.
var reservedSiteNames = map[string]bool{"v1": true, "index": true}

func reservedSiteName(name string) bool { return reservedSiteNames[name] }

// siteFromRequest works out which site a request is for and which file inside it.
// site is "" when the request is not for a site (it belongs to the API or UI).
// filePath is the slash-rooted path within the site, not yet cleaned or checked.
func (s *Server) siteFromRequest(r *http.Request) (site, filePath string) {
	if s.cfg.URLMode == urlModeSubdomain {
		// Subdomain mode: <site>.<base_domain>/<file path>
		label, under := s.subdomainOf(r.Host)
		if !under || !validSiteName(label) {
			return "", ""
		}
		return label, r.URL.Path
	}
	// Path mode: /<site>/<file path>
	first, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !validSiteName(first) || reservedSiteName(first) {
		return "", ""
	}
	return first, "/" + rest
}

// subdomainOf splits host (as in the Host header) against base_domain. under reports
// whether host is a proper subdomain of base_domain; label is the part in front of it,
// which may not be a valid site name (for example "a.b").
func (s *Server) subdomainOf(host string) (label string, under bool) {
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	label, ok := strings.CutSuffix(host, "."+s.cfg.BaseDomain)
	return label, ok && label != ""
}
