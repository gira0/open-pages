// Package names holds the pure naming rules shared by the site and group code and the
// database layer: what a valid site name is, which site names are reserved, and how a
// group name is folded into its uniqueness key. It has no dependencies beyond the standard
// library, so any package may import it without creating a cycle.
package names

import (
	"regexp"
	"strings"
	"unicode"
)

// MaxGroupNameLen is the longest group name, in characters (runes).
const MaxGroupNameLen = 64

// siteNameRe is a DNS label: lowercase letters, digits and inner hyphens, 1 to 63 chars.
var siteNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// reservedSites can't be used as site names in either URL mode: in path mode they
// would shadow API and UI routes on the base domain, in subdomain mode they collide with
// hosts operators commonly point at the service itself (www.pages.corp, api.pages.corp).
var reservedSites = map[string]bool{
	"v1": true, "index": true, "api": true, "www": true, "admin": true, "ui": true,
	"static": true, "assets": true, "health": true, "healthz": true, "readyz": true, "metrics": true,
	"login": true, "logout": true, "register": true, "auth": true, "user": true,
	"users": true, "sites": true, "docs": true, "app": true, "mail": true,
}

// ValidSite reports whether name is usable as a site name (a DNS label).
func ValidSite(name string) bool { return siteNameRe.MatchString(name) }

// ReservedSite reports whether name is reserved and so can't be used as a site name.
func ReservedSite(name string) bool { return reservedSites[name] }

// GroupKey maps a group name to the key that must be unique: every rune is replaced by the
// smallest member of its Unicode simple case-folding orbit, so "Ä" and "ä" (and "K" and
// the Kelvin sign) share a key. The stored name keeps its original spelling.
func GroupKey(name string) string {
	var b strings.Builder
	for _, r := range name {
		low := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			low = min(low, f)
		}
		b.WriteRune(low)
	}
	return b.String()
}
