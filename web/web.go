// Package web embeds the UI templates, so the binary does not depend on the working
// directory.
package web

import "embed"

// Templates holds the HTML templates under templates/.
//
//go:embed templates/*.html
var Templates embed.FS
