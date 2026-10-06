package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

type archiveKind int

const (
	kindUnknown archiveKind = iota
	kindZip
	kindTarGz
	kindTar
)

var errUnsupportedArchive = errors.New("unsupported archive: upload a .zip, .tar.gz or .tar file")

// sniffArchive identifies the archive format from its leading bytes.
func sniffArchive(head []byte) archiveKind {
	switch {
	case bytes.HasPrefix(head, []byte("PK\x03\x04")), bytes.HasPrefix(head, []byte("PK\x05\x06")):
		return kindZip
	case bytes.HasPrefix(head, []byte{0x1f, 0x8b}):
		return kindTarGz
	case len(head) >= 262 && bytes.Equal(head[257:262], []byte("ustar")):
		return kindTar
	}
	return kindUnknown
}

// extractLimits bounds what an archive may expand to.
type extractLimits struct {
	maxBytes int64
	maxFiles int
}

// extractor writes archive entries below dest, refusing anything that would escape it
// or exceed the limits. Symlinks and other special entries are skipped.
type extractor struct {
	dest    string
	limits  extractLimits
	written int64
	files   int
}

// target validates an entry name and returns where it belongs on disk.
func (e *extractor) target(name string) (string, error) {
	local := filepath.FromSlash(name)
	if !filepath.IsLocal(local) {
		return "", fmt.Errorf("archive entry %q points outside the site", name)
	}
	e.files++
	if e.files > e.limits.maxFiles {
		return "", fmt.Errorf("archive has more than %d entries", e.limits.maxFiles)
	}
	return filepath.Join(e.dest, local), nil
}

func (e *extractor) dir(name string) error {
	path, err := e.target(name)
	if err != nil {
		return err
	}
	return os.MkdirAll(path, 0o755)
}

func (e *extractor) file(name string, r io.Reader) error {
	path, err := e.target(name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	remaining := e.limits.maxBytes - e.written
	n, err := io.CopyN(f, r, remaining+1)
	e.written += n
	if cerr := f.Close(); err == nil || err == io.EOF {
		err = cerr
	}
	if err != nil {
		return err
	}
	if e.written > e.limits.maxBytes {
		return fmt.Errorf("archive expands to more than %d MiB", e.limits.maxBytes>>20)
	}
	return nil
}

func extractZip(src io.ReaderAt, size int64, dest string, limits extractLimits) error {
	zr, err := zip.NewReader(src, size)
	if err != nil {
		return fmt.Errorf("read zip: %w", err)
	}
	e := &extractor{dest: dest, limits: limits}
	for _, f := range zr.File {
		mode := f.Mode()
		switch {
		case mode.IsDir():
			err = e.dir(f.Name)
		case mode.IsRegular():
			var rc io.ReadCloser
			if rc, err = f.Open(); err == nil {
				err = e.file(f.Name, rc)
				rc.Close()
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func extractTar(src io.Reader, dest string, limits extractLimits) error {
	tr := tar.NewReader(src)
	e := &extractor{dest: dest, limits: limits}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			err = e.dir(hdr.Name)
		case tar.TypeReg:
			err = e.file(hdr.Name, tr)
		}
		if err != nil {
			return err
		}
	}
}

// extractArchive detects the format of the file at src and extracts it into dest.
func extractArchive(src *os.File, dest string, limits extractLimits) error {
	head := make([]byte, 512)
	n, err := src.ReadAt(head, 0)
	if err != nil && err != io.EOF {
		return err
	}
	st, err := src.Stat()
	if err != nil {
		return err
	}
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}

	switch sniffArchive(head[:n]) {
	case kindZip:
		return extractZip(src, st.Size(), dest, limits)
	case kindTarGz:
		gz, err := gzip.NewReader(src)
		if err != nil {
			return fmt.Errorf("read gzip: %w", err)
		}
		defer gz.Close()
		return extractTar(gz, dest, limits)
	case kindTar:
		return extractTar(src, dest, limits)
	}
	return errUnsupportedArchive
}
