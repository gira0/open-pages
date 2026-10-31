//go:build unix

package extract

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// TestArchiveModesGroupWritable checks that extracted content is group-writable under the
// server's umask 002, so another UID in group 0 can replace or prune it. The umask is
// process-wide, so the check runs in a child process of the test binary.
func TestArchiveModesGroupWritable(t *testing.T) {
	if os.Getenv("OPEN_PAGES_TEST_UMASK_CHILD") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestArchiveModesGroupWritable$", "-test.count=1") //nolint:gosec // G204: re-runs this test binary
		cmd.Env = append(os.Environ(), "OPEN_PAGES_TEST_UMASK_CHILD=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("child: %v\n%s", err, out)
		}
		return
	}
	syscall.Umask(0o002)
	for name, data := range map[string][]byte{
		"zip": zipBytes(t, entry{name: "explicit/"}, entry{name: "implicit/sub/index.html", body: "x"}),
		"tar": tarBytes(t, entry{name: "explicit", typ: '5'}, entry{name: "implicit/sub/index.html", body: "x"}),
	} {
		dest, err := run(t, data, bigLimits)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for rel, want := range map[string]os.FileMode{
			"explicit":                0o775,
			"implicit":                0o775,
			"implicit/sub":            0o775,
			"implicit/sub/index.html": 0o664,
		} {
			fi, err := os.Stat(filepath.Join(dest, rel))
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got := fi.Mode().Perm(); got != want {
				t.Errorf("%s: %s has mode %v, want %v", name, rel, got, want)
			}
		}
	}
}
