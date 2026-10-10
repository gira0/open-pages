package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gira0/open-pages/web"
)

// staticFile is one embedded UI asset, with a gzip copy when compressing pays off.
type staticFile struct {
	body  []byte
	gz    []byte // nil when not worth compressing (fonts are already compressed)
	ctype string
	etag  string
}

// staticAssets serves web.Static under /static/<hash>/, where hash covers every file.
// Each build thus gets new URLs whenever an asset changes, so browsers may cache the
// files forever.
type staticAssets struct {
	hash  string
	files map[string]*staticFile // by path relative to web/static
}

// prefix is the URL path the templates put in front of asset paths.
func (a *staticAssets) prefix() string { return "/static/" + a.hash + "/" }

// loadStatic reads and compresses the embedded assets once; they never change at run time.
var loadStatic = sync.OnceValues(func() (*staticAssets, error) {
	sub, err := fs.Sub(web.Static, "static")
	if err != nil {
		return nil, err
	}
	return newStaticAssets(sub)
})

func newStaticAssets(fsys fs.FS) (*staticAssets, error) {
	a := &staticAssets{files: map[string]*staticFile{}}
	var names []string
	err := fs.WalkDir(fsys, ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		names = append(names, name)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk static assets: %w", err)
	}
	slices.Sort(names)

	all := sha256.New()
	for _, name := range names {
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("read static asset %s: %w", name, err)
		}
		sum := sha256.Sum256(body)
		fmt.Fprintf(all, "%s\x00%x\x00", name, sum)
		f := &staticFile{body: body, ctype: assetType(name), etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		if compressible(f.ctype) {
			if f.gz, err = gzipBytes(body); err != nil {
				return nil, fmt.Errorf("compress static asset %s: %w", name, err)
			}
			if len(f.gz) >= len(body) {
				f.gz = nil
			}
		}
		a.files[name] = f
	}
	a.hash = hex.EncodeToString(all.Sum(nil)[:6])
	return a, nil
}

func assetType(name string) string {
	switch path.Ext(name) {
	case ".woff2":
		return "font/woff2" // not in Go's built-in table
	case ".txt", "":
		return "text/plain; charset=utf-8"
	}
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	return "application/octet-stream"
}

func compressible(ctype string) bool {
	return strings.HasPrefix(ctype, "text/") || strings.HasPrefix(ctype, "image/svg") ||
		strings.Contains(ctype, "javascript") || strings.Contains(ctype, "json")
}

func gzipBytes(b []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(b); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// acceptsGzip reports whether the Accept-Encoding header allows gzip. An explicit gzip
// entry wins over "*"; q=0 refuses.
func acceptsGzip(header string) bool {
	gzipQ, starQ := -1.0, -1.0
	for part := range strings.SplitSeq(header, ",") {
		coding, params, _ := strings.Cut(part, ";")
		q := 1.0
		if k, v, ok := strings.Cut(params, "="); ok && strings.EqualFold(strings.TrimSpace(k), "q") {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil || f < 0 || f > 1 {
				continue // not a valid qvalue: ignore the entry
			}
			q = f
		}
		switch strings.ToLower(strings.TrimSpace(coding)) {
		case "gzip", "x-gzip":
			gzipQ = q
		case "*":
			starQ = q
		}
	}
	if gzipQ >= 0 {
		return gzipQ > 0
	}
	return starQ > 0
}

// handleStatic serves an embedded asset. Only the current hash is served: an old URL
// would otherwise be cached forever with new content.
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	f, ok := s.static.files[r.PathValue("path")]
	if !ok || r.PathValue("hash") != s.static.hash {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("X-Content-Type-Options", "nosniff")
	body, etag := f.body, f.etag
	if f.gz != nil {
		h.Add("Vary", "Accept-Encoding")
		if acceptsGzip(strings.Join(r.Header.Values("Accept-Encoding"), ",")) {
			h.Set("Content-Encoding", "gzip")
			body, etag = f.gz, strings.TrimSuffix(f.etag, `"`)+`-gz"`
		}
	}
	h.Set("ETag", etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(body))
}
