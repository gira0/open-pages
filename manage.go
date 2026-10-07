package main

import (
	"encoding/json"
	"errors"
	"net/http"
)

// siteUpdate is the body of PUT /v1/auth/sites/{name}. Absent fields are left unchanged;
// "group": 0 removes the site's group.
type siteUpdate struct {
	Description *string `json:"description"`
	Group       *int64  `json:"group"`
}

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
	description, group, err := s.updateSite(r.Context(), site.ID, d.Description, d.Group)
	if errors.Is(err, errGroupNotFound) {
		writeError(w, http.StatusBadRequest, err.Error())
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
	writeJSON(w, http.StatusOK, map[string]any{"name": site.Name, "description": description, "group": group})
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
