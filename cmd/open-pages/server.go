package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"sync"

	"github.com/gira0/open-pages/api"
	"github.com/gira0/open-pages/internal/config"
)

type Server struct {
	db     *sql.DB
	cfg    config.Config
	pages  map[string]*template.Template // UI pages by file name, each with the layout
	static *staticAssets
	sites  string // directory holding extracted uploads
	tmp    string // staging directory for extraction

	deployMu  sync.Mutex    // serializes switching and pruning versions
	deploySem chan struct{} // one token per running extraction; cap is cfg.MaxDeploys
	metrics   *metrics
	health    healthProbe
	oidc      *oidcClient // nil unless [oidc] is enabled
	saml      *samlClient // nil unless [saml] is enabled
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
	handleFunc("GET /static/{hash}/{path...}", s.handleStatic)
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
	if s.saml != nil {
		handleFunc("GET /v1/auth/saml/login", s.handleSAMLLogin)
		handleFunc("POST /v1/auth/saml/acs", s.handleSAMLACS)
		handleFunc("GET /v1/auth/saml/metadata", s.handleSAMLMetadata)
	}

	handleFunc("GET /v1/auth/site-login", s.handleSiteLogin)
	handle("GET /v1/auth/user", s.requireAuth(s.handleUserInfo))
	handle("POST /v1/auth/logout", s.requireSession(s.handleLogout))
	// Token management needs a real session: an API token can't mint or revoke tokens.
	handle("POST /v1/auth/tokens", s.requireSession(requireJSON(s.handleTokenCreate)))
	handle("GET /v1/auth/tokens", s.requireSession(s.handleTokenList))
	handle("DELETE /v1/auth/tokens/{id}", s.requireSession(s.handleTokenRevoke))
	handle("GET /v1/auth/groups", s.requireAuth(s.handleGroupList))
	handle("POST /v1/auth/groups", s.requireAuth(requireJSON(s.handleGroupCreate)))
	handle("GET /v1/auth/groups/{id}", s.requireAuth(s.handleGroupGet))
	handle("DELETE /v1/auth/groups/{id}", s.requireAuth(s.handleGroupDelete))
	handle("POST /v1/auth/groups/{id}/members", s.requireAuth(requireJSON(s.handleMemberAdd)))
	handle("DELETE /v1/auth/groups/{id}/members/{uid}", s.requireAuth(s.handleMemberRemove))
	handle("POST /v1/auth/sites", s.requireAuth(requireJSON(s.handleSiteCreate)))
	handle("GET /v1/auth/sites/{name}", s.requireAuth(s.handleSiteGet))
	handle("PUT /v1/auth/sites/{name}", s.requireAuth(requireJSON(s.handleSiteUpdate)))
	handle("DELETE /v1/auth/sites/{name}", s.requireAuth(s.handleSiteDelete))
	handle("GET /v1/auth/sites/{name}/versions", s.requireAuth(s.handleVersions))
	handle("POST /v1/auth/sites/{name}/rollback", s.requireAuth(requireJSON(s.handleRollback)))
	handle("POST /v1/auth/sites/{name}/upload", s.requireAuth(s.handleRawUpload))
	handle("POST /v1/auth/sites/{name}/formupload", s.requireAuth(s.handleFormUpload))
	return rt
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	for _, r := range s.routeTable() {
		mux.Handle(r.pattern, r.handler)
	}
	// Hosts that aren't the app host answer only health probes (see dispatch).
	probes := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			r.Pattern = "GET /healthz" // what ServeMux records; observe keeps probes out of the info log
			s.handleHealth(w, r)
			return
		}
		http.NotFound(w, r)
	})
	return s.observe(s.dispatch(s.cors(s.csrf(mux)), probes))
}

// handleOpenAPI serves the embedded OpenAPI document. It needs no login.
func handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/yaml")
	if _, err := w.Write(api.OpenAPI); err != nil {
		ctxLogger(r.Context()).Warn("write openapi", "err", err)
	}
}

// indexSite is one row of the sites table on the index page.
type indexSite struct {
	Name, Description, URL string
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"title": "Sites", "nav": "sites", "localLogin": s.cfg.LocalLogin, "oidc": s.oidc != nil, "saml": s.saml != nil,
	}
	sites, err := s.listPublicSites(r.Context())
	if err != nil {
		ctxLogger(r.Context()).Error("list public sites for index", "err", err)
		data["sitesError"] = true
	}
	rows := make([]indexSite, 0, len(sites))
	for _, p := range sites {
		rows = append(rows, indexSite{p.Name, p.Description, s.originFor(p.Name+"."+s.cfg.ContentDomain, r) + "/"})
	}
	data["sites"] = rows
	s.render(w, r, "index.html", data)
}

// uiCSP is the Content-Security-Policy of the UI pages. It is never sent with hosted
// sites: their authors decide what they load.
const uiCSP = "default-src 'self'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"

// render executes a UI page with the security headers every page gets. data must be a
// map; render adds the asset URL prefix as "static".
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data map[string]any) {
	data["static"] = s.static.prefix()
	var buf bytes.Buffer
	if err := s.pages[page].ExecuteTemplate(&buf, page, data); err != nil {
		ctxLogger(r.Context()).Error("render page", "page", page, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", uiCSP)
	h.Set("X-Frame-Options", "DENY")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cache-Control", "no-cache")
	_, _ = buf.WriteTo(w)
}

// parsePages parses each page template together with the shared layout.
func parsePages(fsys fs.FS) (map[string]*template.Template, error) {
	layout, err := template.ParseFS(fsys, "templates/layout.html")
	if err != nil {
		return nil, err
	}
	files, err := fs.Glob(fsys, "templates/*.html")
	if err != nil {
		return nil, err
	}
	pages := map[string]*template.Template{}
	for _, f := range files {
		name := path.Base(f)
		if name == "layout.html" {
			continue
		}
		t, err := template.Must(layout.Clone()).ParseFS(fsys, f)
		if err != nil {
			return nil, err
		}
		pages[name] = t
	}
	return pages, nil
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
				h.Set("Access-Control-Allow-Headers", "Accept, Content-Type, Content-Length, Authorization")
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
