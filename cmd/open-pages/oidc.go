package main

import (
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gira0/open-pages/internal/config"
)

// OpenID Connect sign-in (authorization code flow with PKCE, state and nonce). It is only
// wired up when [oidc] enabled = true; see docs/auth.md for how identities map to users.

const (
	oidcStateCookie  = "oidc_state"
	oidcCookiePath   = "/v1/auth/oidc"
	oidcLoginTTL     = 10 * time.Minute
	oidcMaxPending   = 10000
	oidcHTTPTimeout  = 10 * time.Second
	oidcMaxBody      = 1 << 20
	oidcClockSkew    = time.Minute
	oidcKeysTTL      = time.Hour
	oidcKeysCooldown = time.Minute // minimum gap between JWKS refetches
	oidcMaxGroups    = 500
	maxEmailLen      = 255
)

var (
	errOIDCState      = errors.New("unknown or expired login attempt")
	errOIDCDenied     = errors.New("sign-in refused by the identity provider")
	errOIDCClaims     = errors.New("invalid ID token claims")
	errOIDCNonce      = errors.New("nonce mismatch")
	errOIDCEmail      = errors.New("the identity has no usable email address")
	errOIDCDomain     = errors.New("email domain is not allowed")
	errOIDCEmailTaken = errors.New("an account with this email already exists")
)

// oidcClient talks to the provider and holds the login attempts in flight.
type oidcClient struct {
	cfg  config.OIDCConfig
	http *http.Client

	fetchMu     sync.Mutex // guards meta, keys and keysFetched; held while talking to the provider
	meta        *oidcMeta
	keys        []jwk
	keysFetched time.Time

	mu      sync.Mutex // guards pending
	pending map[string]pendingLogin
}

// oidcMeta is the subset of the discovery document the flow needs.
type oidcMeta struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

// pendingLogin is the server-side half of a login attempt, keyed by the state value.
type pendingLogin struct {
	nonce    string
	verifier string
	returnTo string
	expires  time.Time
}

func newOIDCClient(cfg config.OIDCConfig) *oidcClient {
	return &oidcClient{
		cfg: cfg,
		http: &http.Client{
			Timeout: oidcHTTPTimeout,
			// Providers answer discovery, JWKS and token requests directly; a redirect is an error.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
		pending: make(map[string]pendingLogin),
	}
}

// getJSON fetches url and decodes a JSON object of at most oidcMaxBody bytes into out.
func (c *oidcClient) getJSON(ctx context.Context, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	return c.doJSON(req, out)
}

func (c *oidcClient) doJSON(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, oidcMaxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %d", req.URL.Host, resp.StatusCode)
	}
	return json.Unmarshal(body, out)
}

// metadata returns the provider's discovery document, fetching it on first use so the
// server starts even while the provider is down.
func (c *oidcClient) metadata(ctx context.Context) (*oidcMeta, error) {
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()
	if c.meta != nil {
		return c.meta, nil
	}
	var m oidcMeta
	if err := c.getJSON(ctx, strings.TrimSuffix(c.cfg.Issuer, "/")+"/.well-known/openid-configuration", &m); err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}
	if m.Issuer != c.cfg.Issuer {
		return nil, errors.New("discovery: issuer in the provider's document differs from oidc.issuer")
	}
	for _, e := range []string{m.AuthorizationEndpoint, m.TokenEndpoint, m.JWKSURI} {
		if err := config.CheckOIDCURL("discovery endpoint", e); err != nil {
			return nil, err
		}
	}
	c.meta = &m
	return c.meta, nil
}

// signingKey finds the key for kid and alg. An unknown kid triggers one JWKS refetch
// (rate limited), which covers key rotation.
func (c *oidcClient) signingKey(ctx context.Context, meta *oidcMeta, kid, alg string) (crypto.PublicKey, error) {
	c.fetchMu.Lock()
	defer c.fetchMu.Unlock()
	if key, ok := c.findKey(kid, alg); ok && time.Since(c.keysFetched) < oidcKeysTTL {
		return key, nil
	}
	if time.Since(c.keysFetched) >= oidcKeysCooldown {
		var set struct {
			Keys []jwk `json:"keys"`
		}
		if err := c.getJSON(ctx, meta.JWKSURI, &set); err != nil {
			return nil, fmt.Errorf("fetch keys: %w", err)
		}
		c.keys, c.keysFetched = set.Keys, time.Now()
	}
	if key, ok := c.findKey(kid, alg); ok {
		return key, nil
	}
	return nil, errTokenKey
}

func (c *oidcClient) findKey(kid, alg string) (crypto.PublicKey, bool) {
	wantKty := map[string]string{"RS256": "RSA", "ES256": "EC"}[alg]
	var match []jwk
	for _, k := range c.keys {
		if k.Kty == wantKty && (k.Use == "" || k.Use == "sig") && (k.Alg == "" || k.Alg == alg) && (kid == "" || k.Kid == kid) {
			match = append(match, k)
		}
	}
	// Without a kid, only an unambiguous key will do.
	if len(match) != 1 && (kid == "" || len(match) == 0) {
		return nil, false
	}
	key, err := match[0].publicKey()
	return key, err == nil
}

// idTokenClaims verifies raw (signature, issuer, audience, time bounds) and returns its claims.
// The nonce check is left to the caller.
func (c *oidcClient) idTokenClaims(ctx context.Context, meta *oidcMeta, raw string) (map[string]any, error) {
	hdr, payload, signingInput, sig, err := splitJWT(raw)
	if err != nil {
		return nil, err
	}
	if hdr.Alg != "RS256" && hdr.Alg != "ES256" {
		return nil, errTokenAlg
	}
	key, err := c.signingKey(ctx, meta, hdr.Kid, hdr.Alg)
	if err != nil {
		return nil, err
	}
	if err := verifySignature(hdr.Alg, key, signingInput, sig); err != nil {
		return nil, err
	}
	claims, err := decodeClaims(payload)
	if err != nil {
		return nil, err
	}
	if iss, _ := claims["iss"].(string); iss != c.cfg.Issuer {
		return nil, errOIDCClaims
	}
	if !audienceOK(claims, c.cfg.ClientID) {
		return nil, errOIDCClaims
	}
	now := time.Now()
	exp, ok := numericDate(claims["exp"])
	if !ok || !now.Before(exp.Add(oidcClockSkew)) {
		return nil, errOIDCClaims
	}
	if nbf, ok := numericDate(claims["nbf"]); ok && now.Add(oidcClockSkew).Before(nbf) {
		return nil, errOIDCClaims
	}
	if sub, _ := claims["sub"].(string); sub == "" {
		return nil, errOIDCClaims
	}
	return claims, nil
}

// audienceOK reports whether the token was issued for clientID. With several audiences the
// authorized party (azp) must be this client as well.
func audienceOK(claims map[string]any, clientID string) bool {
	switch aud := claims["aud"].(type) {
	case string:
		return aud == clientID
	case []any:
		if !slices.Contains(aud, any(clientID)) {
			return false
		}
		return len(aud) == 1 || claims["azp"] == clientID
	}
	return false
}

func numericDate(v any) (time.Time, bool) {
	n, ok := v.(json.Number)
	if !ok {
		return time.Time{}, false
	}
	f, err := n.Float64()
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(int64(f), 0), true
}

// exchangeCode trades the authorization code for the raw ID token.
func (c *oidcClient) exchangeCode(ctx context.Context, meta *oidcMeta, code, verifier string) (string, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {c.cfg.RedirectURL},
		"code_verifier": {verifier},
		"client_id":     {c.cfg.ClientID},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// client_secret_basic: both parts form-urlencoded first (RFC 6749 section 2.3.1).
	req.SetBasicAuth(url.QueryEscape(c.cfg.ClientID), url.QueryEscape(c.cfg.ClientSecret))
	var tok struct {
		IDToken string `json:"id_token"`
	}
	if err := c.doJSON(req, &tok); err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	if tok.IDToken == "" {
		return "", errTokenMalformed
	}
	return tok.IDToken, nil
}

// begin records a login attempt and returns its state value and the provider URL to send
// the browser to. It returns false when too many attempts are pending.
func (c *oidcClient) begin(meta *oidcMeta, returnTo string) (state, redirect string, ok bool) {
	state, err1 := generateToken()
	nonce, err2 := generateToken()
	verifier, err3 := generateToken()
	if err1 != nil || err2 != nil || err3 != nil {
		return "", "", false
	}
	now := time.Now()
	c.mu.Lock()
	for k, p := range c.pending {
		if now.After(p.expires) {
			delete(c.pending, k)
		}
	}
	if len(c.pending) >= oidcMaxPending {
		c.mu.Unlock()
		return "", "", false
	}
	c.pending[state] = pendingLogin{nonce: nonce, verifier: verifier, returnTo: returnTo, expires: now.Add(oidcLoginTTL)}
	c.mu.Unlock()

	challenge := sha256.Sum256([]byte(verifier))
	u, err := url.Parse(meta.AuthorizationEndpoint)
	if err != nil {
		return "", "", false
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", c.cfg.ClientID)
	q.Set("redirect_uri", c.cfg.RedirectURL)
	q.Set("scope", strings.Join(c.cfg.Scopes, " "))
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return state, u.String(), true
}

// take removes and returns the login attempt for state if it is still valid.
func (c *oidcClient) take(state string) (pendingLogin, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p, ok := c.pending[state]
	delete(c.pending, state)
	return p, ok && time.Now().Before(p.expires)
}

// safeReturnTo accepts only a local absolute path, so login can't redirect off site.
func safeReturnTo(p string) bool {
	if p == "" || len(p) > 512 || p[0] != '/' || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\\r\n") {
		return false
	}
	u, err := url.Parse(p)
	return err == nil && u.Scheme == "" && u.Host == ""
}

// handleOIDCLogin sends the browser to the provider. An optional return_to (a local path)
// is where the callback sends it afterwards.
func (s *Server) handleOIDCLogin(w http.ResponseWriter, r *http.Request) {
	returnTo := r.URL.Query().Get("return_to")
	if returnTo != "" && !safeReturnTo(returnTo) {
		writeError(w, http.StatusBadRequest, "return_to must be a path on this server")
		return
	}
	meta, err := s.oidc.metadata(r.Context())
	if err != nil {
		oidcUnavailable(w, "oidc discovery")
		return
	}
	state, redirect, ok := s.oidc.begin(meta, returnTo)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "too many sign-in attempts, try again later")
		return
	}
	// The cookie ties the attempt to this browser; the rest of it stays on the server.
	s.setCookie(w, oidcStateCookie, oidcCookiePath, state, int(oidcLoginTTL.Seconds()))
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, redirect, http.StatusFound)
}

// oidcUnavailable logs a fixed message (the error can carry request-derived text, so it is not
// logged) and answers 502.
func oidcUnavailable(w http.ResponseWriter, msg string) {
	slog.Error(msg, "request_id", requestIDOf(w))
	writeError(w, http.StatusBadGateway, "identity provider unavailable")
}

// handleOIDCCallback finishes a login: it checks the state, exchanges the code, verifies the
// ID token, finds or creates the user, maps groups and starts a normal session.
func (s *Server) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	q := r.URL.Query()
	cookie, err := r.Cookie(oidcStateCookie)
	state := q.Get("state")
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(state)) != 1 {
		writeError(w, http.StatusBadRequest, errOIDCState.Error())
		return
	}
	s.setCookie(w, oidcStateCookie, oidcCookiePath, "", -1)
	pending, ok := s.oidc.take(state)
	if !ok {
		writeError(w, http.StatusBadRequest, errOIDCState.Error())
		return
	}
	code := q.Get("code")
	if q.Get("error") != "" || code == "" {
		writeError(w, http.StatusUnauthorized, errOIDCDenied.Error())
		return
	}

	meta, err := s.oidc.metadata(r.Context())
	if err != nil {
		oidcUnavailable(w, "oidc discovery")
		return
	}
	raw, err := s.oidc.exchangeCode(r.Context(), meta, code, pending.verifier)
	if err != nil {
		oidcUnavailable(w, "oidc code exchange")
		return
	}
	claims, err := s.oidc.idTokenClaims(r.Context(), meta, raw)
	if err == nil && subtle.ConstantTimeCompare([]byte(claimString(claims, "nonce")), []byte(pending.nonce)) != 1 {
		err = errOIDCNonce
	}
	if err != nil {
		// Only the fixed reason is logged, never the token or anything taken from it.
		slog.Warn("oidc login rejected", "reason", rejectionReason(err), "request_id", requestIDOf(w))
		writeError(w, http.StatusUnauthorized, "the identity provider's response was not accepted")
		return
	}

	id, err := s.oidc.identity(claims)
	if err == nil {
		var uid int64
		if uid, err = s.oidcUser(r.Context(), id); err == nil {
			if err = s.syncOIDCGroups(r.Context(), uid, id.groups); err == nil {
				err = s.startSession(w, r, uid)
			}
		}
	}
	switch {
	case errors.Is(err, errOIDCEmail), errors.Is(err, errOIDCDomain):
		slog.Warn("oidc login refused", "reason", rejectionReason(err), "request_id", requestIDOf(w))
		writeError(w, http.StatusForbidden, err.Error())
		return
	case errors.Is(err, errOIDCEmailTaken):
		slog.Warn("oidc login refused", "reason", rejectionReason(err), "request_id", requestIDOf(w))
		writeError(w, http.StatusConflict, err.Error()+"; it is not linked automatically, ask an administrator")
		return
	case err != nil:
		internalError(w, "oidc login", err)
		return
	}
	if pending.returnTo != "" {
		http.Redirect(w, r, pending.returnTo, http.StatusSeeOther)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "successful login"})
}

// rejectionReason maps an error to a fixed string that is safe to log.
func rejectionReason(err error) string {
	for _, known := range []error{errTokenMalformed, errTokenAlg, errTokenKey, errTokenSignature,
		errOIDCClaims, errOIDCNonce, errOIDCEmail, errOIDCDomain, errOIDCEmailTaken} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return "other"
}

// oidcIdentity is what a verified ID token says about the user.
type oidcIdentity struct {
	issuer, subject, email string
	groups                 []string
}

// identity extracts and checks the identity from verified claims.
func (c *oidcClient) identity(claims map[string]any) (oidcIdentity, error) {
	id := oidcIdentity{issuer: c.cfg.Issuer, subject: claimString(claims, "sub")}
	email := strings.TrimSpace(claimString(claims, c.cfg.EmailClaim))
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || len(email) > maxEmailLen || claims["email_verified"] == false {
		return id, errOIDCEmail
	}
	id.email = email
	if d := c.cfg.AllowedEmailDomain; d != "" && strings.ToLower(email[strings.LastIndex(email, "@")+1:]) != d {
		return id, errOIDCDomain
	}
	if c.cfg.GroupsClaim != "" {
		switch v := claims[c.cfg.GroupsClaim].(type) {
		case string:
			id.groups = []string{v}
		case []any:
			for _, g := range v {
				if name, ok := g.(string); ok && len(id.groups) < oidcMaxGroups {
					id.groups = append(id.groups, name)
				}
			}
		}
	}
	return id, nil
}

func claimString(claims map[string]any, name string) string {
	s, _ := claims[name].(string)
	return s
}

// oidcUser returns the local user for an identity, creating it on first login. Users match
// on issuer plus subject only. An email that already belongs to a different account, local
// or from another identity, is refused: it is never taken over or linked automatically.
func (s *Server) oidcUser(ctx context.Context, id oidcIdentity) (int64, error) {
	lookup := func() (int64, error) {
		var uid int64
		err := s.db.QueryRowContext(ctx,
			"SELECT userid FROM user WHERE oidc_issuer = ? AND oidc_subject = ?", id.issuer, id.subject).Scan(&uid)
		return uid, err
	}
	uid, err := lookup()
	if err == nil {
		return uid, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("lookup oidc user: %w", err)
	}
	// The empty password hash never matches, so the account can't use local login. The
	// case-insensitive email check is part of the INSERT, so two concurrent first logins
	// with case variants of one email can't both succeed.
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO user (email, password, oidc_issuer, oidc_subject)
		 SELECT ?, '', ?, ? WHERE NOT EXISTS (SELECT 1 FROM user WHERE email = ? COLLATE NOCASE)
		 ON CONFLICT DO NOTHING`,
		id.email, id.issuer, id.subject, id.email)
	if err != nil {
		return 0, fmt.Errorf("insert oidc user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// A concurrent first login of the same identity may have won; otherwise the email was.
		if uid, err := lookup(); err == nil {
			return uid, nil
		}
		return 0, errOIDCEmailTaken
	}
	uid, err = res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("oidc user id: %w", err)
	}
	return uid, nil
}

// syncOIDCGroups makes the user's provider-managed memberships match the group names in
// the token. Only existing groups are mapped (matched like group names everywhere: ignoring
// case); unknown names are ignored. Memberships added by hand (oidc = 0) are never removed.
func (s *Server) syncOIDCGroups(ctx context.Context, uid int64, names []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	var want []int64
	for _, name := range names {
		name, ok := validGroupName(name)
		if !ok {
			continue
		}
		var gid int64
		err := tx.QueryRowContext(ctx, "SELECT groupid FROM groups WHERE name_key = ?", groupNameKey(name)).Scan(&gid)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return fmt.Errorf("find group: %w", err)
		}
		want = append(want, gid)
	}
	rows, err := tx.QueryContext(ctx, "SELECT gid FROM user_group WHERE uid = ? AND oidc = 1", uid)
	if err != nil {
		return fmt.Errorf("list memberships: %w", err)
	}
	var have []int64
	for rows.Next() {
		var gid int64
		if err := rows.Scan(&gid); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan membership: %w", err)
		}
		have = append(have, gid)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, gid := range have {
		if slices.Contains(want, gid) {
			continue
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM user_group WHERE uid = ? AND gid = ? AND oidc = 1", uid, gid); err != nil {
			return fmt.Errorf("remove membership: %w", err)
		}
	}
	for _, gid := range want {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO user_group (uid, gid, oidc) VALUES (?, ?, 1) ON CONFLICT DO NOTHING", uid, gid); err != nil {
			return fmt.Errorf("add membership: %w", err)
		}
	}
	return tx.Commit()
}
