// Package extract unpacks uploaded site archives (.zip, .tar.gz, .tar) into a directory,
// refusing entries that escape it and archives that expand past the given limits.
package extract

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
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

// ErrUnsupported is returned when the file is not a recognised archive format.
var ErrUnsupported = errors.New("unsupported archive: upload a .zip, .tar.gz or .tar file")

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

// Limits bounds what an archive may expand to.
type Limits struct {
	MaxBytes int64 // total uncompressed bytes, of skipped entries too
	MaxFiles int   // number of entries
}

const (
	// headerAllowance is the room per entry for tar headers and block padding on top of
	// MaxBytes, so that an archive holding exactly MaxBytes of files still fits.
	headerAllowance = 2048
	// streamSlack covers the tar end marker and extended headers.
	streamSlack = 1 << 20
	// maxDirEntryBytes caps the zip central directory at this many bytes per allowed entry.
	maxDirEntryBytes = 1024
)

var errCanceled = errors.New("extraction stopped")

// guardReader enforces the decompression budget and the context on a stream.
type guardReader struct {
	ctx   context.Context
	r     io.Reader
	n     int64
	limit int64
}

func (g *guardReader) Read(p []byte) (int, error) {
	if err := g.ctx.Err(); err != nil {
		return 0, fmt.Errorf("%w: %w", errCanceled, err)
	}
	n, err := g.r.Read(p)
	g.n += int64(n)
	if g.limit > 0 && g.n > g.limit {
		return n, fmt.Errorf("archive expands to more than %d MiB", g.limit>>20)
	}
	return n, err
}

// streamLimit is how many decompressed bytes a tar stream may produce.
func streamLimit(l Limits) int64 {
	return l.MaxBytes + int64(l.MaxFiles)*headerAllowance + streamSlack
}

// extractor writes archive entries below dest, refusing anything that would escape it
// or exceed the limits. Symlinks and other special entries are skipped.
type extractor struct {
	ctx     context.Context
	dest    string
	limits  Limits
	written int64
	files   int
}

// count records one archive entry, skipped ones included, and enforces MaxFiles.
func (e *extractor) count() error {
	if err := e.ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", errCanceled, err)
	}
	e.files++
	if e.files > e.limits.MaxFiles {
		return fmt.Errorf("archive has more than %d entries", e.limits.MaxFiles)
	}
	return nil
}

// target validates an entry name and returns where it belongs on disk.
func (e *extractor) target(name string) (string, error) {
	local := filepath.FromSlash(name)
	if !filepath.IsLocal(local) {
		return "", fmt.Errorf("archive entry %q points outside the site", name)
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
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644) //nolint:gosec // G304: path checked by target()
	if err != nil {
		return err
	}
	remaining := e.limits.MaxBytes - e.written
	n, err := io.CopyN(f, &guardReader{ctx: e.ctx, r: r}, remaining+1)
	e.written += n
	if cerr := f.Close(); err == nil || errors.Is(err, io.EOF) {
		err = cerr
	}
	if err != nil {
		return err
	}
	if e.written > e.limits.MaxBytes {
		return fmt.Errorf("archive expands to more than %d MiB", e.limits.MaxBytes>>20)
	}
	return nil
}

// checkZipDirectory reads the entry count from the end-of-central-directory record, and
// the zip64 record when the count overflows, so that an archive with millions of entries
// is refused before zip.NewReader parses them all into memory.
func checkZipDirectory(src io.ReaderAt, size int64, limits Limits) error {
	const eocdLen, maxComment = 22, 1<<16 - 1
	tailLen := min(size, eocdLen+maxComment)
	tail := make([]byte, tailLen)
	if _, err := src.ReadAt(tail, size-tailLen); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read zip: %w", err)
	}
	// Pick the record the way archive/zip does, the last signature whose comment length fits,
	// so a forged record inside a comment cannot hide the real count.
	i := len(tail) - eocdLen
	for ; i >= 0; i-- {
		if bytes.HasPrefix(tail[i:], []byte("PK\x05\x06")) &&
			i+eocdLen+int(binary.LittleEndian.Uint16(tail[i+20:])) <= len(tail) {
			break
		}
	}
	if i < 0 {
		return errors.New("read zip: no end of central directory")
	}
	eocd := tail[i:]
	entries := uint64(binary.LittleEndian.Uint16(eocd[10:]))
	dirSize := uint64(binary.LittleEndian.Uint32(eocd[12:]))
	dirOffset := uint64(binary.LittleEndian.Uint32(eocd[16:]))
	if entries == 0xffff || dirSize == 0xffffffff || dirOffset == 0xffffffff {
		// zip64: the locator sits right before the end record and points at the real one.
		eocdAt := size - tailLen + int64(i)
		if eocdAt < 20 {
			return errors.New("read zip: bad zip64 locator")
		}
		var loc [20]byte
		if _, err := src.ReadAt(loc[:], eocdAt-20); err != nil || !bytes.HasPrefix(loc[:], []byte("PK\x06\x07")) {
			return errors.New("read zip: bad zip64 locator")
		}
		at := binary.LittleEndian.Uint64(loc[8:])
		var rec [56]byte
		if at > uint64(size) || size-int64(at) < int64(len(rec)) { //nolint:gosec // G115: sizes are non-negative and compared, never used as lengths
			return errors.New("read zip: bad zip64 record")
		}
		if _, err := src.ReadAt(rec[:], int64(at)); err != nil || !bytes.HasPrefix(rec[:], []byte("PK\x06\x06")) { //nolint:gosec // G115: sizes are non-negative and compared, never used as lengths
			return errors.New("read zip: bad zip64 record")
		}
		entries = binary.LittleEndian.Uint64(rec[32:])
		dirSize = binary.LittleEndian.Uint64(rec[40:])
	}
	if entries > uint64(limits.MaxFiles) { //nolint:gosec // G115: sizes are non-negative and compared, never used as lengths
		return fmt.Errorf("archive has more than %d entries", limits.MaxFiles)
	}
	if dirSize > uint64(limits.MaxFiles)*maxDirEntryBytes+maxDirEntryBytes { //nolint:gosec // G115: sizes are non-negative and compared, never used as lengths
		return errors.New("read zip: central directory is too large")
	}
	return nil
}

func extractZip(ctx context.Context, src io.ReaderAt, size int64, dest string, limits Limits) error {
	if err := checkZipDirectory(src, size, limits); err != nil {
		return err
	}
	zr, err := zip.NewReader(src, size)
	if err != nil {
		return fmt.Errorf("read zip: %w", err)
	}
	// Declared sizes can lie, which the byte count in file() still catches; this just
	// turns away the honest oversized archive without writing anything.
	var declared uint64
	for _, f := range zr.File {
		declared += f.UncompressedSize64
		if declared > uint64(limits.MaxBytes) { //nolint:gosec // G115: sizes are non-negative and compared, never used as lengths
			return fmt.Errorf("archive expands to more than %d MiB", limits.MaxBytes>>20)
		}
	}
	e := &extractor{ctx: ctx, dest: dest, limits: limits}
	for _, f := range zr.File {
		if err := e.count(); err != nil {
			return err
		}
		mode := f.Mode()
		switch {
		case mode.IsDir():
			err = e.dir(f.Name)
		case mode.IsRegular():
			var rc io.ReadCloser
			if rc, err = f.Open(); err == nil {
				err = e.file(f.Name, rc)
				_ = rc.Close()
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// extractTar reads the tar stream src. The stream is wrapped in a guardReader because
// tar.Reader.Next reads and discards the data of entries it skips, so only a limit on the
// stream itself bounds that work.
func extractTar(ctx context.Context, src io.Reader, dest string, limits Limits) error {
	tr := tar.NewReader(&guardReader{ctx: ctx, r: src, limit: streamLimit(limits)})
	e := &extractor{ctx: ctx, dest: dest, limits: limits}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		if err := e.count(); err != nil {
			return err
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

// Archive detects the format of src (zip, gzipped tar or tar) and extracts it into dest.
// It returns ErrUnsupported for any other format, and stops with an error once ctx is done.
func Archive(ctx context.Context, src *os.File, dest string, limits Limits) error {
	head := make([]byte, 512)
	n, err := src.ReadAt(head, 0)
	if err != nil && !errors.Is(err, io.EOF) {
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
		return extractZip(ctx, src, st.Size(), dest, limits)
	case kindTarGz:
		gz, err := gzip.NewReader(src)
		if err != nil {
			return fmt.Errorf("read gzip: %w", err)
		}
		defer gz.Close()
		return extractTar(ctx, gz, dest, limits)
	case kindTar:
		return extractTar(ctx, src, dest, limits)
	}
	return ErrUnsupported
}
