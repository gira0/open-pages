package server

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gira0/open-pages/internal/config"
)

func domainsOn(c *config.Config) {
	c.CustomDomains = config.CustomDomains{Enabled: true, AllowedSuffixes: []string{"corp.example", "docs.example"}, PerSite: 2}
}

func newDomainEnv(t *testing.T) *loginEnv {
	t.Helper()
	return newLoginEnvWith(t, true, domainsOn)
}

// domainAPI calls the domain routes as uid.
func (e *loginEnv) domainAPI(method, site, domain string, uid int64) *httptestResult {
	target := "/v1/auth/sites/" + site + "/domains"
	if domain != "" {
		target += "/" + url.PathEscape(domain)
	}
	w := do(e.s, method, testAppHost, target, "Cookie", e.appCookie(uid))
	return &httptestResult{Code: w.Code, Body: w.Body.String()}
}

type httptestResult struct {
	Code int
	Body string
}

func TestDomainValidation(t *testing.T) {
	e := newDomainEnv(t)
	for _, tc := range []struct {
		domain string
		want   int
	}{
		{"handbook.corp.example", http.StatusCreated},
		{"HANDBOOK.corp.example", http.StatusOK}, // same domain, case folded: already attached
		{"handbook.corp.example.", http.StatusOK},
		{"a.b.docs.example", http.StatusCreated},
		{"corp.example", http.StatusBadRequest},              // the suffix itself
		{"evilcorp.example", http.StatusBadRequest},          // not a subdomain
		{"handbook.other.example", http.StatusBadRequest},    // outside the suffixes
		{"xn--e1afmkfd.corp.example", http.StatusBadRequest}, // IDNA label
		{"10.0.0.1", http.StatusBadRequest},                  // IP address
		{"localhost", http.StatusBadRequest},                 // one label
		{"under_score.corp.example", http.StatusBadRequest},  //
		{"host.corp.example:8080", http.StatusBadRequest},    // port
		{testAppHost, http.StatusBadRequest},                 // app host
		{"x." + testAppHost, http.StatusBadRequest},          // under the app host
		{"blog." + testContentDomain, http.StatusBadRequest}, // a site host
		{strings.Repeat("a", 64) + ".corp.example", http.StatusBadRequest},
	} {
		got := e.domainAPI("PUT", "open", tc.domain, e.owner)
		// The third distinct domain would hit the limit of 2 in this env, so use a fresh site.
		if tc.want == http.StatusCreated && got.Code == http.StatusForbidden {
			t.Fatalf("PUT %q hit the limit unexpectedly", tc.domain)
		}
		if got.Code != tc.want {
			t.Errorf("PUT %q = %d, want %d (%s)", tc.domain, got.Code, tc.want, got.Body)
		}
	}
}

func TestDomainOwnerOnlyAndLimit(t *testing.T) {
	e := newDomainEnv(t)
	if r := e.domainAPI("PUT", "open", "a.corp.example", e.outsider); r.Code != http.StatusForbidden {
		t.Errorf("outsider PUT = %d, want 403", r.Code)
	}
	if r := e.domainAPI("PUT", "open", "a.corp.example", e.member); r.Code != http.StatusForbidden {
		t.Errorf("group member PUT = %d, want 403 (owner only)", r.Code)
	}
	if r := e.domainAPI("GET", "secret", "", e.member); r.Code != http.StatusForbidden {
		t.Errorf("member GET = %d, want 403", r.Code)
	}
	if r := e.domainAPI("PUT", "nope", "a.corp.example", e.owner); r.Code != http.StatusNotFound {
		t.Errorf("missing site PUT = %d, want 404", r.Code)
	}
	for _, d := range []string{"a.corp.example", "b.corp.example"} {
		if r := e.domainAPI("PUT", "open", d, e.owner); r.Code != http.StatusCreated {
			t.Fatalf("PUT %s = %d (%s)", d, r.Code, r.Body)
		}
	}
	if r := e.domainAPI("PUT", "open", "c.corp.example", e.owner); r.Code != http.StatusForbidden {
		t.Errorf("third domain = %d, want 403 (limit 2)", r.Code)
	}
	if n := countRows(t, e.s, "site_domain"); n != 2 {
		t.Errorf("rows = %d, want 2 (a refused add must not leave a row)", n)
	}
	r := e.domainAPI("GET", "open", "", e.owner)
	var body struct {
		Domains  []string `json:"domains"`
		Suffixes []string `json:"allowed_suffixes"`
		Limit    int      `json:"limit"`
	}
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil || len(body.Domains) != 2 || body.Limit != 2 || len(body.Suffixes) != 2 {
		t.Errorf("GET = %d %s", r.Code, r.Body)
	}
	if r := e.domainAPI("DELETE", "open", "a.corp.example", e.outsider); r.Code != http.StatusForbidden {
		t.Errorf("outsider DELETE = %d, want 403", r.Code)
	}
	if r := e.domainAPI("DELETE", "open", "a.corp.example", e.owner); r.Code != http.StatusOK {
		t.Errorf("DELETE = %d, want 200", r.Code)
	}
	if r := e.domainAPI("DELETE", "open", "a.corp.example", e.owner); r.Code != http.StatusNotFound {
		t.Errorf("second DELETE = %d, want 404", r.Code)
	}
	if r := e.domainAPI("PUT", "open", "c.corp.example", e.owner); r.Code != http.StatusCreated {
		t.Errorf("after removal a slot is free: %d", r.Code)
	}
}

func TestDomainCollision(t *testing.T) {
	e := newDomainEnv(t)
	if _, err := e.s.createSite(t.Context(), "other", "", e.outsider, 0, visPublic); err != nil {
		t.Fatal(err)
	}
	if r := e.domainAPI("PUT", "open", "wiki.corp.example", e.owner); r.Code != http.StatusCreated {
		t.Fatalf("PUT = %d", r.Code)
	}
	if r := e.domainAPI("PUT", "other", "wiki.corp.example", e.outsider); r.Code != http.StatusConflict {
		t.Errorf("another site claiming the domain = %d, want 409", r.Code)
	}
	if r := e.domainAPI("PUT", "open", "wiki.corp.example", e.owner); r.Code != http.StatusOK {
		t.Errorf("repeating the add = %d, want 200", r.Code)
	}
}

func TestDomainsOffByDefault(t *testing.T) {
	e := newLoginEnv(t, true)
	if r := e.domainAPI("PUT", "open", "a.corp.example", e.owner); r.Code != http.StatusNotFound {
		t.Errorf("PUT with the feature off = %d, want 404", r.Code)
	}
	// Even a row in the table (left over from an earlier configuration) is not served.
	if _, err := e.s.db.Exec("INSERT INTO site_domain (siteid, domain, created) SELECT docid, 'old.corp.example', 0 FROM docs WHERE name = 'open'"); err != nil {
		t.Fatal(err)
	}
	if w := do(e.s, "GET", "old.corp.example", "/"); w.Code != http.StatusNotFound {
		t.Errorf("custom host with the feature off = %d, want 404", w.Code)
	}
}

func TestServeCustomDomain(t *testing.T) {
	e := newDomainEnv(t)
	for site, d := range map[string]string{"open": "open.corp.example", "secret": "secret.corp.example"} {
		if r := e.domainAPI("PUT", site, d, e.owner); r.Code != http.StatusCreated {
			t.Fatalf("PUT %s = %d", d, r.Code)
		}
	}
	w := do(e.s, "GET", "open.corp.example", "/")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<h1>open</h1>") {
		t.Fatalf("custom domain GET = %d %q", w.Code, w.Body)
	}
	if w := do(e.s, "GET", "open.corp.example:8443", "/page.html"); w.Code != http.StatusOK {
		t.Errorf("port in Host header: %d, want 200", w.Code)
	}
	if w := do(e.s, "POST", "open.corp.example", "/"); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST = %d, want 405", w.Code)
	}
	// Nothing of the app is reachable there.
	for _, p := range []string{"/manage", "/v1/auth/user", "/v1/sites", "/index"} {
		if w := do(e.s, "GET", "open.corp.example", p); w.Code != http.StatusNotFound {
			t.Errorf("GET %s on a custom domain = %d, want 404", p, w.Code)
		}
	}
	// X-Forwarded-Host never selects a site, and an unmapped host is not served.
	if w := do(e.s, "GET", "unmapped.corp.example", "/", "X-Forwarded-Host", "open.corp.example"); w.Code != http.StatusNotFound {
		t.Errorf("unmapped host = %d, want 404", w.Code)
	}
	// Removing the domain stops serving it at once; deleting the site removes its domains.
	e.domainAPI("DELETE", "open", "open.corp.example", e.owner)
	if w := do(e.s, "GET", "open.corp.example", "/"); w.Code != http.StatusNotFound {
		t.Errorf("after removal = %d, want 404", w.Code)
	}
	if err := e.s.deleteSite(t.Context(), mustSite(t, e.s, "secret")); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, e.s, "site_domain"); n != 0 {
		t.Errorf("rows after deleting the site = %d, want 0", n)
	}
}

// TestCustomDomainLogin runs the browser login of a restricted site on a custom domain: the
// code goes back to that domain, the cookie works there only, and a domain the site does
// not have is refused.
func TestCustomDomainLogin(t *testing.T) {
	e := newDomainEnv(t)
	const host = "secret.corp.example"
	if r := e.domainAPI("PUT", "secret", host, e.owner); r.Code != http.StatusCreated {
		t.Fatalf("PUT = %d", r.Code)
	}
	w := do(e.s, "GET", host, "/page.html?x=1")
	toApp := location(t, w)
	if toApp.Host != testAppHost || toApp.Query().Get("host") != host || toApp.Query().Get("site") != "secret" {
		t.Fatalf("login redirect = %s", toApp)
	}
	st := cookieNamed(w, e.s.cookieName(siteStateCookie))
	if st == nil {
		t.Fatal("no state cookie")
	}
	// Signed out: the app sends the browser through the UI and keeps the host.
	w = do(e.s, "GET", testAppHost, toApp.RequestURI())
	next, _ := url.QueryUnescape(location(t, w).Query().Get("next"))
	if !strings.Contains(next, "host="+url.QueryEscape(host)) {
		t.Errorf("sign-in detour lost the host: %q", next)
	}
	w = do(e.s, "GET", testAppHost, toApp.RequestURI(), "Cookie", e.appCookie(e.member))
	back := location(t, w)
	if back.Host != host || back.Scheme != "https" || back.Path != siteRedeemPath {
		t.Fatalf("code goes to %s, want https://%s%s", back, host, siteRedeemPath)
	}
	w = do(e.s, "GET", host, back.RequestURI(), "Cookie", st.Name+"="+st.Value)
	if loc := location(t, w); loc.Path != "/page.html" || loc.RawQuery != "x=1" {
		t.Errorf("return = %s", loc)
	}
	c := cookieNamed(w, e.s.cookieName(siteSessionCookie))
	if c == nil {
		t.Fatal("no site cookie")
	}
	cookie := c.Name + "=" + c.Value
	if w := do(e.s, "GET", host, "/", "Cookie", cookie); w.Code != http.StatusOK {
		t.Errorf("signed-in GET = %d, want 200", w.Code)
	}
	// The cookie is host-only: the content host of the same site does not see it, and a
	// browser would not send it there anyway. Access still follows the site's rules:
	// an outsider gets nothing.
	if w := do(e.s, "GET", host, "/", "Cookie", e.appCookie(e.member)); w.Code != http.StatusFound {
		t.Errorf("app session cookie on a custom domain = %d, want a login redirect", w.Code)
	}
	w = do(e.s, "GET", testAppHost, toApp.RequestURI(), "Cookie", e.appCookie(e.outsider))
	if w.Code != http.StatusNotFound {
		t.Errorf("outsider login = %d, want 404", w.Code)
	}
}

func TestSiteLoginRejectsForeignHost(t *testing.T) {
	e := newDomainEnv(t)
	if r := e.domainAPI("PUT", "open", "open.corp.example", e.owner); r.Code != http.StatusCreated {
		t.Fatal(r.Code)
	}
	state := strings.Repeat("a", siteLoginStateSize)
	for _, host := range []string{"open.corp.example", "evil.example", "other.corp.example", testAppHost, "x.corp.example/..", "[::1]"} {
		// "open.corp.example" belongs to "open" but the request names "secret".
		q := url.Values{"site": {"secret"}, "return": {"/"}, "state": {state}, "host": {host}}
		w := do(e.s, "GET", testAppHost, "/v1/auth/site-login?"+q.Encode(), "Cookie", e.appCookie(e.owner))
		if w.Code != http.StatusNotFound {
			t.Errorf("host %q for a site that does not have it = %d, want 404 (Location %q)", host, w.Code, w.Header().Get("Location"))
		}
	}
	// With the feature off the parameter is refused even for a host in the table.
	e2 := newLoginEnv(t, true)
	q := url.Values{"site": {"secret"}, "return": {"/"}, "state": {state}, "host": {"x.corp.example"}}
	if w := do(e2.s, "GET", testAppHost, "/v1/auth/site-login?"+q.Encode(), "Cookie", e2.appCookie(e2.owner)); w.Code != http.StatusNotFound {
		t.Errorf("host parameter with the feature off = %d, want 404", w.Code)
	}
}
