// Package api embeds the OpenAPI description of the HTTP API, so the server can serve it
// and tests can check it against the registered routes.
package api

import _ "embed"

// OpenAPI is the OpenAPI 3.1 document in openapi.yaml.
//
//go:embed openapi.yaml
var OpenAPI []byte
