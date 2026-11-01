// Package names holds the pure naming rules shared by the site and group code and the
// database layer: what a valid site name is, which site names are reserved, and how a
// group name is folded into its uniqueness key. It has no dependencies beyond the standard
// library, so any package may import it without creating a cycle.
package names

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
	"unicode"
)

// MaxGroupNameLen is the longest group name, in characters (runes).
const MaxGroupNameLen = 64

// siteNameRe is a DNS label: lowercase letters, digits and inner hyphens, 1 to 63 chars.
var siteNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// reservedSites can't be used as site names: they are hosts operators commonly point at
// the service itself or at other infrastructure (www.pages-content.corp, api.pages-content.corp),
// and names that would look like an official login or admin page.
var reservedSites = map[string]bool{
	"www": true, "api": true, "app": true, "admin": true, "ui": true, "static": true,
	"assets": true, "login": true, "auth": true, "docs": true, "mail": true,
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

// previewSep separates a site name from the branch slug in a preview site's name.
const previewSep = "--"

// PreviewSite returns the name of the preview site for branch of site: "<site>--<slug>",
// where slug is the branch name folded into DNS-label characters (lowercase letters and
// digits, runs of anything else become one hyphen). A slug that had to be shortened or
// changed in any way but case gets an 8-hex-digit hash of the original branch appended, so
// "feature/x" and "feature-x" (or two very long names with a common prefix) never share a
// site. It reports false if site is not a valid site name or branch has no usable
// characters at all.
func PreviewSite(site, branch string) (string, bool) {
	if !ValidSite(site) {
		return "", false
	}
	var b strings.Builder
	lossy := false
	dash := true // suppresses a leading hyphen
	for _, r := range strings.ToLower(branch) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		default:
			if !dash {
				b.WriteByte('-')
			}
			dash = true
			if r != '-' {
				lossy = true
			}
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "", false
	}
	name := site + previewSep + slug
	if lossy || len(name) > 63 {
		sum := sha256.Sum256([]byte(branch))
		suffix := "-" + hex.EncodeToString(sum[:4])
		room := 63 - len(site) - len(previewSep) - len(suffix)
		if room < 1 {
			return "", false
		}
		name = site + previewSep + strings.Trim(slug[:min(len(slug), room)], "-") + suffix
	}
	return name, ValidSite(name)
}
