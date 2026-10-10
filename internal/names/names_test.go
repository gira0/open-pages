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
	for _, n := range []string{"v1", "api", "www", "docs", "metrics"} {
		if !ReservedSite(n) {
			t.Errorf("ReservedSite(%q) = false, want true", n)
		}
	}
	if ReservedSite("my-site") {
		t.Error("ReservedSite(my-site) = true, want false")
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
