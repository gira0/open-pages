package main

import (
	"net"
	"net/http"
	"strings"
)

// reservedSiteNames can't be used as site names in either URL mode: in path mode they
// would shadow API and UI routes on the base domain, in subdomain mode they collide with
// hosts operators commonly point at the service itself (www.pages.corp, api.pages.corp).
var reservedSiteNames = map[string]bool{
	"v1": true, "index": true, "api": true, "www": true, "admin": true, "ui": true,
	"static": true, "assets": true, "health": true, "healthz": true, "metrics": true,
	"login": true, "logout": true, "register": true, "auth": true, "user": true,
	"users": true, "sites": true, "docs": true, "app": true, "mail": true,
}

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
