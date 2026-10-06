package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"

	"github.com/rs/xid"
)

var docNameRe = regexp.MustCompile(`^[A-Za-z0-9]{1,256}$`)

type docCreate struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (s *Server) handleDocCreate(w http.ResponseWriter, r *http.Request) {
	var d docCreate
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !docNameRe.MatchString(d.Name) {
		writeError(w, http.StatusBadRequest, "name must be 1 to 256 letters or digits")
		return
	}
	if len(d.Description) > 512 {
		writeError(w, http.StatusBadRequest, "description must be at most 512 characters")
		return
	}
	res, err := s.db.ExecContext(r.Context(),
		"INSERT INTO docs (uowner, name, description) VALUES (?, ?, ?) ON CONFLICT (name) DO NOTHING",
		userID(r), d.Name, d.Description)
	if err != nil {
		internalError(w, "insert doc", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusConflict, "a doc with that name already exists")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "doc created"})
}

// handleRawUpload accepts an archive as the raw request body.
func (s *Server) handleRawUpload(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes)
	s.storeUpload(w, body)
}

// handleFormUpload accepts an archive in the multipart field "file".
func (s *Server) handleFormUpload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxUploadBytes+1<<20)
	f, _, err := r.FormFile("file")
	if err != nil {
		writeError(w, http.StatusBadRequest, "missing or invalid file field")
		return
	}
	defer f.Close()
	defer r.MultipartForm.RemoveAll()
	s.storeUpload(w, f)
}

// storeUpload spools the archive to disk, extracts it into a staging directory and
// moves it into place only when extraction fully succeeded.
func (s *Server) storeUpload(w http.ResponseWriter, src io.Reader) {
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

	// Stage next to the final location so the rename stays on one filesystem.
	staging, err := os.MkdirTemp(s.sites, ".staging-*")
	if err != nil {
		internalError(w, "create staging dir", err)
		return
	}
	defer os.RemoveAll(staging)

	limits := extractLimits{maxBytes: s.cfg.MaxExtractSize, maxFiles: s.cfg.MaxExtractFile}
	if err := extractArchive(spool, staging, limits); err != nil {
		slog.Info("rejected upload", "err", err)
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	id := xid.New().String()
	if err := os.Rename(staging, filepath.Join(s.sites, id)); err != nil {
		internalError(w, "move upload into place", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "File uploaded", "id": id})
}
