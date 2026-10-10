package main

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
)

var assetRefRe = regexp.MustCompile(`(?:href|src)="(/static/[^"]+)"`)

// Every asset the layout links to is served, with the caching and type headers.
func TestIndexAssetsAreServed(t *testing.T) {
	s := newServeTestServer(t, nil)
	page := do(s, "GET", "", "/index")
	expectStatus(t, page.Code, http.StatusOK)
	refs := assetRefRe.FindAllStringSubmatch(page.Body.String(), -1)
	if len(refs) < 4 {
		t.Fatalf("found %d asset links in /index, want the CSS and scripts", len(refs))
	}
	for _, ref := range refs {
		if !strings.HasPrefix(ref[1], s.static.prefix()) {
			t.Errorf("%s: not under %s", ref[1], s.static.prefix())
		}
		w := do(s, "GET", "", ref[1])
		expectStatus(t, w.Code, http.StatusOK)
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("%s: Cache-Control %q", ref[1], cc)
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: missing nosniff", ref[1])
		}
	}
}

// The fonts the PatternFly stylesheet points at were all vendored.
func TestPatternFlyFontsResolve(t *testing.T) {
	s := newServeTestServer(t, nil)
	css := do(s, "GET", "", s.static.prefix()+"patternfly/patternfly.min.css")
	expectStatus(t, css.Code, http.StatusOK)
	if ct := css.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type %q", ct)
	}
	fonts := regexp.MustCompile(`url\((assets/[^)]+\.woff2)\)`).FindAllStringSubmatch(css.Body.String(), -1)
	if len(fonts) == 0 {
		t.Fatal("no fonts referenced; did the stylesheet change?")
	}
	for _, f := range fonts {
		w := do(s, "GET", "", s.static.prefix()+"patternfly/"+f[1])
		expectStatus(t, w.Code, http.StatusOK)
		if ct := w.Header().Get("Content-Type"); ct != "font/woff2" {
			t.Errorf("%s: Content-Type %q", f[1], ct)
		}
		if w.Header().Get("Content-Encoding") != "" {
			t.Errorf("%s: fonts are already compressed", f[1])
		}
	}
}

func TestStaticGzip(t *testing.T) {
	s := newServeTestServer(t, nil)
	url := s.static.prefix() + "patternfly/patternfly.min.css"
	plain := do(s, "GET", "", url)
	gz := do(s, "GET", "", url, "Accept-Encoding", "br, gzip")
	if plain.Header().Get("Content-Encoding") != "" {
		t.Error("gzip sent without Accept-Encoding")
	}
	if gz.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("gzip not sent")
	}
	for _, w := range []http.Header{plain.Header(), gz.Header()} {
		if w.Get("Vary") != "Accept-Encoding" {
			t.Errorf("Vary %q", w.Get("Vary"))
		}
	}
	if plain.Header().Get("ETag") == gz.Header().Get("ETag") {
		t.Error("both encodings share an ETag")
	}
	zr, err := gzip.NewReader(gz.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plain.Body.Bytes()) {
		t.Error("gzip body differs from the plain one")
	}
	if gz.Body.Len() >= plain.Body.Len()/4 {
		t.Errorf("gzip %d bytes of %d: compression not effective", gz.Body.Len(), plain.Body.Len())
	}

	// Accept-Encoding may arrive split over several header lines.
	r := httptest.NewRequest("GET", url, nil)
	r.Header.Add("Accept-Encoding", "br")
	r.Header.Add("Accept-Encoding", "gzip")
	split := httptest.NewRecorder()
	s.routes().ServeHTTP(split, r)
	if split.Header().Get("Content-Encoding") != "gzip" {
		t.Error("gzip not sent for a split Accept-Encoding header")
	}

	notModified := do(s, "GET", "", url, "Accept-Encoding", "gzip", "If-None-Match", gz.Header().Get("ETag"))
	expectStatus(t, notModified.Code, http.StatusNotModified)
}

func TestStaticRejectsUnknown(t *testing.T) {
	s := newServeTestServer(t, nil)
	for _, target := range []string{
		"/static/0000000000/app.js",                // stale or made-up hash
		s.static.prefix() + "missing.js",           // no such file
		s.static.prefix() + "patternfly/../app.js", // cleaned by the mux, then redirected
		s.static.prefix() + "patternfly",           // a directory
		"/static/app.js",                           // no hash at all
	} {
		if w := do(s, "GET", "", target); w.Code == http.StatusOK {
			t.Errorf("%s: served", target)
		}
	}
}

// The strict CSP belongs to the UI only; hosted sites decide their own headers.
func TestCSPOnlyOnUIPages(t *testing.T) {
	s := newServeTestServer(t, nil)
	if csp := do(s, "GET", "", "/index").Header().Get("Content-Security-Policy"); csp != uiCSP {
		t.Errorf("/index CSP %q", csp)
	}
	if fo := do(s, "GET", "", "/index").Header().Get("X-Frame-Options"); fo != "DENY" {
		t.Errorf("/index X-Frame-Options %q", fo)
	}
	deploySite(t, s, "blog", blogFiles)
	w := do(s, "GET", "", "/blog/")
	expectStatus(t, w.Code, http.StatusOK)
	if csp := w.Header().Get("Content-Security-Policy"); csp != "" {
		t.Errorf("hosted site got CSP %q", csp)
	}
}

// Under that CSP the pages must not need inline scripts, inline styles or handlers.
func TestPagesHaveNoInlineCode(t *testing.T) {
	s := newServeTestServer(t, nil)
	body := do(s, "GET", "", "/index").Body.String()
	for _, bad := range []*regexp.Regexp{
		regexp.MustCompile(`<script(?:\s[^>]*)?>[^<]*\S[^<]*</script>`), // inline script body
		regexp.MustCompile(`\sstyle=`),
		regexp.MustCompile(`<style`),
		regexp.MustCompile(`\son[a-z]+=`),
	} {
		if m := bad.FindString(body); m != "" {
			t.Errorf("inline code in /index: %q", m)
		}
	}
	for _, tag := range regexp.MustCompile(`<script[^>]*>`).FindAllString(body, -1) {
		if !strings.Contains(tag, " src=") {
			t.Errorf("script without src in /index: %q", tag)
		}
	}
}

func TestAcceptsGzip(t *testing.T) {
	for header, want := range map[string]bool{
		"":                    false,
		"gzip":                true,
		"GZIP":                true,
		"deflate, gzip;q=0.5": true,
		"gzip;q=0":            false,
		"br":                  false,
		"*":                   true,
		"*;q=0":               false,
		"gzip;q=0, *":         false,
		"*, gzip;q=0":         false,
		"*;q=0, gzip":         true,
		"gzip;q=bad":          false,
		"gzip;Q=0":            false,
		"gzip; q=0.0":         false,
		"gzip;q=2":            false,
		"gzip;q=-1, *":        true,
	} {
		if got := acceptsGzip(header); got != want {
			t.Errorf("acceptsGzip(%q) = %v, want %v", header, got, want)
		}
	}
}

// The hash changes with any file, so a changed asset never reuses a cached URL.
func TestStaticHashCoversContent(t *testing.T) {
	a, err := newStaticAssets(fstest.MapFS{"a.css": {Data: []byte("a{}")}, "b.js": {Data: []byte("1")}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := newStaticAssets(fstest.MapFS{"a.css": {Data: []byte("a{}")}, "b.js": {Data: []byte("2")}})
	if err != nil {
		t.Fatal(err)
	}
	if a.hash == b.hash {
		t.Error("hash ignores file content")
	}
	if a.files["a.css"].gz != nil {
		t.Error("tiny file kept a gzip copy larger than itself")
	}
}
