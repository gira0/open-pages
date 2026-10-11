package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gira0/open-pages/internal/config"
)

// loginEnv is a server with three users (owner, member of group eng, outsider), a restricted
// site "secret" shared with eng, an authenticated site "intra" and a public site "open".
type loginEnv struct {
	s                       *Server
	owner, member, outsider int64
	group                   int64
	sess                    map[int64]string // user id -> app session token
	secure                  bool
}

func newLoginEnv(t *testing.T, secure bool) *loginEnv {
	t.Helper()
	s := newServeTestServer(t, func(c *config.Config) { c.CookieSecure = secure })
	e := &loginEnv{s: s, sess: map[int64]string{}, secure: secure}
	ctx := t.Context()
	add := func(email string) int64 {
		res, err := s.db.ExecContext(ctx, "INSERT INTO user (email, password) VALUES (?, 'x')", email)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		tok := "app-session-" + email
		if _, err := s.db.ExecContext(ctx, "INSERT INTO session (userid, token, expires) VALUES (?, ?, ?)",
			id, tok, time.Now().Add(sessionLifetime).Unix()); err != nil {
			t.Fatal(err)
		}
		e.sess[id] = tok
		return id
	}
	e.owner, e.member, e.outsider = add("owner@example.com"), add("member@example.com"), add("outsider@example.com")
	g, err := s.createGroup(ctx, "eng", e.owner)
	if err != nil {
		t.Fatal(err)
	}
	e.group = g.ID
	if _, err := s.db.ExecContext(ctx, "INSERT INTO user_group (uid, gid) VALUES (?, ?)", e.member, g.ID); err != nil {
		t.Fatal(err)
	}
	for _, site := range []struct {
		name, vis string
		group     int64
	}{{"secret", visRestricted, g.ID}, {"intra", visAuthenticated, 0}, {"open", visPublic, 0}} {
		if _, err := s.createSite(ctx, site.name, "", e.owner, site.group, site.vis); err != nil {
			t.Fatal(err)
		}
		files := map[string]string{"index.html": "<h1>" + site.name + "</h1>", "page.html": "page", "__open-pages/auth": "shadow"}
		if _, err := deployNamed(t, s, site.name, writeZip(t, files)); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *loginEnv) appCookie(uid int64) string {
	return e.s.cookieName(sessionCookie) + "=" + e.sess[uid]
}

func (e *loginEnv) scheme() string {
	if e.secure {
		return "https"
	}
	return "http"
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func location(t *testing.T, w *httptest.ResponseRecorder) *url.URL {
	t.Helper()
	if w.Code != http.StatusFound {
		t.Fatalf("status %d, want 302 (body %q)", w.Code, w.Body)
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// handoff runs the first two steps for uid (0: no app session): the visit to the site root and
// the visit to the app host. It returns the code and the state cookie header of the site.
func (e *loginEnv) handoff(t *testing.T, uid int64, site string) (code, stateCookie string) {
	t.Helper()
	w := do(e.s, "GET", siteHost(site), "/")
	toApp := location(t, w)
	st := cookieNamed(w, e.s.cookieName(siteStateCookie))
	if st == nil {
		t.Fatal("no state cookie")
	}
	hdr := []string{}
	if uid != 0 {
		hdr = append(hdr, "Cookie", e.appCookie(uid))
	}
	w = do(e.s, "GET", testAppHost, toApp.RequestURI(), hdr...)
	back := location(t, w)
	return back.Query().Get("code"), st.Name + "=" + st.Value
}

func (e *loginEnv) redeem(site, code, stateCookie string) *httptest.ResponseRecorder {
	hdr := []string{}
	if stateCookie != "" {
		hdr = append(hdr, "Cookie", stateCookie)
	}
	return do(e.s, "GET", siteHost(site), "/__open-pages/auth?code="+url.QueryEscape(code), hdr...)
}

// signIn runs the whole handoff and returns the site cookie header.
func (e *loginEnv) signIn(t *testing.T, uid int64, site string) string {
	t.Helper()
	code, st := e.handoff(t, uid, site)
	w := e.redeem(site, code, st)
	location(t, w)
	c := cookieNamed(w, e.s.cookieName(siteSessionCookie))
	if c == nil {
		t.Fatal("no site cookie")
	}
	return c.Name + "=" + c.Value
}

func countRows(t *testing.T, s *Server, table string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSiteLoginHandoff(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[secure], func(t *testing.T) {
			e := newLoginEnv(t, secure)
			s := e.s

			// 1. A browser without credentials is sent to the app host, with a state cookie.
			w := do(s, "GET", siteHost("secret"), "/page.html?x=1")
			toApp := location(t, w)
			if got := toApp.Scheme + "://" + toApp.Host + toApp.Path; got != e.scheme()+"://"+testAppHost+siteLoginPath {
				t.Errorf("login redirect to %q", got)
			}
			q := toApp.Query()
			if q.Get("site") != "secret" || q.Get("return") != "/page.html?x=1" || !validState(q.Get("state")) {
				t.Errorf("login query = %v", q)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q", w.Header().Get("Cache-Control"))
			}
			st := cookieNamed(w, s.cookieName(siteStateCookie))
			if st == nil || st.Value != q.Get("state") || !st.HttpOnly || st.Secure != secure || st.Domain != "" || st.Path != "/" ||
				st.MaxAge <= 0 || st.MaxAge > int(siteLoginStateTTL.Seconds()) || st.SameSite != http.SameSiteLaxMode {
				t.Fatalf("state cookie = %+v", st)
			}
			if strings.HasPrefix(st.Name, "__Host-") != secure {
				t.Errorf("state cookie name %q", st.Name)
			}

			// 2. The app host issues a code for the signed-in member.
			w = do(s, "GET", testAppHost, toApp.RequestURI(), "Cookie", e.appCookie(e.member))
			toSite := location(t, w)
			if toSite.Scheme != e.scheme() || toSite.Host != siteHost("secret") || toSite.Path != siteRedeemPath {
				t.Fatalf("redirect back to %q", toSite)
			}
			for k, want := range map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "no-referrer"} {
				if got := w.Header().Get(k); got != want {
					t.Errorf("%s = %q, want %q", k, got, want)
				}
			}
			code := toSite.Query().Get("code")
			if len(code) != 64 {
				t.Fatalf("code = %q", code)
			}
			var stored string
			if err := s.db.QueryRowContext(t.Context(), "SELECT hash FROM site_login_code").Scan(&stored); err != nil || stored != hashToken(code) {
				t.Errorf("stored code %q (%v): not the hash of the code", stored, err)
			}

			// 3. The site host redeems it.
			w = e.redeem("secret", code, st.Name+"="+st.Value)
			if got := location(t, w).String(); got != "/page.html?x=1" {
				t.Errorf("redeem redirects to %q", got)
			}
			for k, want := range map[string]string{"Cache-Control": "no-store", "Referrer-Policy": "no-referrer"} {
				if got := w.Header().Get(k); got != want {
					t.Errorf("redeem %s = %q, want %q", k, got, want)
				}
			}
			if c := cookieNamed(w, s.cookieName(siteStateCookie)); c == nil || c.MaxAge >= 0 {
				t.Errorf("state cookie not cleared: %+v", c)
			}
			c := cookieNamed(w, s.cookieName(siteSessionCookie))
			if c == nil || !c.HttpOnly || c.Secure != secure || c.Domain != "" || c.Path != "/" ||
				c.SameSite != http.SameSiteLaxMode || c.MaxAge <= 0 || c.MaxAge > 3600 {
				t.Fatalf("site cookie = %+v", c)
			}
			if strings.HasPrefix(c.Name, "__Host-") != secure {
				t.Errorf("site cookie name %q", c.Name)
			}
			if c.Value == e.sess[e.member] || strings.Contains(c.Value, "app-session") {
				t.Error("site cookie carries the app session token")
			}
			var hash string
			if err := s.db.QueryRowContext(t.Context(), "SELECT hash FROM site_session").Scan(&hash); err != nil || hash != hashToken(c.Value) {
				t.Errorf("stored site token %q (%v)", hash, err)
			}

			// 4. The cookie opens the page; HEAD works too; content is private.
			for _, m := range []string{"GET", "HEAD"} {
				w = do(s, m, siteHost("secret"), "/page.html", "Cookie", c.Name+"="+c.Value)
				if w.Code != 200 || (m == "GET" && w.Body.String() != "page") || !strings.Contains(w.Header().Get("Cache-Control"), "private") {
					t.Errorf("%s with site cookie: %d %q %q", m, w.Code, w.Body, w.Header().Get("Cache-Control"))
				}
				if w.Header().Get("Access-Control-Allow-Origin") != "" {
					t.Error("CORS header on a site")
				}
			}
			w = do(s, "HEAD", siteHost("secret"), "/")
			if w.Code != http.StatusFound {
				t.Errorf("anonymous HEAD: %d", w.Code)
			}
		})
	}
}

func TestSiteLoginPorts(t *testing.T) {
	e := newLoginEnv(t, false)
	w := do(e.s, "GET", siteHost("secret")+":8443", "/")
	toApp := location(t, w)
	if toApp.Host != testAppHost+":8443" {
		t.Errorf("app host with port: %q", toApp.Host)
	}
	w = do(e.s, "GET", testAppHost+":8443", toApp.RequestURI(), "Cookie", e.appCookie(e.member))
	if h := location(t, w).Host; h != siteHost("secret")+":8443" {
		t.Errorf("site host with port: %q", h)
	}
}

func TestSiteLoginCodeSingleUse(t *testing.T) {
	e := newLoginEnv(t, false)
	code, st := e.handoff(t, e.member, "secret")
	if w := e.redeem("secret", code, st); w.Code != http.StatusFound {
		t.Fatalf("first redeem: %d", w.Code)
	}
	if w := e.redeem("secret", code, st); w.Code != http.StatusNotFound || cookieNamed(w, e.s.cookieName(siteSessionCookie)) != nil {
		t.Errorf("second redeem: %d", w.Code)
	}
	if n := countRows(t, e.s, "site_session"); n != 1 {
		t.Errorf("%d site sessions", n)
	}
	if w := e.redeem("secret", "nope", st); w.Code != http.StatusNotFound {
		t.Errorf("unknown code: %d", w.Code)
	}
}

func TestSiteLoginCodeExpires(t *testing.T) {
	e := newLoginEnv(t, false)
	code, st := e.handoff(t, e.member, "secret")
	if _, err := e.s.db.ExecContext(t.Context(), "UPDATE site_login_code SET expires = ?", time.Now().Unix()-1); err != nil {
		t.Fatal(err)
	}
	if w := e.redeem("secret", code, st); w.Code != http.StatusNotFound || cookieNamed(w, e.s.cookieName(siteSessionCookie)) != nil {
		t.Errorf("expired code: %d", w.Code)
	}
	// Expired codes are pruned when the next one is issued.
	e.handoff(t, e.member, "secret")
	if n := countRows(t, e.s, "site_login_code"); n != 1 {
		t.Errorf("%d codes left", n)
	}
	var exp int64
	if err := e.s.db.QueryRowContext(t.Context(), "SELECT expires FROM site_login_code").Scan(&exp); err != nil {
		t.Fatal(err)
	}
	if d := exp - time.Now().Unix(); d <= 0 || d > 60 {
		t.Errorf("code lifetime %ds", d)
	}
}

func TestSiteLoginCodeBoundToSite(t *testing.T) {
	e := newLoginEnv(t, false)
	code, st := e.handoff(t, e.member, "secret")
	// Same state cookie, other site host: rejected, and the code is spent.
	if w := e.redeem("intra", code, st); w.Code != http.StatusNotFound || cookieNamed(w, e.s.cookieName(siteSessionCookie)) != nil {
		t.Errorf("code on another site: %d", w.Code)
	}
	if w := e.redeem("secret", code, st); w.Code != http.StatusNotFound {
		t.Errorf("code after failed attempt: %d", w.Code)
	}
}

func TestSiteLoginStateCookie(t *testing.T) {
	e := newLoginEnv(t, false)
	other := e.s.cookieName(siteStateCookie) + "=" + strings.Repeat("0", 64)
	for name, cookie := range map[string]string{"missing": "", "mismatched": other} {
		code, st := e.handoff(t, e.member, "secret")
		_ = st
		w := e.redeem("secret", code, cookie)
		if w.Code != http.StatusNotFound || cookieNamed(w, e.s.cookieName(siteSessionCookie)) != nil {
			t.Errorf("%s state cookie: %d", name, w.Code)
		}
		// The code is consumed even by a failed attempt.
		if w := e.redeem("secret", code, st); w.Code != http.StatusNotFound {
			t.Errorf("%s: code reusable after failure: %d", name, w.Code)
		}
	}
	if n := countRows(t, e.s, "site_session"); n != 0 {
		t.Errorf("%d site sessions", n)
	}
}

func TestSiteLoginReturnPath(t *testing.T) {
	for in, want := range map[string]string{
		"":                              "/",
		"/":                             "/",
		"/docs/a.html?x=1#f":            "/docs/a.html?x=1#f",
		"//evil.example":                "/",
		"//evil.example/x":              "/",
		"https://evil.example":          "/",
		"evil.example/x":                "/",
		"/\\evil.example":               "/",
		"/a\\b":                         "/",
		"/a\x00b":                       "/",
		"/a\r\nSet-Cookie: x=y":         "/",
		"/a\tb":                         "/",
		"/a b":                          "/",
		"/__open-pages/auth":            "/",
		"/__open-pages/auth?x=1":        "/",
		"/x/../__open-pages/a":          "/",
		"/" + strings.Repeat("a", 2048): "/",
		"/" + strings.Repeat("a", 2047): "/" + strings.Repeat("a", 2047),
	} {
		if got := siteReturnPath(in); got != want {
			t.Errorf("siteReturnPath(%q) = %q, want %q", in, got, want)
		}
	}

	// Through the handler: an unsafe return falls back to "/" and is what the redeem sends back to.
	e := newLoginEnv(t, false)
	for _, ret := range []string{"//evil.example", "https://evil.example", "/a\\b", "/a%00b\x01", "/__open-pages/auth"} {
		state := strings.Repeat("a", 64)
		target := siteLoginPath + "?" + url.Values{"site": {"secret"}, "return": {ret}, "state": {state}}.Encode()
		w := do(e.s, "GET", testAppHost, target, "Cookie", e.appCookie(e.member))
		code := location(t, w).Query().Get("code")
		st := e.s.cookieName(siteStateCookie) + "=" + state
		w = e.redeem("secret", code, st)
		if got := location(t, w).String(); got != "/" {
			t.Errorf("return %q redirected to %q", ret, got)
		}
	}
}

func TestSiteLoginAppHost(t *testing.T) {
	e := newLoginEnv(t, false)
	state := strings.Repeat("b", 64)
	get := func(site, state string, uid int64) *httptest.ResponseRecorder {
		target := siteLoginPath + "?" + url.Values{"site": {site}, "return": {"/p"}, "state": {state}}.Encode()
		hdr := []string{}
		if uid != 0 {
			hdr = append(hdr, "Cookie", e.appCookie(uid))
		}
		return do(e.s, "GET", testAppHost, target, hdr...)
	}

	// Not signed in: to the UI, which comes back here after login.
	w := get("secret", state, 0)
	u := location(t, w)
	next := u.Query().Get("next")
	if u.Path != "/index" || next != siteLoginPath+"?return=%2Fp&site=secret&state="+state {
		t.Errorf("sign-in redirect = %q", w.Header().Get("Location"))
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q", w.Header().Get("Cache-Control"))
	}
	if n := countRows(t, e.s, "site_login_code"); n != 0 {
		t.Errorf("%d codes", n)
	}

	// Signed in but not allowed, or no such site, or a bad name: the same plain 404, no code.
	denied := get("secret", state, e.outsider)
	for what, w := range map[string]*httptest.ResponseRecorder{
		"missing site": get("ghost", state, e.member),
		"bad name":     get("A.b", state, e.member),
		"denied":       denied,
	} {
		if w.Code != http.StatusNotFound || w.Body.String() != denied.Body.String() || w.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d %q", what, w.Code, w.Body)
		}
	}
	if n := countRows(t, e.s, "site_login_code"); n != 0 {
		t.Errorf("%d codes after refusals", n)
	}
	if w := get("secret", "short", e.member); w.Code != http.StatusBadRequest {
		t.Errorf("bad state: %d", w.Code)
	}
	// An authenticated site opens for any signed-in user; a bearer token is not a session.
	if w := get("intra", state, e.outsider); w.Code != http.StatusFound {
		t.Errorf("authenticated site: %d", w.Code)
	}
}

func TestSiteLoginAccessChangesApplyAtOnce(t *testing.T) {
	e := newLoginEnv(t, false)
	cookie := e.signIn(t, e.member, "secret")
	if w := do(e.s, "GET", siteHost("secret"), "/", "Cookie", cookie); w.Code != 200 {
		t.Fatalf("member: %d", w.Code)
	}
	if _, err := e.s.db.ExecContext(t.Context(), "DELETE FROM user_group WHERE uid = ?", e.member); err != nil {
		t.Fatal(err)
	}
	// The cookie is still valid, but the user may no longer see the site: a plain 404.
	w := do(e.s, "GET", siteHost("secret"), "/", "Cookie", cookie)
	if w.Code != http.StatusNotFound || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("after removal: %d", w.Code)
	}
	// Narrowing the visibility works the same way.
	cookie = e.signIn(t, e.outsider, "intra")
	if _, err := e.s.db.ExecContext(t.Context(), "UPDATE docs SET visibility = 'restricted' WHERE name = 'intra'"); err != nil {
		t.Fatal(err)
	}
	if w := do(e.s, "GET", siteHost("intra"), "/", "Cookie", cookie); w.Code != http.StatusNotFound {
		t.Errorf("after restricting: %d", w.Code)
	}
}

func TestSiteLoginSiteCookieIsPerSite(t *testing.T) {
	e := newLoginEnv(t, false)
	// The owner may see both sites; the cookie for one is no good on the other, replayed or not.
	cookie := e.signIn(t, e.owner, "secret")
	w := do(e.s, "GET", siteHost("intra"), "/", "Cookie", cookie)
	if w.Code != http.StatusFound || !strings.Contains(w.Header().Get("Location"), siteLoginPath) {
		t.Errorf("secret's cookie on intra: %d", w.Code)
	}
	if w := do(e.s, "GET", siteHost("secret"), "/", "Cookie", cookie); w.Code != 200 {
		t.Errorf("secret's cookie on secret: %d", w.Code)
	}
	// A missing site and a refusal look alike for the same cookie.
	cookie = e.signIn(t, e.outsider, "intra")
	if _, err := e.s.db.ExecContext(t.Context(), "UPDATE site_session SET site = 'secret', siteid = (SELECT docid FROM docs WHERE name = 'secret')"); err != nil {
		t.Fatal(err)
	}
	if w := do(e.s, "GET", siteHost("intra"), "/", "Cookie", cookie); w.Code != http.StatusFound {
		t.Errorf("cookie bound to another site: %d", w.Code)
	}
	if w := do(e.s, "GET", siteHost("secret"), "/", "Cookie", cookie); w.Code != http.StatusNotFound {
		t.Errorf("outsider with a valid cookie on secret: %d", w.Code)
	}
}

func TestSiteLoginLogoutRevokes(t *testing.T) {
	e := newLoginEnv(t, false)
	cookie := e.signIn(t, e.member, "secret")
	other := e.signIn(t, e.owner, "secret")
	r := httptest.NewRequest(http.MethodPost, "/v1/auth/logout", nil)
	r.Host = testAppHost
	r.Header.Set("Cookie", e.appCookie(e.member))
	w := httptest.NewRecorder()
	e.s.routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("logout: %d", w.Code)
	}
	if w := do(e.s, "GET", siteHost("secret"), "/", "Cookie", cookie); w.Code != http.StatusFound {
		t.Errorf("site cookie after logout: %d", w.Code)
	}
	if w := do(e.s, "GET", siteHost("secret"), "/", "Cookie", other); w.Code != 200 {
		t.Errorf("another user's site cookie: %d", w.Code)
	}
	if n := countRows(t, e.s, "site_session"); n != 1 {
		t.Errorf("%d site sessions left", n)
	}
	// An unredeemed code dies with the session too.
	e.handoff(t, e.owner, "secret")
	r = httptest.NewRequest(http.MethodPost, "/v1/auth/logout", nil)
	r.Host = testAppHost
	r.Header.Set("Cookie", e.appCookie(e.owner))
	e.s.routes().ServeHTTP(httptest.NewRecorder(), r)
	if n := countRows(t, e.s, "site_login_code") + countRows(t, e.s, "site_session"); n != 0 {
		t.Errorf("%d rows left after logout", n)
	}
}

func TestSiteLoginExpiry(t *testing.T) {
	e := newLoginEnv(t, false)
	// The site cookie never outlives the app session.
	if _, err := e.s.db.ExecContext(t.Context(), "UPDATE session SET expires = ? WHERE userid = ?", time.Now().Unix()+120, e.member); err != nil {
		t.Fatal(err)
	}
	code, st := e.handoff(t, e.member, "secret")
	w := e.redeem("secret", code, st)
	c := cookieNamed(w, e.s.cookieName(siteSessionCookie))
	if c == nil || c.MaxAge <= 0 || c.MaxAge > 120 {
		t.Fatalf("site cookie = %+v", c)
	}
	cookie := c.Name + "=" + c.Value
	// An expired site session, or an expired app session behind it, is no session.
	if _, err := e.s.db.ExecContext(t.Context(), "UPDATE site_session SET expires = ?", time.Now().Unix()-1); err != nil {
		t.Fatal(err)
	}
	if w := do(e.s, "GET", siteHost("secret"), "/", "Cookie", cookie); w.Code != http.StatusFound {
		t.Errorf("expired site session: %d", w.Code)
	}
	if _, err := e.s.db.ExecContext(t.Context(), "UPDATE site_session SET expires = ?", time.Now().Unix()+100); err != nil {
		t.Fatal(err)
	}
	if w := do(e.s, "GET", siteHost("secret"), "/", "Cookie", cookie); w.Code != 200 {
		t.Fatalf("live site session: %d", w.Code)
	}
	if _, err := e.s.db.ExecContext(t.Context(), "UPDATE session SET expires = ?", time.Now().Unix()-1); err != nil {
		t.Fatal(err)
	}
	if w := do(e.s, "GET", siteHost("secret"), "/", "Cookie", cookie); w.Code != http.StatusFound {
		t.Errorf("expired app session: %d", w.Code)
	}
}

func TestSiteLoginMissingLooksLikeDenied(t *testing.T) {
	e := newLoginEnv(t, false)
	// Anonymous: a missing site, a restricted one and an authenticated one all redirect alike.
	var want string
	for _, site := range []string{"ghost", "secret", "intra"} {
		w := do(e.s, "GET", siteHost(site), "/p")
		l := location(t, w)
		l.RawQuery = ""
		if w.Body.Len() != 0 || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Type") != "" {
			t.Errorf("%s: body %q headers %v", site, w.Body, w.Header())
		}
		if site == "ghost" {
			want = l.String()
		} else if l.String() != want {
			t.Errorf("%s redirects to %q, want %q", site, l, want)
		}
	}
	// Signed in with a site cookie but not allowed: the plain 404. A cookie left over for a
	// site that is gone counts for nothing, like no cookie at all.
	cookie := e.signIn(t, e.outsider, "intra")
	if _, err := e.s.db.ExecContext(t.Context(), "UPDATE docs SET visibility = 'restricted' WHERE name = 'intra'"); err != nil {
		t.Fatal(err)
	}
	if denied := do(e.s, "GET", siteHost("intra"), "/", "Cookie", cookie); denied.Code != http.StatusNotFound {
		t.Errorf("denied with a valid cookie: %d", denied.Code)
	}
	if _, err := e.s.db.ExecContext(t.Context(), "UPDATE site_session SET site = 'ghost'"); err != nil {
		t.Fatal(err)
	}
	stale := do(e.s, "GET", siteHost("ghost"), "/", "Cookie", cookie)
	anon := do(e.s, "GET", siteHost("ghost"), "/")
	if stale.Code != http.StatusFound || anon.Code != http.StatusFound {
		t.Errorf("missing site: stale cookie %d, no cookie %d, want 302 for both", stale.Code, anon.Code)
	}
}

func TestSiteLoginBearerNeverRedirects(t *testing.T) {
	e := newLoginEnv(t, false)
	if _, err := e.s.db.ExecContext(t.Context(),
		"INSERT INTO api_token (userid, name, prefix, hash, created) VALUES (?, 'ci', 'opt_', ?, ?)",
		e.member, hashToken("opt_member"), time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	cookie := e.signIn(t, e.member, "secret")
	for _, tc := range []struct {
		name, bearer, cookie string
		status               int
	}{
		{"valid token", "opt_member", "", 200},
		{"invalid token", "opt_nope", "", 404},
		{"invalid token, valid site cookie", "opt_nope", cookie, 404},
		{"empty bearer", "", cookie, 404},
	} {
		hdr := []string{"Authorization", "Bearer " + tc.bearer}
		if tc.cookie != "" {
			hdr = append(hdr, "Cookie", tc.cookie)
		}
		for _, site := range []string{"secret", "ghost"} {
			want := tc.status
			if site == "ghost" {
				want = 404
			}
			if w := do(e.s, "GET", siteHost(site), "/", hdr...); w.Code != want || w.Header().Get("Location") != "" {
				t.Errorf("%s on %s: %d", tc.name, site, w.Code)
			}
		}
	}
}

func TestSiteReservedPrefix(t *testing.T) {
	e := newLoginEnv(t, false)
	// Every site ships a file at __open-pages/auth; it must never be served, on any host.
	for _, site := range []string{"open", "secret"} {
		cookie := e.signIn(t, e.owner, "secret")
		for _, target := range []string{
			"/__open-pages/auth", "/__open-pages/other", "/__open-pages/", "/__open-pages",
			"/x/../__open-pages/auth", "/__open-pages/auth/", "/__open-pages/auth?code=x",
		} {
			w := do(e.s, "GET", siteHost(site), target, "Cookie", cookie)
			if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "shadow") {
				t.Errorf("%s %s: %d %q", site, target, w.Code, w.Body)
			}
			if w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("%s %s: Cache-Control %q", site, target, w.Header().Get("Cache-Control"))
			}
		}
	}
	// Only GET redeems.
	if w := do(e.s, "HEAD", siteHost("open"), "/__open-pages/auth?code=x"); w.Code != http.StatusNotFound {
		t.Errorf("HEAD: %d", w.Code)
	}
}

func TestSiteLoginPublicSiteUnchanged(t *testing.T) {
	e := newLoginEnv(t, false)
	for _, m := range []string{"GET", "HEAD"} {
		w := do(e.s, m, siteHost("open"), "/")
		if w.Code != 200 || w.Header().Get("Location") != "" || cookieNamed(w, e.s.cookieName(siteStateCookie)) != nil {
			t.Errorf("public %s: %d", m, w.Code)
		}
	}
	if w := do(e.s, "GET", siteHost("open"), "/nope"); w.Code != 404 {
		t.Errorf("public missing page: %d", w.Code)
	}
}

func TestSiteDeleteRevokesSiteLogins(t *testing.T) {
	e := newLoginEnv(t, false)
	e.signIn(t, e.member, "secret")
	site, err := e.s.getSite(t.Context(), "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.deleteSite(t.Context(), site); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, e.s, "site_session"); n != 0 {
		t.Errorf("%d site sessions left for a deleted site", n)
	}
}

// Rows are tied to the site's id, not just its name: deleting a site removes pending codes
// too, and a site recreated under the same name rejects cookies and codes of the old one.
func TestRecreatedSiteRejectsOldLogins(t *testing.T) {
	e := newLoginEnv(t, false)
	ctx := t.Context()
	cookie := e.signIn(t, e.member, "secret")
	pending, st := e.handoff(t, e.member, "secret")

	site, err := e.s.getSite(ctx, "secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.s.deleteSite(ctx, site); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, e.s, "site_login_code") + countRows(t, e.s, "site_session"); n != 0 {
		t.Errorf("%d login rows left after deleting the site", n)
	}

	// Bring back stale rows as a race would have left them (the foreign key refuses them
	// for a missing site, so recreate first), then check the id binding.
	if _, err := e.s.createSite(ctx, "secret", "", e.owner, e.group, visRestricted); err != nil {
		t.Fatal(err)
	}
	if _, err := deployNamed(t, e.s, "secret", writeZip(t, map[string]string{"index.html": "new"})); err != nil {
		t.Fatal(err)
	}
	if w := do(e.s, "GET", siteHost("secret"), "/", "Cookie", cookie); w.Code == http.StatusOK {
		t.Error("old cookie works on the recreated site")
	}
	if w := e.redeem("secret", pending, st); cookieNamed(w, e.s.cookieName(siteSessionCookie)) != nil {
		t.Error("old code redeemed on the recreated site")
	}
	// A fresh login works.
	cookie = e.signIn(t, e.member, "secret")
	if w := do(e.s, "GET", siteHost("secret"), "/", "Cookie", cookie); w.Code != http.StatusOK {
		t.Errorf("fresh login on the recreated site: %d", w.Code)
	}
}

// A planted cookie of the same name (possible without cookie_secure) must not shadow the real
// site cookie, nor the state cookie at redeem.
func TestSiteLoginIgnoresShadowCookies(t *testing.T) {
	e := newLoginEnv(t, false)
	name := e.s.cookieName(siteSessionCookie)
	good := e.signIn(t, e.member, "secret")

	w := do(e.s, "GET", siteHost("secret"), "/", "Cookie", name+"=junk; "+good)
	if w.Code != http.StatusOK {
		t.Errorf("real cookie shadowed by a junk one: status %d", w.Code)
	}

	code, st := e.handoff(t, e.member, "secret")
	w = e.redeem("secret", code, siteStateCookie+"=junk; "+st)
	if cookieNamed(w, name) == nil {
		t.Errorf("redeem shadowed by a junk state cookie: status %d", w.Code)
	}
}

// Any Authorization header is an authentication attempt: it is never answered by a cookie
// and never redirected, so a Basic header next to a valid site cookie gets the plain 404.
func TestSiteLoginAnyAuthorizationHeaderBlocksCookie(t *testing.T) {
	e := newLoginEnv(t, false)
	cookie := e.signIn(t, e.member, "secret")
	expectStatus(t, do(e.s, "GET", siteHost("secret"), "/", "Cookie", cookie).Code, http.StatusOK)
	for _, auth := range []string{"Basic dTpw", "Token abc", "Bearer nope", ""} {
		w := do(e.s, "GET", siteHost("secret"), "/", "Cookie", cookie, "Authorization", auth)
		expectStatus(t, w.Code, http.StatusNotFound)
		w = do(e.s, "GET", siteHost("secret"), "/", "Authorization", auth)
		expectStatus(t, w.Code, http.StatusNotFound)
	}
}

// Every outcome of the app-host route is uncacheable, including errors.
func TestSiteLoginNoStoreEverywhere(t *testing.T) {
	e := newLoginEnv(t, false)
	for _, target := range []string{
		siteLoginPath + "?site=Bad_Name&state=x",
		siteLoginPath + "?site=secret&state=short",
	} {
		w := do(e.s, "GET", testAppHost, target)
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control %q", target, got)
		}
	}
}

// A stale code arriving first (two tabs logging in to the same site) fails without cancelling
// the handoff whose state cookie the browser now holds.
func TestSiteLoginStaleHandoffKeepsStateCookie(t *testing.T) {
	e := newLoginEnv(t, false)
	codeA, _ := e.handoff(t, e.member, "secret")
	codeB, stB := e.handoff(t, e.member, "secret") // the browser's cookie is now B's

	w := e.redeem("secret", codeA, stB)
	expectStatus(t, w.Code, http.StatusNotFound)
	if c := cookieNamed(w, siteStateCookie); c != nil {
		t.Errorf("a failed redeem touched the state cookie: %v", c)
	}
	w = e.redeem("secret", codeB, stB)
	if cookieNamed(w, e.s.cookieName(siteSessionCookie)) == nil {
		t.Errorf("the newer handoff was cancelled: status %d", w.Code)
	}
	if c := cookieNamed(w, siteStateCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("state cookie not cleared after a matched redeem: %v", c)
	}
}

// Only a handful of same-named cookies are tried, so a flood of them can't turn one request
// into hundreds of lookups; a real cookie within the first few still works.
func TestSiteLoginCapsSameNameCookies(t *testing.T) {
	e := newLoginEnv(t, false)
	name := e.s.cookieName(siteSessionCookie)
	good := e.signIn(t, e.member, "secret")

	junk := strings.Repeat(name+"=junk; ", maxSameNameCookies)
	if w := do(e.s, "GET", siteHost("secret"), "/", "Cookie", junk+good); w.Code == http.StatusOK {
		t.Error("a cookie beyond the cap was still tried")
	}
	if w := do(e.s, "GET", siteHost("secret"), "/", "Cookie", strings.Repeat(name+"=junk; ", maxSameNameCookies-1)+good); w.Code != http.StatusOK {
		t.Errorf("a cookie within the cap was ignored: %d", w.Code)
	}
}
