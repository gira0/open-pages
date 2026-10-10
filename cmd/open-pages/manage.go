package main

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gira0/open-pages/internal/names"
)

// siteUpdate is the body of PUT /v1/auth/sites/{name}. Absent fields are left unchanged;
// "group": 0 removes the site's group.
type siteUpdate struct {
	Description *string `json:"description"`
	Group       *int64  `json:"group"`
	Visibility  *string `json:"visibility"`
}

const errBadVisibility = `visibility must be "public", "authenticated" or "restricted"`

// handleSiteUpdate changes a site's description and/or group. Owner only.
func (s *Server) handleSiteUpdate(w http.ResponseWriter, r *http.Request) {
	site, ok := s.authorizeSite(w, r)
	if !ok {
		return
	}
	var d siteUpdate
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if d.Description != nil && len(*d.Description) > 512 {
		writeError(w, http.StatusBadRequest, "description must be at most 512 characters")
		return
	}
	if d.Visibility != nil && !validVisibility(*d.Visibility) {
		writeError(w, http.StatusBadRequest, errBadVisibility)
		return
	}
	updated, err := s.updateSite(r.Context(), site, d.Description, d.Group, d.Visibility)
	if errors.Is(err, errGroupNotFound) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, errNotGroupMember) {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	if errors.Is(err, errSiteNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		internalError(w, "update site", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": site.Name, "description": updated.Description, "group": updated.GroupID, "visibility": updated.Visibility,
	})
}

// handleSiteGet returns a site's metadata to the people who may view it: everyone logged in
// for a public or authenticated site, the owner and group members for a restricted one. Other callers get 403
// (the manage endpoints already tell logged-in users that a site exists).
func (s *Server) handleSiteGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !names.ValidSite(name) {
		writeError(w, http.StatusNotFound, errSiteNotFound.Error())
		return
	}
	site, err := s.getSite(r.Context(), name)
	if errors.Is(err, errSiteNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		internalError(w, "load site", err)
		return
	}
	ok, err := s.canView(r.Context(), site, userID(r))
	if err != nil {
		internalError(w, "check site access", err)
		return
	}
	if !ok {
		writeError(w, http.StatusForbidden, "you may not view this site")
		return
	}
	current, err := s.CurrentVersion(site.Name)
	if err != nil {
		internalError(w, "read current version", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name": site.Name, "description": site.Description, "owner_id": nullableID(site.OwnerID),
		"group": site.GroupID, "visibility": site.Visibility, "current": current,
	})
}

// handlePublicSites lists the public sites. No login needed; restricted sites never appear.
func (s *Server) handlePublicSites(w http.ResponseWriter, r *http.Request) {
	sites, err := s.listPublicSites(r.Context())
	if err != nil {
		internalError(w, "list public sites", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sites": sites})
}

// handleSiteDelete removes a site, its versions and its database row. Owner only.
func (s *Server) handleSiteDelete(w http.ResponseWriter, r *http.Request) {
	site, ok := s.authorizeSite(w, r)
	if !ok {
		return
	}
	err := s.deleteSite(r.Context(), site)
	if errors.Is(err, errSiteNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		internalError(w, "delete site", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "site deleted", "name": site.Name})
}

// handleVersions lists the versions kept on disk, newest first, and which one is live.
func (s *Server) handleVersions(w http.ResponseWriter, r *http.Request) {
	site, ok := s.authorizeSite(w, r)
	if !ok {
		return
	}
	versions, current, err := s.versionsAndCurrent(site.Name)
	if err != nil {
		internalError(w, "list versions", err)
		return
	}
	out := make([]string, 0, len(versions))
	for i := len(versions) - 1; i >= 0; i-- {
		out = append(out, versions[i])
	}
	writeJSON(w, http.StatusOK, map[string]any{"site": site.Name, "current": current, "versions": out})
}

// handleRollback makes an existing version the live one. Owner only.
func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	site, ok := s.authorizeSite(w, r)
	if !ok {
		return
	}
	var d struct {
		Version string `json:"version"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil || d.Version == "" {
		writeError(w, http.StatusBadRequest, "body must be JSON with a version id")
		return
	}
	err := s.SwitchCurrent(site.Name, d.Version)
	if errors.Is(err, errVersionAbsent) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		internalError(w, "roll back site", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "rolled back", "site": site.Name, "version": d.Version})
}
