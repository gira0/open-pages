package main

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testClientID     = "open-pages"
	testClientSecret = "s3cret-value"
	testRedirectURL  = "http://localhost/v1/auth/oidc/callback"
)

// idpSettings is what the fake provider puts into the next ID token.
type idpSettings struct {
	sub, email string
	groups     []string
	alg        string // RS256 (default), ES256 or none
	badSig     bool   // sign with a key the JWKS doesn't publish
	nonce      string // replaces the real nonce when set
	tweak      func(claims map[string]any)
}

// fakeIDP is an in-process OpenID provider with discovery, JWKS, authorize and token endpoints.
type fakeIDP struct {
	srv     *httptest.Server
	rsaKey  *rsa.PrivateKey
	ecKey   *ecdsa.PrivateKey
	rogue   *rsa.PrivateKey
	mu      sync.Mutex
	set     idpSettings
	codes   map[string]idpAuthRequest
	tokenOK int // successful token requests
}

type idpAuthRequest struct{ nonce, challenge string }

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	p := &fakeIDP{codes: map[string]idpAuthRequest{}, set: idpSettings{sub: "sub-1", email: "ann@corp.example"}}
	var err error
	if p.rsaKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		t.Fatal(err)
	}
	if p.rogue, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		t.Fatal(err)
	}
	if p.ecKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{
			"issuer":                 p.srv.URL,
			"authorization_endpoint": p.srv.URL + "/authorize",
			"token_endpoint":         p.srv.URL + "/token",
			"jwks_uri":               p.srv.URL + "/jwks",
		})
	})
	mux.HandleFunc("GET /jwks", func(w http.ResponseWriter, r *http.Request) {
		pt, _ := p.ecKey.PublicKey.Bytes() // 0x04 || X || Y
		writeJSON(w, http.StatusOK, map[string]any{"keys": []jwk{
			{Kty: "RSA", Kid: "rsa1", Use: "sig", Alg: "RS256",
				N: b64(p.rsaKey.N.Bytes()), E: b64(big.NewInt(int64(p.rsaKey.E)).Bytes())},
			{Kty: "EC", Kid: "ec1", Use: "sig", Crv: "P-256", X: b64(pt[1:33]), Y: b64(pt[33:])},
		}})
	})
	mux.HandleFunc("POST /token", p.handleToken)
	p.srv = httptest.NewServer(mux)
	t.Cleanup(p.srv.Close)
	return p
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func pad32(b []byte) []byte {
	return append(make([]byte, 32-len(b)), b...)
}

func (p *fakeIDP) exchanged() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tokenOK
}

func (p *fakeIDP) update(f func(s *idpSettings)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f(&p.set)
}

// authorize plays the user's approval at the provider: it takes the URL the login endpoint
// redirected to and returns the callback query the provider would send the browser back with.
func (p *fakeIDP) authorize(t *testing.T, location string) url.Values {
	t.Helper()
	u, err := url.Parse(location)
	if err != nil || !strings.HasPrefix(location, p.srv.URL+"/authorize?") {
		t.Fatalf("login redirected to %q, want the provider's authorize endpoint", location)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != testClientID || q.Get("redirect_uri") != testRedirectURL ||
		q.Get("code_challenge_method") != "S256" || q.Get("state") == "" || q.Get("nonce") == "" || q.Get("code_challenge") == "" {
		t.Fatalf("unexpected authorize parameters: %v", q)
	}
	for _, scope := range []string{"openid", "email"} {
		if !strings.Contains(" "+q.Get("scope")+" ", " "+scope+" ") {
			t.Fatalf("scope %q lacks %s", q.Get("scope"), scope)
		}
	}
	code, err := generateToken()
	if err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.codes[code] = idpAuthRequest{nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	p.mu.Unlock()
	return url.Values{"code": {code}, "state": {q.Get("state")}}
}

func (p *fakeIDP) handleToken(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, secret, ok := r.BasicAuth()
	if !ok || id != testClientID || secret != testClientSecret ||
		r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("redirect_uri") != testRedirectURL {
		http.Error(w, "bad client", http.StatusUnauthorized)
		return
	}
	p.mu.Lock()
	req, found := p.codes[r.PostForm.Get("code")]
	delete(p.codes, r.PostForm.Get("code")) // codes are single use
	s := p.set
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !found || b64(sum[:]) != req.challenge {
		http.Error(w, "bad grant", http.StatusBadRequest)
		return
	}
	nonce := req.nonce
	if s.nonce != "" {
		nonce = s.nonce
	}
	claims := map[string]any{
		"iss": p.srv.URL, "aud": testClientID, "sub": s.sub, "email": s.email, "email_verified": true,
		"nonce": nonce, "iat": time.Now().Unix(), "exp": time.Now().Add(5 * time.Minute).Unix(),
	}
	if s.groups != nil {
		claims["groups"] = s.groups
	}
	if s.tweak != nil {
		s.tweak(claims)
	}
	p.mu.Lock()
	p.tokenOK++
	p.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"id_token": p.sign(s, claims), "token_type": "Bearer"})
}

func (p *fakeIDP) sign(s idpSettings, claims map[string]any) string {
	hdr := map[string]string{"alg": "RS256", "kid": "rsa1", "typ": "JWT"}
	if s.alg != "" {
		hdr["alg"] = s.alg
	}
	if s.alg == "ES256" {
		hdr["kid"] = "ec1"
	}
	hb, _ := json.Marshal(hdr)
	cb, _ := json.Marshal(claims)
	input := b64(hb) + "." + b64(cb)
	digest := sha256.Sum256([]byte(input))
	var sig []byte
	switch s.alg {
	case "none":
	case "ES256":
		der, _ := ecdsa.SignASN1(rand.Reader, p.ecKey, digest[:])
		var rs struct{ R, S *big.Int }
		_, _ = asn1.Unmarshal(der, &rs)
		sig = append(pad32(rs.R.Bytes()), pad32(rs.S.Bytes())...)
	default:
		key := p.rsaKey
		if s.badSig {
			key = p.rogue
		}
		sig, _ = rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	}
	return input + "." + b64(sig)
}

// newOIDCTestServer starts open-pages with OIDC pointed at idp. mutate adjusts the config.
func newOIDCTestServer(t *testing.T, idp *fakeIDP, mutate func(*Config)) (*httptest.Server, *Server) {
	t.Helper()
	cfg := defaultConfig()
	cfg.DataPath, cfg.TmpPath = t.TempDir(), t.TempDir()
	cfg.OIDC = OIDCConfig{
		Enabled: true, Issuer: idp.srv.URL, ClientID: testClientID, ClientSecret: testClientSecret,
		RedirectURL: testRedirectURL, Scopes: []string{"openid", "email", "profile"},
		EmailClaim: "email", GroupsClaim: "groups",
	}
	if mutate != nil {
		mutate(&cfg)
	}
	s, err := newServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(func() { ts.Close(); s.db.Close() })
	return ts, s
}

// browser returns a cookie-keeping client that does not follow redirects.
func browser(t *testing.T) *http.Client {
	t.Helper()
	c := newClient(t)
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

func oidcGet(t *testing.T, c *http.Client, target string) (int, string, http.Header) {
	t.Helper()
	resp, err := c.Get(target)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(body), resp.Header
}

// signIn runs the whole flow for idp's current settings and returns the callback's status.
func signIn(t *testing.T, ts *httptest.Server, idp *fakeIDP, c *http.Client) int {
	t.Helper()
	code, _, h := oidcGet(t, c, ts.URL+"/v1/auth/oidc/login")
	expectStatus(t, code, http.StatusFound)
	q := idp.authorize(t, h.Get("Location"))
	code, _, _ = oidcGet(t, c, ts.URL+"/v1/auth/oidc/callback?"+q.Encode())
	return code
}

func sessionEmail(t *testing.T, ts *httptest.Server, c *http.Client) string {
	t.Helper()
	code, body, _ := oidcGet(t, c, ts.URL+"/v1/auth/user")
	if code != http.StatusOK {
		return ""
	}
	var info userInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		t.Fatal(err)
	}
	return info.Email
}

func countUsers(t *testing.T, s *Server) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM user").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestOIDCHappyPath(t *testing.T) {
	idp := newFakeIDP(t)
	ts, s := newOIDCTestServer(t, idp, nil)
	c := browser(t)
	expectStatus(t, signIn(t, ts, idp, c), http.StatusOK)
	if got := sessionEmail(t, ts, c); got != "ann@corp.example" {
		t.Fatalf("session email = %q", got)
	}
	var issuer, subject, password string
	if err := s.db.QueryRow("SELECT oidc_issuer, oidc_subject, password FROM user").Scan(&issuer, &subject, &password); err != nil {
		t.Fatal(err)
	}
	if issuer != idp.srv.URL || subject != "sub-1" || password != "" {
		t.Fatalf("user row = %q %q %q", issuer, subject, password)
	}
	// OIDC-created accounts have no password, so local login can't be used against them.
	creds := []byte(`{"email":"ann@corp.example","password":"anything goes"}`)
	expectStatus(t, post(t, newClient(t), ts.URL+"/v1/user/login", "application/json", creds), http.StatusUnauthorized)
}

func TestOIDCRepeatLogin(t *testing.T) {
	idp := newFakeIDP(t)
	ts, s := newOIDCTestServer(t, idp, nil)
	expectStatus(t, signIn(t, ts, idp, browser(t)), http.StatusOK)
	// The same subject with a changed email is the same user.
	idp.update(func(s *idpSettings) { s.email = "ann.new@corp.example" })
	c := browser(t)
	expectStatus(t, signIn(t, ts, idp, c), http.StatusOK)
	if n := countUsers(t, s); n != 1 {
		t.Fatalf("users = %d, want 1", n)
	}
	if got := sessionEmail(t, ts, c); got != "ann@corp.example" {
		t.Fatalf("email = %q, want the one stored at first login", got)
	}
	// A different subject is a different user, even with the same issuer.
	idp.update(func(s *idpSettings) { s.sub, s.email = "sub-2", "bob@corp.example" })
	expectStatus(t, signIn(t, ts, idp, browser(t)), http.StatusOK)
	if n := countUsers(t, s); n != 2 {
		t.Fatalf("users = %d, want 2", n)
	}
}

func TestOIDCES256(t *testing.T) {
	idp := newFakeIDP(t)
	idp.update(func(s *idpSettings) { s.alg = "ES256" })
	ts, _ := newOIDCTestServer(t, idp, nil)
	expectStatus(t, signIn(t, ts, idp, browser(t)), http.StatusOK)
}

func TestOIDCRejectsBadTokens(t *testing.T) {
	cases := map[string]func(s *idpSettings){
		"bad nonce": func(s *idpSettings) { s.nonce = "not-the-nonce" },
		"expired": func(s *idpSettings) {
			s.tweak = func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() }
		},
		"wrong audience": func(s *idpSettings) { s.tweak = func(c map[string]any) { c["aud"] = "someone-else" } },
		"wrong issuer":   func(s *idpSettings) { s.tweak = func(c map[string]any) { c["iss"] = "https://evil.example" } },
		"bad signature":  func(s *idpSettings) { s.badSig = true },
		"alg none":       func(s *idpSettings) { s.alg = "none" },
		"no subject":     func(s *idpSettings) { s.tweak = func(c map[string]any) { delete(c, "sub") } },
		"not yet valid":  func(s *idpSettings) { s.tweak = func(c map[string]any) { c["nbf"] = time.Now().Add(time.Hour).Unix() } },
		"unverified email": func(s *idpSettings) {
			s.tweak = func(c map[string]any) { c["email_verified"] = false }
		},
		"multiple audiences without azp": func(s *idpSettings) {
			s.tweak = func(c map[string]any) { c["aud"] = []string{testClientID, "other"} }
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			idp := newFakeIDP(t)
			idp.update(mutate)
			ts, s := newOIDCTestServer(t, idp, nil)
			c := browser(t)
			if code := signIn(t, ts, idp, c); code != http.StatusUnauthorized && code != http.StatusForbidden {
				t.Fatalf("callback status = %d, want a refusal", code)
			}
			if sessionEmail(t, ts, c) != "" || countUsers(t, s) != 0 {
				t.Fatal("a rejected token produced a session or a user")
			}
		})
	}
}

func TestOIDCAudienceListWithAzp(t *testing.T) {
	idp := newFakeIDP(t)
	idp.update(func(s *idpSettings) {
		s.tweak = func(c map[string]any) { c["aud"] = []string{testClientID, "other"}; c["azp"] = testClientID }
	})
	ts, _ := newOIDCTestServer(t, idp, nil)
	expectStatus(t, signIn(t, ts, idp, browser(t)), http.StatusOK)
}

func TestOIDCBadState(t *testing.T) {
	idp := newFakeIDP(t)
	ts, _ := newOIDCTestServer(t, idp, nil)

	t.Run("state not issued", func(t *testing.T) {
		c := browser(t)
		_, _, h := oidcGet(t, c, ts.URL+"/v1/auth/oidc/login")
		q := idp.authorize(t, h.Get("Location"))
		q.Set("state", "forged")
		code, _, _ := oidcGet(t, c, ts.URL+"/v1/auth/oidc/callback?"+q.Encode())
		expectStatus(t, code, http.StatusBadRequest)
		if idp.exchanged() != 0 {
			t.Fatal("the code was exchanged despite a bad state")
		}
	})
	t.Run("no cookie", func(t *testing.T) {
		_, _, h := oidcGet(t, browser(t), ts.URL+"/v1/auth/oidc/login")
		q := idp.authorize(t, h.Get("Location"))
		// Another browser, without the state cookie, completes the callback.
		code, _, _ := oidcGet(t, browser(t), ts.URL+"/v1/auth/oidc/callback?"+q.Encode())
		expectStatus(t, code, http.StatusBadRequest)
	})
	t.Run("replay", func(t *testing.T) {
		c := browser(t)
		_, _, h := oidcGet(t, c, ts.URL+"/v1/auth/oidc/login")
		q := idp.authorize(t, h.Get("Location"))
		code, _, _ := oidcGet(t, c, ts.URL+"/v1/auth/oidc/callback?"+q.Encode())
		expectStatus(t, code, http.StatusOK)
		code, _, _ = oidcGet(t, c, ts.URL+"/v1/auth/oidc/callback?"+q.Encode())
		expectStatus(t, code, http.StatusBadRequest)
	})
	t.Run("provider error", func(t *testing.T) {
		c := browser(t)
		_, _, h := oidcGet(t, c, ts.URL+"/v1/auth/oidc/login")
		state := idp.authorize(t, h.Get("Location")).Get("state")
		code, _, _ := oidcGet(t, c, ts.URL+"/v1/auth/oidc/callback?error=access_denied&state="+url.QueryEscape(state))
		expectStatus(t, code, http.StatusUnauthorized)
	})
}

func TestOIDCGroupMapping(t *testing.T) {
	idp := newFakeIDP(t)
	ts, s := newOIDCTestServer(t, idp, nil)

	// An admin creates the groups; the provider only maps people into them.
	admin := loggedInClient(t, ts)
	eng := newGroupVia(t, ts, admin, "Eng")
	ops := newGroupVia(t, ts, admin, "ops")

	idp.update(func(s *idpSettings) { s.groups = []string{"eng", "no-such-group", "  "} })
	c := browser(t)
	expectStatus(t, signIn(t, ts, idp, c), http.StatusOK)
	groups := func() map[int64]bool {
		var uid int64
		if err := s.db.QueryRow("SELECT userid FROM user WHERE oidc_subject = 'sub-1'").Scan(&uid); err != nil {
			t.Fatal(err)
		}
		gs, err := s.userGroups(t.Context(), uid)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]bool{}
		for _, g := range gs {
			out[g.ID] = true
		}
		return out
	}
	if g := groups(); len(g) != 1 || !g[eng] {
		t.Fatalf("groups after first login = %v, want only Eng", g)
	}
	var total int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM groups").Scan(&total); err != nil || total != 2 {
		t.Fatalf("groups table has %d rows (%v); unknown names must not create groups", total, err)
	}

	// A membership added by hand survives; provider-managed ones follow the token.
	var uid int64
	if err := s.db.QueryRow("SELECT userid FROM user WHERE oidc_subject = 'sub-1'").Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO user_group (uid, gid) VALUES (?, ?)", uid, ops); err != nil {
		t.Fatal(err)
	}
	idp.update(func(s *idpSettings) { s.groups = []string{"OPS"} })
	expectStatus(t, signIn(t, ts, idp, browser(t)), http.StatusOK)
	if g := groups(); len(g) != 1 || !g[ops] {
		t.Fatalf("groups after second login = %v, want only ops", g)
	}
	idp.update(func(s *idpSettings) { s.groups = nil })
	expectStatus(t, signIn(t, ts, idp, browser(t)), http.StatusOK)
	if g := groups(); len(g) != 1 || !g[ops] {
		t.Fatalf("manual membership lost or provider one kept: %v", g)
	}
}

func TestOIDCGroupsClaimAsString(t *testing.T) {
	idp := newFakeIDP(t)
	ts, s := newOIDCTestServer(t, idp, func(c *Config) { c.OIDC.GroupsClaim = "roles" })
	newGroupVia(t, ts, loggedInClient(t, ts), "admins")
	idp.update(func(s *idpSettings) { s.tweak = func(c map[string]any) { c["roles"] = "admins" } })
	expectStatus(t, signIn(t, ts, idp, browser(t)), http.StatusOK)
	var n int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM user_group WHERE oidc = 1").Scan(&n); err != nil || n != 1 {
		t.Fatalf("provider-managed memberships = %d (%v), want 1", n, err)
	}
}

func TestOIDCNoEmailTakeover(t *testing.T) {
	idp := newFakeIDP(t)
	ts, s := newOIDCTestServer(t, idp, nil)
	userClient(t, ts, "ann@corp.example") // a local account with the same email
	idp.update(func(s *idpSettings) { s.email = "Ann@Corp.example" })
	c := browser(t)
	expectStatus(t, signIn(t, ts, idp, c), http.StatusConflict)
	if sessionEmail(t, ts, c) != "" || countUsers(t, s) != 1 {
		t.Fatal("an OIDC login took over or duplicated a local account")
	}
}

func TestOIDCEmailRules(t *testing.T) {
	idp := newFakeIDP(t)
	ts, s := newOIDCTestServer(t, idp, func(c *Config) { c.OIDC.AllowedEmailDomain = "corp.example" })
	idp.update(func(s *idpSettings) { s.email = "mallory@other.example" })
	expectStatus(t, signIn(t, ts, idp, browser(t)), http.StatusForbidden)
	idp.update(func(s *idpSettings) { s.email = "" })
	expectStatus(t, signIn(t, ts, idp, browser(t)), http.StatusForbidden)
	idp.update(func(s *idpSettings) { s.email = "ann@corp.example" })
	expectStatus(t, signIn(t, ts, idp, browser(t)), http.StatusOK)
	if n := countUsers(t, s); n != 1 {
		t.Fatalf("users = %d, want 1", n)
	}
}

func TestOIDCReturnTo(t *testing.T) {
	idp := newFakeIDP(t)
	ts, _ := newOIDCTestServer(t, idp, nil)
	for _, bad := range []string{"https://evil.example/", "//evil.example", "/\\evil.example", "relative"} {
		code, _, _ := oidcGet(t, browser(t), ts.URL+"/v1/auth/oidc/login?return_to="+url.QueryEscape(bad))
		expectStatus(t, code, http.StatusBadRequest)
	}
	c := browser(t)
	_, _, h := oidcGet(t, c, ts.URL+"/v1/auth/oidc/login?return_to="+url.QueryEscape("/index"))
	q := idp.authorize(t, h.Get("Location"))
	code, _, h := oidcGet(t, c, ts.URL+"/v1/auth/oidc/callback?"+q.Encode())
	expectStatus(t, code, http.StatusSeeOther)
	if h.Get("Location") != "/index" {
		t.Fatalf("redirected to %q", h.Get("Location"))
	}
}

func TestOIDCDisabledLeavesRoutesOut(t *testing.T) {
	ts, s := newTestServer(t)
	if s.oidc != nil {
		t.Fatal("OIDC client built without configuration")
	}
	for _, p := range []string{"/v1/auth/oidc/login", "/v1/auth/oidc/callback"} {
		code, _, _ := oidcGet(t, browser(t), ts.URL+p)
		expectStatus(t, code, http.StatusNotFound)
	}
}

func TestOIDCOnlyDisablesLocalLogin(t *testing.T) {
	idp := newFakeIDP(t)
	ts, _ := newOIDCTestServer(t, idp, func(c *Config) { c.LocalLogin = false })
	creds := []byte(`{"email":"a@example.com","password":"correct horse"}`)
	expectStatus(t, post(t, newClient(t), ts.URL+"/v1/user/register", "application/json", creds), http.StatusMethodNotAllowed)
	expectStatus(t, post(t, newClient(t), ts.URL+"/v1/user/login", "application/json", creds), http.StatusMethodNotAllowed)
	expectStatus(t, signIn(t, ts, idp, browser(t)), http.StatusOK)
}

func TestOIDCProviderDown(t *testing.T) {
	idp := newFakeIDP(t)
	ts, _ := newOIDCTestServer(t, idp, nil)
	idp.srv.Close()
	code, _, _ := oidcGet(t, browser(t), ts.URL+"/v1/auth/oidc/login")
	expectStatus(t, code, http.StatusBadGateway)
}

func TestOIDCSecretNeverServed(t *testing.T) {
	idp := newFakeIDP(t)
	ts, _ := newOIDCTestServer(t, idp, nil)
	c := browser(t)
	_, body, h := oidcGet(t, c, ts.URL+"/v1/auth/oidc/login")
	if strings.Contains(body+h.Get("Location"), testClientSecret) {
		t.Fatal("client secret leaked into the login response")
	}
}

func TestOIDCConcurrentEmailVariants(t *testing.T) {
	idp := newFakeIDP(t)
	_, s := newOIDCTestServer(t, idp, nil)
	ids := []oidcIdentity{
		{issuer: idp.srv.URL, subject: "sub-a", email: "Ann@Corp.example"},
		{issuer: idp.srv.URL, subject: "sub-b", email: "ann@corp.example"},
	}
	errs := make([]error, len(ids))
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = s.oidcUser(t.Context(), ids[i])
		}()
	}
	wg.Wait()
	taken := 0
	for _, err := range errs {
		if errors.Is(err, errOIDCEmailTaken) {
			taken++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if users := countUsers(t, s); taken != 1 || users != 1 {
		t.Fatalf("taken = %d, users = %d; want exactly one insert to win", taken, users)
	}
}

func TestManualAddKeepsMembershipAcrossSync(t *testing.T) {
	idp := newFakeIDP(t)
	ts, s := newOIDCTestServer(t, idp, nil)
	owner := loggedInClient(t, ts)
	gid := newGroupVia(t, ts, owner, "eng")
	idp.update(func(s *idpSettings) { s.groups = []string{"eng"} })
	expectStatus(t, signIn(t, ts, idp, browser(t)), http.StatusOK)

	// The owner adds the provider-managed member by hand; the membership becomes manual.
	code, _ := doJSON(t, owner, http.MethodPost, groupURL(ts, gid, "/members"), `{"email":"ann@corp.example"}`)
	expectStatus(t, code, http.StatusOK)

	idp.update(func(s *idpSettings) { s.groups = nil })
	expectStatus(t, signIn(t, ts, idp, browser(t)), http.StatusOK)
	var n int
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM user_group WHERE gid = ? AND oidc = 0 AND uid IN (SELECT userid FROM user WHERE oidc_subject = 'sub-1')",
		gid).Scan(&n); err != nil || n != 1 {
		t.Fatalf("manual membership survivors = %d (%v), want 1", n, err)
	}
}

func TestIndexShowsEnabledSignInMethods(t *testing.T) {
	idp := newFakeIDP(t)
	for _, tc := range []struct {
		name  string
		local bool
	}{
		{"both", true},
		{"oidc only", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := newOIDCTestServer(t, idp, func(c *Config) { c.LocalLogin = tc.local })
			code, body, _ := oidcGet(t, browser(t), ts.URL+"/index")
			expectStatus(t, code, http.StatusOK)
			if got := strings.Contains(body, `action="/v1/user/login"`); got != tc.local {
				t.Errorf("local form shown = %v, want %v", got, tc.local)
			}
			if !strings.Contains(body, `href="/v1/auth/oidc/login"`) {
				t.Error("OIDC sign-in link missing")
			}
		})
	}
	ts, _ := newTestServer(t)
	_, body, _ := oidcGet(t, browser(t), ts.URL+"/index")
	if strings.Contains(body, "oidc/login") || !strings.Contains(body, `action="/v1/user/login"`) {
		t.Error("default server must show only the local form")
	}
}
