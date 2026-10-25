package server

import (
	"log/slog"
	"mime"
	"net/http"

	"github.com/gira0/open-pages/internal/metrics"
)

// samlACSPath is exempt from the cross-origin check: the identity provider delivers the
// response with a cross-site POST. The request is bound to the browser by the
// saml_request cookie and to the provider by the response signature instead.
const samlACSPath = "/v1/auth/saml/acs"

// csrf wraps next in Go's cross-origin protection. Unsafe methods (anything but GET, HEAD
// and OPTIONS) from a browser must be same-origin: Sec-Fetch-Site is same-origin or none, or,
// without that header, Origin matches Host. Requests carrying neither header, such as
// curl and CI, pass, as do those from the origins in cors_origins.
func (s *Server) csrf(next http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	for _, o := range s.cfg.CORSOrigins {
		_ = cop.AddTrustedOrigin(o) // config.Load has already rejected malformed origins
	}
	cop.AddInsecureBypassPattern("POST " + samlACSPath)
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the method is logged: the other values are attacker-controlled, and the
		// request log line with the same request_id carries the route.
		slog.Warn("cross-origin request rejected", "method", metrics.MethodLabel(r.Method), "request_id", requestIDOf(w))
		writeError(w, http.StatusForbidden, "cross-origin request rejected")
	}))
	return cop.Handler(next)
}

// requireJSON rejects requests whose Content-Type is not application/json with 415. A
// browser cannot send that type cross-origin without a CORS preflight, so a JSON endpoint
// is never reachable by a plain form or a "simple" fetch.
func requireJSON(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || ct != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
			return
		}
		next(w, r)
	}
}
