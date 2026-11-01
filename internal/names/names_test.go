package names

import (
	"strings"
	"testing"
)

func TestValidSite(t *testing.T) {
	for _, n := range []string{"a", "docs", "my-site", "a1", strings.Repeat("a", 63)} {
		if !ValidSite(n) {
			t.Errorf("ValidSite(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"", "-a", "a-", "A", "a_b", "a.b", "a/b", strings.Repeat("a", 64)} {
		if ValidSite(n) {
			t.Errorf("ValidSite(%q) = true, want false", n)
		}
	}
}

func TestReservedSite(t *testing.T) {
	for _, n := range []string{"api", "www", "admin", "docs"} {
		if !ReservedSite(n) {
			t.Errorf("ReservedSite(%q) = false, want true", n)
		}
	}
	// Route names only mattered when sites shared the app's host.
	for _, n := range []string{"my-site", "v1", "index", "healthz", "metrics"} {
		if ReservedSite(n) {
			t.Errorf("ReservedSite(%q) = true, want false", n)
		}
	}
}

func TestGroupKey(t *testing.T) {
	if GroupKey("Ärzte") != GroupKey("ärzte") {
		t.Error("case variants should share a key")
	}
	if GroupKey("K") != GroupKey("\u212a") {
		t.Error("Kelvin sign should fold to k")
	}
	if GroupKey("a") == GroupKey("b") {
		t.Error("different names must have different keys")
	}
	if GroupKey("Team") != GroupKey("tEAM") {
		t.Error("mixed-case variants should share a key")
	}
}

func TestPreviewSite(t *testing.T) {
	long := strings.Repeat("a", 70)
	got, ok := PreviewSite("blog", "main")
	if !ok || got != "blog--main" {
		t.Fatalf("PreviewSite(blog, main) = %q, %v", got, ok)
	}
	// Case alone is folded without a hash.
	if got, _ := PreviewSite("blog", "Main"); got != "blog--main" {
		t.Errorf("Main = %q", got)
	}
	a, _ := PreviewSite("blog", "feature/x")
	b, _ := PreviewSite("blog", "feature-x")
	if a == b || !strings.HasPrefix(a, "blog--feature-x-") || b != "blog--feature-x" {
		t.Errorf("feature/x = %q, feature-x = %q", a, b)
	}
	n, ok := PreviewSite("blog", long)
	if !ok || len(n) > 63 || !ValidSite(n) {
		t.Errorf("long branch = %q (%d), %v", n, len(n), ok)
	}
	n2, _ := PreviewSite("blog", long+"b")
	if n == n2 {
		t.Error("long branches sharing a prefix must differ")
	}
	for _, bad := range [][2]string{{"blog", ""}, {"blog", "///"}, {"Bad", "main"}, {strings.Repeat("a", 60), "feature/x"}, {"xn", "foo"}, {"ab", "main"}} {
		if n, ok := PreviewSite(bad[0], bad[1]); ok {
			t.Errorf("PreviewSite(%q, %q) = %q, want failure", bad[0], bad[1], n)
		}
	}
}

func TestReservedLDHAndPreviewBase(t *testing.T) {
	for _, n := range []string{"xn--abc", "ab--c", "ab--"} {
		if !ReservedSite(n) {
			t.Errorf("ReservedSite(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"blog--x", "a--b", "abc--d", "a-b"} {
		if ReservedSite(n) {
			t.Errorf("ReservedSite(%q) = true, want false", n)
		}
	}
	for name, want := range map[string]string{"blog--x": "blog", "a--b--c": "a"} {
		if got, ok := PreviewBase(name); !ok || got != want {
			t.Errorf("PreviewBase(%q) = %q, %v", name, got, ok)
		}
	}
	for _, n := range []string{"blog", "a-b", "--x"} {
		if _, ok := PreviewBase(n); ok {
			t.Errorf("PreviewBase(%q) ok", n)
		}
	}
}
