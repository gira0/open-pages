package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/gira0/open-pages/internal/names"
)

type siteCreate struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Group       int64  `json:"group"`      // optional group id; 0 or absent for none
	Visibility  string `json:"visibility"` // "public" (default), "authenticated" or "restricted"
}

func (s *Server) handleSiteCreate(w http.ResponseWriter, r *http.Request) {
	var d siteCreate
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !names.ValidSite(d.Name) {
		writeError(w, http.StatusBadRequest,
			"name must be a DNS label: 1 to 63 lowercase letters, digits or hyphens, not starting or ending with a hyphen")
		return
	}
	if names.ReservedSite(d.Name) {
		writeError(w, http.StatusBadRequest, "that site name is reserved")
		return
	}
	if len(d.Description) > 512 {
		writeError(w, http.StatusBadRequest, "description must be at most 512 characters")
		return
	}
	if d.Visibility == "" {
		d.Visibility = visPublic
	}
	if !validVisibility(d.Visibility) {
		writeError(w, http.StatusBadRequest, errBadVisibility)
		return
	}
	_, err := s.createSite(r.Context(), d.Name, d.Description, userID(r), d.Group, d.Visibility)
	if errors.Is(err, errGroupNotFound) {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if errors.Is(err, errNotGroupMember) {
		writeError(w, http.StatusForbidden, err.Error())
		return
	}
	if errors.Is(err, errSiteExists) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if err != nil {
		internalError(w, "create site", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "site created", "name": d.Name, "visibility": d.Visibility})
}

// authorizeSite loads the site named in the URL and checks the caller owns it,
// writing the error response itself when it returns false.
func (s *Server) authorizeSite(w http.ResponseWriter, r *http.Request) (Site, bool) {
	name := r.PathValue("name")
	if !names.ValidSite(name) {
		writeError(w, http.StatusNotFound, errSiteNotFound.Error())
		return Site{}, false
	}
	site, err := s.getSite(r.Context(), name)
	if errors.Is(err, errSiteNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return Site{}, false
	}
	if err != nil {
		internalError(w, "load site", err)
		return Site{}, false
	}
	if site.OwnerID != userID(r) {
		writeError(w, http.StatusForbidden, "you do not own this site")
		return Site{}, false
	}
	return site, true
}

// handleRawUpload accepts an archive as the raw request body.
func (s *Server) handleRawUpload(w http.ResponseWriter, r *http.Request) {
	site, ok := s.authorizeSite(w, r)
	if !ok {
		return
	}
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	s.storeUpload(w, r, site, body)
}

// handleFormUpload accepts an archive in the multipart field "file".
func (s *Server) handleFormUpload(w http.ResponseWriter, r *http.Request) {
	site, ok := s.authorizeSite(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+1<<20)
	f, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing or invalid file field")
		return
	}
	defer f.Close()
	defer func() { _ = r.MultipartForm.RemoveAll() }()
	s.storeUpload(w, r, site, f)
}

// storeUpload spools the archive to disk and deploys it as a new version of site.
func (s *Server) storeUpload(w http.ResponseWriter, r *http.Request, site Site, src io.Reader) {
	spool, err := os.CreateTemp(s.tmp, "upload-*")
	if err != nil {
		internalError(w, "create spool file", err)
		return
	}
	defer os.Remove(spool.Name())
	defer spool.Close()

	if _, err := io.Copy(spool, src); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("upload exceeds %d MiB", s.cfg.MaxUploadBytes>>20))
			return
		}
		writeError(w, http.StatusBadRequest, "failed to read upload")
		return
	}

	version, err := s.deploy(r.Context(), site, spool)
	if errors.Is(err, errSiteNotFound) {
		s.metrics.recordDeploy(deployFailed)
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	var bad *badArchiveError
	if errors.As(err, &bad) {
		s.metrics.recordDeploy(deployRejected)
		ctxLogger(r.Context()).Info("rejected upload", "site", site.Name)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err != nil {
		s.metrics.recordDeploy(deployFailed)
		internalError(w, "deploy site", err)
		return
	}
	s.metrics.recordDeploy(deployOK)
	writeJSON(w, http.StatusOK, map[string]string{"status": "File uploaded", "site": site.Name, "version": version})
}
