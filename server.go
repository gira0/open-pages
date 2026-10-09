package main

import (
	"database/sql"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"slices"
	"sync"
)

type Server struct {
	db    *sql.DB
	cfg   Config
	tmpl  *template.Template
	sites string // directory holding extracted uploads
	tmp   string // staging directory for extraction

	deployMu sync.Mutex // serializes switching and pruning versions
	metrics  *metrics
	health   healthProbe
	oidc     *oidcClient // nil unless [oidc] is enabled
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /index", s.handleIndex)
	mux.HandleFunc("GET /v1/ping", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"message": "pong"})
	})
	mux.HandleFunc("GET /v1/sites", s.handlePublicSites)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	if s.cfg.MetricsToken != "" {
		mux.HandleFunc("GET /metrics", s.handleMetrics)
	}
	if s.cfg.LocalLogin {
		mux.HandleFunc("POST /v1/user/register", s.handleRegister)
		mux.HandleFunc("POST /v1/user/login", s.handleLogin)
	}
	if s.oidc != nil {
		mux.HandleFunc("GET /v1/auth/oidc/login", s.handleOIDCLogin)
		mux.HandleFunc("GET /v1/auth/oidc/callback", s.handleOIDCCallback)
	}

	mux.Handle("GET /v1/auth/user", s.requireAuth(s.handleUserInfo))
	mux.Handle("POST /v1/auth/logout", s.requireSession(s.handleLogout))
	// Token management needs a real session: an API token can't mint or revoke tokens.
	mux.Handle("POST /v1/auth/tokens", s.requireSession(s.handleTokenCreate))
	mux.Handle("GET /v1/auth/tokens", s.requireSession(s.handleTokenList))
	mux.Handle("DELETE /v1/auth/tokens/{id}", s.requireSession(s.handleTokenRevoke))
	mux.Handle("GET /v1/auth/groups", s.requireAuth(s.handleGroupList))
	mux.Handle("POST /v1/auth/groups", s.requireAuth(s.handleGroupCreate))
	mux.Handle("GET /v1/auth/groups/{id}", s.requireAuth(s.handleGroupGet))
	mux.Handle("DELETE /v1/auth/groups/{id}", s.requireAuth(s.handleGroupDelete))
	mux.Handle("POST /v1/auth/groups/{id}/members", s.requireAuth(s.handleMemberAdd))
	mux.Handle("DELETE /v1/auth/groups/{id}/members/{uid}", s.requireAuth(s.handleMemberRemove))
	mux.Handle("POST /v1/auth/sites", s.requireAuth(s.handleSiteCreate))
	mux.Handle("GET /v1/auth/sites/{name}", s.requireAuth(s.handleSiteGet))
	mux.Handle("PUT /v1/auth/sites/{name}", s.requireAuth(s.handleSiteUpdate))
	mux.Handle("DELETE /v1/auth/sites/{name}", s.requireAuth(s.handleSiteDelete))
	mux.Handle("GET /v1/auth/sites/{name}/versions", s.requireAuth(s.handleVersions))
	mux.Handle("POST /v1/auth/sites/{name}/rollback", s.requireAuth(s.handleRollback))
	mux.Handle("POST /v1/auth/sites/{name}/upload", s.requireAuth(s.handleRawUpload))
	mux.Handle("POST /v1/auth/sites/{name}/formupload", s.requireAuth(s.handleFormUpload))

	return s.observe(s.cors(s.withSites(mux)))
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if err := s.tmpl.ExecuteTemplate(w, "index.html", map[string]string{"title": "Posts"}); err != nil {
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
