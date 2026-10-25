package server

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gira0/open-pages/internal/config"
)

// send issues an unsafe request with the given headers and returns the status.
func send(t *testing.T, c *http.Client, method, url, body string, headers map[string]string) int {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestCrossOriginRequestsRejected(t *testing.T) {
	ts, s := newTestServer(t)
	s.cfg.CORSOrigins = []string{"https://tools.example"}
	// Rebuild the handler so the trusted origin takes effect.
	ts.Config.Handler = s.Handler()
	c := loggedInClient(t, ts)
	url := ts.URL + "/v1/auth/sites"
	with := func(extra map[string]string) map[string]string {
		h := map[string]string{"Content-Type": "application/json"}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}

	cases := []struct {
		name    string
		headers map[string]string
		want    int
	}{
		{"cross-site fetch", with(map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}), http.StatusForbidden},
		{"same-site sibling host", with(map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://evil.corp.example"}), http.StatusForbidden},
		{"Origin differs from Host, no fetch metadata", with(map[string]string{"Origin": "https://evil.example"}), http.StatusForbidden},
		{"simple request with text/plain", map[string]string{"Sec-Fetch-Site": "cross-site", "Content-Type": "text/plain"}, http.StatusForbidden},
		{"trusted cors origin", with(map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://tools.example"}), http.StatusCreated},
		{"same-origin", with(map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": ts.URL}), http.StatusCreated},
		{"no fetch metadata (curl)", with(nil), http.StatusCreated},
	}
	for i, tc := range cases {
		body := `{"name":"site-` + string(rune('a'+i)) + `"}`
		if got := send(t, c, http.MethodPost, url, body, tc.headers); got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
	// A rejected request must not have created anything.
	if got := get(t, c, ts.URL+"/v1/auth/sites/site-a"); got != http.StatusNotFound {
		t.Errorf("site from a rejected request exists: GET status %d", got)
	}
}

func TestCrossOriginUploadAndLoginRejected(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts)
	createSiteVia(t, ts, c, "mysite")
	cross := map[string]string{"Sec-Fetch-Site": "same-site", "Content-Type": "multipart/form-data; boundary=x"}
	body := "--x\r\nContent-Disposition: form-data; name=\"file\"; filename=\"a.zip\"\r\n\r\nPK\r\n--x--\r\n"
	if got := send(t, c, http.MethodPost, ts.URL+"/v1/auth/sites/mysite/formupload", body, cross); got != http.StatusForbidden {
		t.Errorf("cross-site formupload: status %d, want 403", got)
	}
	form := map[string]string{"Sec-Fetch-Site": "cross-site", "Content-Type": "application/x-www-form-urlencoded"}
	if got := send(t, newClient(t), http.MethodPost, ts.URL+"/v1/user/login", "email=a%40example.com&password=x", form); got != http.StatusForbidden {
		t.Errorf("cross-site login: status %d, want 403", got)
	}
}

func TestBearerTokenIgnoresCrossOriginCheckOnlyWithoutBrowserHeaders(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts)
	tok := mintToken(t, ts, c, `{"name":"ci"}`)["token"].(string)
	auth := "Bearer " + tok
	if got := bearerStatus(t, http.MethodPost, ts.URL+"/v1/auth/sites", auth, []byte(`{"name":"ci-site"}`)); got != http.StatusCreated {
		t.Errorf("bearer POST without browser headers: status %d, want 201", got)
	}
}

func TestJSONEndpointsRequireJSONContentType(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts)
	createSiteVia(t, ts, c, "mysite")
	for _, tc := range []struct{ method, path string }{
		{"POST", "/v1/auth/sites"},
		{"PUT", "/v1/auth/sites/mysite"},
		{"POST", "/v1/auth/sites/mysite/rollback"},
		{"POST", "/v1/auth/tokens"},
		{"POST", "/v1/auth/groups"},
		{"POST", "/v1/auth/groups/1/members"},
	} {
		for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", ""} {
			h := map[string]string{}
			if ct != "" {
				h["Content-Type"] = ct
			}
			if got := send(t, c, tc.method, ts.URL+tc.path, `{"name":"x"}`, h); got != http.StatusUnsupportedMediaType {
				t.Errorf("%s %s with Content-Type %q: status %d, want 415", tc.method, tc.path, ct, got)
			}
		}
	}
	// An unauthenticated caller still learns 401, not 415.
	if got := send(t, newClient(t), "POST", ts.URL+"/v1/auth/sites", `{}`, map[string]string{"Content-Type": "text/plain"}); got != http.StatusUnauthorized {
		t.Errorf("anonymous: status %d, want 401", got)
	}
	// application/json with a charset parameter is fine.
	if got := send(t, c, "POST", ts.URL+"/v1/auth/sites", `{"name":"charset"}`, map[string]string{"Content-Type": "application/json; charset=utf-8"}); got != http.StatusCreated {
		t.Errorf("json with charset: status %d, want 201", got)
	}
}

func TestSessionCookieName(t *testing.T) {
	for _, secure := range []bool{false, true} {
		ts, s := newTestServer(t)
		s.cfg.CookieSecure = secure
		want := "auth_cookie"
		if secure {
			want = "__Host-auth_cookie"
		}
		creds := `{"email":"a@example.com","password":"correct horse"}`
		hdr := map[string]string{"Content-Type": "application/json"}
		expectStatus(t, send(t, http.DefaultClient, "POST", ts.URL+"/v1/user/register", creds, hdr), http.StatusCreated)
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/user/login", strings.NewReader(creds))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		var session *http.Cookie
		for _, c := range resp.Cookies() {
			session = c
		}
		if session == nil || session.Name != want {
			t.Fatalf("secure=%v: session cookie %+v, want name %s", secure, session, want)
		}
		if session.Secure != secure || session.Path != "/" || session.Domain != "" || !session.HttpOnly {
			t.Errorf("secure=%v: cookie attributes %+v", secure, session)
		}
		// Only the cookie under the expected name authenticates.
		if secure {
			if got := cookieStatus(t, ts.URL, "auth_cookie", session.Value); got != http.StatusUnauthorized {
				t.Errorf("unprefixed cookie accepted with cookie_secure: status %d", got)
			}
		}
		if got := cookieStatus(t, ts.URL, want, session.Value); got != http.StatusOK {
			t.Errorf("secure=%v: cookie %s: status %d, want 200", secure, want, got)
		}
	}
}

func cookieStatus(t *testing.T, base, name, value string) int {
	t.Helper()
	r, _ := http.NewRequest(http.MethodGet, base+"/v1/auth/user", nil)
	r.AddCookie(&http.Cookie{Name: name, Value: value})
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestOIDCStateCookieIsHostPrefixed(t *testing.T) {
	idp := newFakeIDP(t)
	for _, secure := range []bool{false, true} {
		ts, _ := newOIDCTestServer(t, idp, func(c *config.Config) { c.CookieSecure = secure })
		_, _, h := oidcGet(t, browser(t), ts.URL+"/v1/auth/oidc/login")
		cookies := (&http.Response{Header: h}).Cookies()
		if len(cookies) != 1 {
			t.Fatalf("secure=%v: %d cookies set", secure, len(cookies))
		}
		c := cookies[0]
		wantName, wantPath := "oidc_state", oidcCookiePath
		if secure {
			wantName, wantPath = "__Host-oidc_state", "/"
		}
		if c.Name != wantName || c.Path != wantPath || c.Secure != secure || c.Domain != "" {
			t.Errorf("secure=%v: cookie %+v, want name %s path %s", secure, c, wantName, wantPath)
		}
	}
}

func TestSAMLACSAcceptsCrossSitePost(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, nil)
	// The identity provider's POST is cross-site by nature; the cross-origin check must let
	// it through to the handler's own checks (which reject it here for lack of a cookie).
	req, _ := http.NewRequest(http.MethodPost, ts.ts.URL+"/v1/auth/saml/acs", strings.NewReader("SAMLResponse=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.Header.Set("Origin", "https://idp.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	expectStatus(t, resp.StatusCode, http.StatusBadRequest)
}

func TestSAMLIgnoresUnprefixedCookieWhenSecure(t *testing.T) {
	idp := newFakeSAMLIdP(t)
	ts := newSAMLTestServer(t, idp, func(c *config.Config) { c.CookieSecure = true })
	// A response for a login this browser started, with the cookie value planted under the
	// plain name (as a sibling host could), is not accepted: only __Host-saml_request counts.
	loc := ts.start(t, browser(t), "")
	req, _ := http.NewRequest(http.MethodPost, ts.ts.URL+"/v1/auth/saml/acs",
		strings.NewReader("SAMLResponse="+url.QueryEscape(idp.respond(t, loc))))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: samlRequestCookie, Value: "planted"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	expectStatus(t, resp.StatusCode, http.StatusBadRequest)
}
