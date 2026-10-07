package main

import (
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
	// Path mode: /<site>/<file path>
	first, rest, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !validSiteName(first) || reservedSiteName(first) {
		return "", ""
	}
	return first, "/" + rest
}
