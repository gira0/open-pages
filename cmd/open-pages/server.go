package main

import (
	"database/sql"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"slices"
	"sync"

	"github.com/gira0/open-pages/api"
	"github.com/gira0/open-pages/internal/config"
)

type Server struct {
	db    *sql.DB
	cfg   config.Config
	tmpl  *template.Template
	sites string // directory holding extracted uploads
	tmp   string // staging directory for extraction

	deployMu sync.Mutex // serializes switching and pruning versions
	metrics  *metrics
	health   healthProbe
	oidc     *oidcClient // nil unless [oidc] is enabled
}

// route is one pattern the API mux registers.
type route struct {
	pattern string
	handler http.Handler
}

// routeTable lists the routes the server registers with its current configuration. The
// OpenAPI test checks it against api/openapi.yaml.
func (s *Server) routeTable() []route {
	var rt []route
	handle := func(pattern string, h http.Handler) { rt = append(rt, route{pattern, h}) }
	handleFunc := func(pattern string, h http.HandlerFunc) { handle(pattern, h) }

	handleFunc("GET /index", s.handleIndex)
	handleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"message": "pong"})
	})
	handleFunc("GET /v1/openapi.yaml", handleOpenAPI)
	handleFunc("GET /v1/sites", s.handlePublicSites)
	handleFunc("GET /healthz", s.handleHealth)
	if s.cfg.MetricsToken != "" {
		handleFunc("GET /metrics", s.handleMetrics)
	}
	if s.cfg.LocalLogin {
		handleFunc("POST /v1/user/register", s.handleRegister)
		handleFunc("POST /v1/user/login", s.handleLogin)
	}
	if s.oidc != nil {
		handleFunc("GET /v1/auth/oidc/login", s.handleOIDCLogin)
		handleFunc("GET /v1/auth/oidc/callback", s.handleOIDCCallback)
	}

	handle("GET /v1/auth/user", s.requireAuth(s.handleUserInfo))
	handle("POST /v1/auth/logout", s.requireSession(s.handleLogout))
	// Token management needs a real session: an API token can't mint or revoke tokens.
	handle("POST /v1/auth/tokens", s.requireSession(s.handleTokenCreate))
	handle("GET /v1/auth/tokens", s.requireSession(s.handleTokenList))
	handle("DELETE /v1/auth/tokens/{id}", s.requireSession(s.handleTokenRevoke))
	handle("GET /v1/auth/groups", s.requireAuth(s.handleGroupList))
	handle("POST /v1/auth/groups", s.requireAuth(s.handleGroupCreate))
	handle("GET /v1/auth/groups/{id}", s.requireAuth(s.handleGroupGet))
	handle("DELETE /v1/auth/groups/{id}", s.requireAuth(s.handleGroupDelete))
	handle("POST /v1/auth/groups/{id}/members", s.requireAuth(s.handleMemberAdd))
	handle("DELETE /v1/auth/groups/{id}/members/{uid}", s.requireAuth(s.handleMemberRemove))
	handle("POST /v1/auth/sites", s.requireAuth(s.handleSiteCreate))
	handle("GET /v1/auth/sites/{name}", s.requireAuth(s.handleSiteGet))
	handle("PUT /v1/auth/sites/{name}", s.requireAuth(s.handleSiteUpdate))
	handle("DELETE /v1/auth/sites/{name}", s.requireAuth(s.handleSiteDelete))
	handle("GET /v1/auth/sites/{name}/versions", s.requireAuth(s.handleVersions))
	handle("POST /v1/auth/sites/{name}/rollback", s.requireAuth(s.handleRollback))
	handle("POST /v1/auth/sites/{name}/upload", s.requireAuth(s.handleRawUpload))
	handle("POST /v1/auth/sites/{name}/formupload", s.requireAuth(s.handleFormUpload))
	return rt
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	for _, r := range s.routeTable() {
		mux.Handle(r.pattern, r.handler)
	}
	return s.observe(s.cors(s.withSites(mux)))
}

// handleOpenAPI serves the embedded OpenAPI document. It needs no login.
func handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	if _, err := w.Write(api.OpenAPI); err != nil {
		ctxLogger(r.Context()).Warn("write openapi", "err", err)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"title": "Posts", "localLogin": s.cfg.LocalLogin, "oidc": s.oidc != nil}
	if err := s.tmpl.ExecuteTemplate(w, "index.html", data); err != nil {
		ctxLogger(r.Context()).Error("render index", "err", err)
	}
}

// cors allows the configured origins to call the API with credentials.
// With no origins configured, no CORS headers are sent (same-origin only).
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && slices.Contains(s.cfg.CORSOrigins, origin) {
			h := w.Header()
			h.Set("Access-Control-Allow-Origin", origin)
			h.Set("Access-Control-Allow-Credentials", "true")
			h.Add("Vary", "Origin")
			h.Set("Access-Control-Expose-Headers", requestIDHeader)
			if r.Method == http.MethodOptions {
				h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				h.Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Authorization, X-CSRF-Token")
				h.Set("Access-Control-Max-Age", "43200")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("write response", "err", err, "request_id", requestIDOf(w))
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// internalError logs err and returns a generic 500 so internals don't leak to clients.
func internalError(w http.ResponseWriter, msg string, err error) {
	slog.Error(msg, "err", err, "request_id", requestIDOf(w))
	writeError(w, http.StatusInternalServerError, "internal error")
}
