package extract

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
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
	return dest, Archive(f, dest, limits)
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
