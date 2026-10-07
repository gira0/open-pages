package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
)

// registerSiteRoutes makes the mux serve sites for requests no API route claims.
func (s *Server) registerSiteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /{path...}", s.handleSite)
}

// handleSite serves a file of a site. HEAD requests are routed here too.
func (s *Server) handleSite(w http.ResponseWriter, r *http.Request) {
	site, filePath := s.siteFromRequest(r)
	if site == "" {
		http.NotFound(w, r)
		return
	}
	s.serveSite(w, r, site, filePath)
}

// serveSite serves filePath from the live version of site. Lookup and (later) access
// control happen here, whatever the URL mode.
func (s *Server) serveSite(w http.ResponseWriter, r *http.Request, site, filePath string) {
	if _, err := s.getSite(r.Context(), site); errors.Is(err, errSiteNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		slog.Error("look up site", "site", site, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// CurrentDir is a symlink; os.Root follows it for the root itself but then refuses
	// any path (including symlinks inside the site) that leads outside it.
	root, err := os.OpenRoot(s.CurrentDir(site))
	if err != nil {
		http.NotFound(w, r) // never deployed
		return
	}
	defer root.Close()
	version, _ := s.CurrentVersion(site)

	name, ok := rootName(filePath)
	if !ok {
		s.serveNotFound(w, r, root)
		return
	}

	f, info, err := openFile(root, name)
	switch {
	case errors.Is(err, errIsDir):
		if !strings.HasSuffix(r.URL.Path, "/") {
			redirectToSlash(w, r)
			return
		}
		f, info, err = openFile(root, path.Join(name, "index.html"))
		if errors.Is(err, errIsDir) {
			err = fs.ErrNotExist // no directory listings
		}
	case err == nil && strings.HasSuffix(r.URL.Path, "/"):
		f.Close()
		err = fs.ErrNotExist // "/file.html/" is not "/file.html"
	}
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, fs.ErrPermission) {
			slog.Debug("open site file", "site", site, "path", filePath, "err", err)
		}
		s.serveNotFound(w, r, root)
		return
	}
	defer f.Close()

	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-cache") // always revalidate: cheap with ETag, and deploys take effect at once
	h.Set("ETag", etag(version, info))
	http.ServeContent(w, r, info.Name(), info.ModTime(), f)
}

// serveNotFound sends the site's own 404.html with status 404, or a plain 404.
func (s *Server) serveNotFound(w http.ResponseWriter, r *http.Request, root *os.Root) {
	f, info, err := openFile(root, "404.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusNotFound)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, f)
	}
}

var errIsDir = errors.New("is a directory")

// openFile opens a regular file in root. It returns errIsDir for a directory.
func openFile(root *os.Root, name string) (*os.File, fs.FileInfo, error) {
	f, err := root.Open(name)
	if err != nil {
		return nil, nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if info.IsDir() {
		f.Close()
		return nil, nil, errIsDir
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, nil, fs.ErrNotExist
	}
	return f, info, nil
}

// rootName turns a URL file path into a name for os.Root: cleaned, relative, "." for the
// root itself. Anything with a NUL byte is refused.
func rootName(filePath string) (string, bool) {
	if strings.ContainsRune(filePath, 0) {
		return "", false
	}
	name := strings.TrimPrefix(path.Clean("/"+filePath), "/")
	if name == "" {
		name = "."
	}
	return name, true
}

func redirectToSlash(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Path + "/"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusMovedPermanently)
}

// etag is weak: it names the deployed version plus the file's size and mtime, so it
// changes on every deploy and rollback.
func etag(version string, info fs.FileInfo) string {
	return fmt.Sprintf(`W/"%s-%x-%x"`, version, info.Size(), info.ModTime().UnixMicro())
}
