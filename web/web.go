// Package web embeds the UI templates and static assets, so the binary does not depend on
// the working directory.
package web

import "embed"

// Templates holds the HTML templates under templates/.
//
//go:embed templates/*.html
var Templates embed.FS

// Static holds the files under static/: our own scripts and styles plus the vendored
// PatternFly CSS and fonts (see scripts/vendor-patternfly.sh).
//
//go:embed static
var Static embed.FS
