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
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var bigLimits = Limits{MaxBytes: 1 << 20, MaxFiles: 100}

type entry struct {
	name, body string
	typ        byte // tar type flag; 0 means regular file, '5' a directory, '2' a symlink
}

func zipBytes(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		w, err := zw.Create(e.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarBytes(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		typ := e.typ
		if typ == 0 {
			typ = tar.TypeReg
		}
		hdr := &tar.Header{Name: e.name, Typeflag: typ, Mode: 0o644, Size: int64(len(e.body))}
		if typ == tar.TypeDir {
			hdr.Mode, hdr.Size = 0o755, 0
		}
		if typ == tar.TypeSymlink {
			hdr.Linkname, hdr.Size = "/etc/passwd", 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Size > 0 {
			if _, err := tw.Write([]byte(e.body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gz(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func run(t *testing.T, data []byte, limits Limits) (string, error) {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "up-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	return dest, Archive(t.Context(), f, dest, limits)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestArchiveFormats(t *testing.T) {
	entries := []entry{
		{name: "index.html", body: "home"},
		{name: "assets/", typ: tar.TypeDir},
		{name: "assets/app.js", body: "js"},
	}
	tarData := tarBytes(t, entries...)
	zipEntries := []entry{{name: "index.html", body: "home"}, {name: "assets/app.js", body: "js"}}
	for name, data := range map[string][]byte{
		"zip":    zipBytes(t, zipEntries...),
		"tar":    tarData,
		"tar.gz": gz(t, tarData),
	} {
		t.Run(name, func(t *testing.T) {
			dest, err := run(t, data, bigLimits)
			if err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, filepath.Join(dest, "index.html")); got != "home" {
				t.Errorf("index.html = %q", got)
			}
			if got := readFile(t, filepath.Join(dest, "assets", "app.js")); got != "js" {
				t.Errorf("app.js = %q", got)
			}
		})
	}
}

func TestArchiveUnsupported(t *testing.T) {
	for name, data := range map[string][]byte{
		"empty": nil,
		"text":  []byte("just some text, not an archive"),
	} {
		if _, err := run(t, data, bigLimits); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", name, err)
		}
	}
}

func TestArchiveCorrupt(t *testing.T) {
	if _, err := run(t, []byte("PK\x03\x04 truncated"), bigLimits); err == nil {
		t.Error("corrupt zip extracted")
	}
	if _, err := run(t, []byte{0x1f, 0x8b, 0, 0}, bigLimits); err == nil {
		t.Error("corrupt gzip extracted")
	}
	tarData := tarBytes(t, entry{name: "a", body: "hello"})
	if _, err := run(t, tarData[:514], bigLimits); err == nil {
		t.Error("truncated tar extracted")
	}
}

func TestArchiveRefusesEscapes(t *testing.T) {
	for _, name := range []string{"../evil", "a/../../evil", "/abs/evil", "..", ""} {
		for kind, data := range map[string][]byte{
			"zip": zipBytes(t, entry{name: name, body: "x"}),
			"tar": tarBytes(t, entry{name: name, body: "x"}),
		} {
			dest, err := run(t, data, bigLimits)
			if err == nil {
				t.Errorf("%s %q: extracted", kind, name)
			}
			if _, statErr := os.Stat(filepath.Join(filepath.Dir(dest), "evil")); statErr == nil {
				t.Errorf("%s %q: wrote outside dest", kind, name)
			}
		}
	}
}

func TestArchiveSkipsSymlinks(t *testing.T) {
	data := tarBytes(t, entry{name: "link", typ: tar.TypeSymlink}, entry{name: "ok.txt", body: "ok"})
	dest, err := run(t, data, bigLimits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dest, "link")); err == nil {
		t.Error("symlink was created")
	}
	if got := readFile(t, filepath.Join(dest, "ok.txt")); got != "ok" {
		t.Errorf("ok.txt = %q", got)
	}
}

func TestArchiveCountsSkippedEntries(t *testing.T) {
	data := tarBytes(t,
		entry{name: "l1", typ: tar.TypeSymlink},
		entry{name: "l2", typ: tar.TypeSymlink},
		entry{name: "l3", typ: tar.TypeSymlink},
		entry{name: "ok.txt", body: "ok"},
	)
	if _, err := run(t, data, Limits{MaxBytes: 10, MaxFiles: 4}); err != nil {
		t.Fatalf("at the limit: %v", err)
	}
	if _, err := run(t, data, Limits{MaxBytes: 10, MaxFiles: 3}); err == nil {
		t.Error("symlinks not counted toward the file limit")
	}
}

func TestArchiveLimits(t *testing.T) {
	zipData := zipBytes(t, entry{name: "a", body: "12345"}, entry{name: "b", body: "67890"})
	tarData := tarBytes(t, entry{name: "a", body: "12345"}, entry{name: "b", body: "67890"})
	for kind, data := range map[string][]byte{"zip": zipData, "tar": tarData, "tar.gz": gz(t, tarData)} {
		if _, err := run(t, data, Limits{MaxBytes: 10, MaxFiles: 2}); err != nil {
			t.Errorf("%s at the limits: %v", kind, err)
		}
		if _, err := run(t, data, Limits{MaxBytes: 9, MaxFiles: 2}); err == nil {
			t.Errorf("%s: byte limit not enforced", kind)
		}
		if _, err := run(t, data, Limits{MaxBytes: 10, MaxFiles: 1}); err == nil {
			t.Errorf("%s: file limit not enforced", kind)
		}
	}
}

// An entry of a skipped type still has its data decompressed by tar.Reader.Next, so the
// budget has to apply to the stream, not to what is written.
func TestSkippedEntryDataCountsAgainstBudget(t *testing.T) {
	big := entry{name: "bomb", typ: 'Z', body: strings.Repeat("\x00", 8<<20)}
	tarData := tarBytes(t, big)
	limits := Limits{MaxBytes: 1 << 20, MaxFiles: 10}
	for kind, data := range map[string][]byte{"tar": tarData, "tar.gz": gz(t, tarData)} {
		dest, err := run(t, data, limits)
		if err == nil || !strings.Contains(err.Error(), "expands to more than") {
			t.Errorf("%s: err = %v, want expansion error", kind, err)
		}
		if entries, _ := os.ReadDir(dest); len(entries) != 0 {
			t.Errorf("%s: wrote %v", kind, entries)
		}
	}
}

// An archive that holds exactly MaxBytes of files, with headers and padding, still fits.
func TestStreamAllowanceCoversHeaders(t *testing.T) {
	var es []entry
	for i := range 20 {
		es = append(es, entry{name: fmt.Sprintf("d/f%02d", i), body: "x"})
	}
	tarData := tarBytes(t, es...)
	for kind, data := range map[string][]byte{"tar": tarData, "tar.gz": gz(t, tarData)} {
		if _, err := run(t, data, Limits{MaxBytes: 20, MaxFiles: 20}); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
	}
}

func TestZipEntryCountCheckedBeforeParsing(t *testing.T) {
	var es []entry
	for i := range 200 {
		es = append(es, entry{name: fmt.Sprintf("f%03d", i)})
	}
	data := zipBytes(t, es...)
	err := checkZipDirectory(bytes.NewReader(data), int64(len(data)), Limits{MaxBytes: 1 << 20, MaxFiles: 100})
	if err == nil || !strings.Contains(err.Error(), "more than 100 entries") {
		t.Fatalf("err = %v, want entry count error", err)
	}
	if err := checkZipDirectory(bytes.NewReader(data), int64(len(data)), Limits{MaxBytes: 1 << 20, MaxFiles: 200}); err != nil {
		t.Fatal(err)
	}
}

// A zip64 archive keeps its real count in the zip64 record, not in the end record.
func TestZip64EntryCount(t *testing.T) {
	le := binary.LittleEndian
	rec := make([]byte, 56)
	copy(rec, "PK\x06\x06")
	le.PutUint64(rec[32:], 1_000_000)
	le.PutUint64(rec[40:], 1_000_000*46)
	loc := make([]byte, 20)
	copy(loc, "PK\x06\x07")
	le.PutUint64(loc[8:], 0)
	eocd := make([]byte, 22)
	copy(eocd, "PK\x05\x06")
	le.PutUint16(eocd[10:], 0xffff)
	le.PutUint32(eocd[12:], 0xffffffff)
	data := slices.Concat(rec, loc, eocd)
	err := checkZipDirectory(bytes.NewReader(data), int64(len(data)), Limits{MaxBytes: 1 << 20, MaxFiles: 100})
	if err == nil || !strings.Contains(err.Error(), "more than 100 entries") {
		t.Fatalf("err = %v, want entry count error", err)
	}
}

// archive/zip also takes the zip64 record when only the directory offset is saturated.
func TestZip64SaturatedOffsetOnly(t *testing.T) {
	le := binary.LittleEndian
	rec := make([]byte, 56)
	copy(rec, "PK\x06\x06")
	le.PutUint64(rec[32:], 1_000_000)
	loc := make([]byte, 20)
	copy(loc, "PK\x06\x07")
	eocd := make([]byte, 22)
	copy(eocd, "PK\x05\x06")
	le.PutUint16(eocd[10:], 1)
	le.PutUint32(eocd[16:], 0xffffffff)
	data := slices.Concat(rec, loc, eocd)
	err := checkZipDirectory(bytes.NewReader(data), int64(len(data)), Limits{MaxBytes: 1 << 20, MaxFiles: 100})
	if err == nil || !strings.Contains(err.Error(), "more than 100 entries") {
		t.Fatalf("err = %v, want entry count error", err)
	}
}

// A record forged inside the archive comment must be read the way archive/zip reads it,
// or the pre-check could approve a count that zip.NewReader never sees.
func TestZipForgedRecordMatchesReader(t *testing.T) {
	var es []entry
	for i := range 50 {
		es = append(es, entry{name: fmt.Sprintf("f%02d", i)})
	}
	data := zipBytes(t, es...)
	forged := make([]byte, 22)
	copy(forged, "PK\x05\x06")
	eocdAt := bytes.LastIndex(data, []byte("PK\x05\x06"))
	binary.LittleEndian.PutUint16(data[eocdAt+20:], uint16(len(forged)))
	data = append(data, forged...)
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	seen := 0
	if err == nil {
		seen = len(zr.File)
	}
	// Whatever archive/zip makes of it, the check must not allow more than it will read.
	limits := Limits{MaxBytes: 1 << 20, MaxFiles: 10}
	if err := checkZipDirectory(bytes.NewReader(data), int64(len(data)), limits); err == nil && seen > limits.MaxFiles {
		t.Fatalf("check passed but reader sees %d entries", seen)
	}
}

func TestZipDeclaredSizeRejectedUpFront(t *testing.T) {
	half := strings.Repeat("a", 600<<10)
	data := zipBytes(t, entry{name: "a", body: half}, entry{name: "b", body: half})
	dest, err := run(t, data, Limits{MaxBytes: 1 << 20, MaxFiles: 10})
	if err == nil || !strings.Contains(err.Error(), "expands to more than") {
		t.Fatalf("err = %v, want expansion error", err)
	}
	if entries, _ := os.ReadDir(dest); len(entries) != 0 {
		t.Errorf("wrote %v before rejecting", entries)
	}
}

func TestCanceledContextStopsExtraction(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for kind, data := range map[string][]byte{
		"zip":    zipBytes(t, entry{name: "a", body: "a"}),
		"tar":    tarBytes(t, entry{name: "a", body: "a"}),
		"tar.gz": gz(t, tarBytes(t, entry{name: "a", body: "a"})),
	} {
		f, err := os.CreateTemp(t.TempDir(), "up-*")
		if err != nil {
			t.Fatal(err)
		}
		f.Write(data)
		err = Archive(ctx, f, t.TempDir(), bigLimits)
		f.Close()
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: err = %v, want context.Canceled", kind, err)
		}
	}
}

// The counts in the end record are attacker-controlled; the real directory decides.
func TestZipForgedCountStillCaught(t *testing.T) {
	var es []entry
	for i := range 200 {
		es = append(es, entry{name: fmt.Sprintf("f%03d", i)})
	}
	data := zipBytes(t, es...)
	limits := Limits{MaxBytes: 1 << 20, MaxFiles: 100}
	forge := func(b []byte) []byte {
		at := bytes.LastIndex(b, []byte("PK\x05\x06"))
		binary.LittleEndian.PutUint16(b[at+8:], 1)
		binary.LittleEndian.PutUint16(b[at+10:], 1)
		return b
	}
	plain := forge(slices.Clone(data))
	prepended := forge(slices.Concat([]byte(strings.Repeat("junk", 100)), data))
	for name, b := range map[string][]byte{"plain": plain, "prepended": prepended} {
		err := checkZipDirectory(bytes.NewReader(b), int64(len(b)), limits)
		if err == nil || !strings.Contains(err.Error(), "more than 100 entries") {
			t.Errorf("%s: err = %v, want entry count error", name, err)
		}
	}
}

func TestZipDeclaredSizeSumCannotWrap(t *testing.T) {
	data := zipBytes(t, entry{name: "a", body: "a"}, entry{name: "b", body: "b"})
	// Declare a near-maximum size in the second central directory header.
	second := bytes.LastIndex(data, []byte("PK\x01\x02"))
	binary.LittleEndian.PutUint32(data[second+24:], 0xffffffff)
	if _, err := run(t, data, Limits{MaxBytes: 1 << 20, MaxFiles: 10}); err == nil {
		t.Fatal("oversized declared entry accepted")
	}
}
