package main

import (
	"crypto/subtle"
	"database/sql"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/gira0/open-pages/internal/names"
)

// Browser login for non-public sites. The app session cookie is host-only on app_host and
// never reaches a content host, so a site is opened through a handoff, like GitHub private
// Pages:
//
//  1. A GET without credentials to a site is redirected to
//     app_host/v1/auth/site-login?site=..&return=..&state=.. and gets a short-lived state
//     cookie on the site host.
//  2. The app host checks the app session and canView, stores a one-time code (hashed,
//     bound to site, user, app session, return path and state) and redirects to
//     <site>.<content_domain>/__open-pages/auth?code=..
//  3. The site host redeems the code, which needs the state cookie, and sets a per-site
//     cookie holding a random token unrelated to the app session. Only its hash is stored.
//
// Every request to the site still goes through canView, so access changes take effect at once.

const (
	siteSessionCookie  = "site_session"     // see cookieName for the name actually used
	siteStateCookie    = "site_login_state" // login CSRF nonce, see handleSiteLogin
	siteSessionTTL     = time.Hour
	siteLoginCodeTTL   = time.Minute
	siteLoginStateTTL  = 5 * time.Minute
	siteLoginPath      = "/v1/auth/site-login"
	reservedPrefix     = "/__open-pages"
	siteRedeemPath     = reservedPrefix + "/auth"
	maxSiteReturnLen   = 2048
	siteLoginStateSize = 64 // hex characters of generateToken
)

// reservedPath reports whether a request path falls under the prefix reserved for open-pages
// on site hosts, however it is spelled (dot segments are resolved as file lookup would).
func reservedPath(p string) bool {
	c := path.Clean("/" + p)
	return c == reservedPrefix || strings.HasPrefix(c, reservedPrefix+"/")
}

// serveReserved answers the paths under /__open-pages/ before any file lookup, so a site's
// own files can't shadow them. Only GET /__open-pages/auth exists.
func (s *Server) serveReserved(w http.ResponseWriter, r *http.Request, site string) {
	if r.URL.Path == siteRedeemPath && r.Method == http.MethodGet {
		s.handleSiteRedeem(w, r, site)
		return
	}
	notFoundNoStore(w, r)
}

// siteReturnPath returns p if it is a relative path on the site that a redirect may safely
// target, otherwise "/".
func siteReturnPath(p string) string {
	if len(p) == 0 || len(p) > maxSiteReturnLen || p[0] != '/' {
		return "/"
	}
	if len(p) > 1 && (p[1] == '/' || p[1] == '\\') { // "//host" and "/\host" are scheme-relative to browsers
		return "/"
	}
	if strings.ContainsRune(p, '\\') || reservedPath(strings.SplitN(p, "?", 2)[0]) {
		return "/"
	}
	for i := 0; i < len(p); i++ {
		if p[i] <= ' ' || p[i] == 0x7f {
			return "/"
		}
	}
	return p
}

// originFor builds scheme://host for the redirect between app and site hosts. Both are
// served by the same listener, so the port the browser used for this request is reused.
func (s *Server) originFor(host string, r *http.Request) string {
	scheme := "http"
	if s.cfg.CookieSecure {
		scheme = "https"
	}
	if _, port, err := net.SplitHostPort(r.Host); err == nil {
		host = net.JoinHostPort(host, port)
	}
	return scheme + "://" + host
}

// noStoreRedirect sends a redirect that must not be cached or leak its URL as a referrer.
func noStoreRedirect(w http.ResponseWriter, location string) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Location", location)
	w.WriteHeader(http.StatusFound)
}

// siteUser returns the user behind the request on the content host of site: an API token if
// the request carries a bearer header (never falling back to a cookie), otherwise the site
// cookie. The app session cookie is not looked at. It returns 0 for no valid credentials.
func (s *Server) siteUser(r *http.Request, site string) (int64, error) {
	if token, ok := bearerToken(r); ok {
		return s.tokenUser(r.Context(), token)
	}
	if len(r.Header.Values("Authorization")) > 0 {
		return 0, nil // an attempt with another scheme is rejected, not answered by a cookie
	}
	// Without cookie_secure a sibling site can plant a cookie of the same name that comes
	// first, so every one is tried.
	for _, c := range r.CookiesNamed(s.cookieName(siteSessionCookie)) {
		var uid int64
		err := s.db.QueryRowContext(r.Context(),
			`SELECT ss.userid FROM site_session ss JOIN session ON session.sessionid = ss.sessionid
			 WHERE ss.hash = ? AND ss.site = ? AND ss.siteid = (SELECT docid FROM docs WHERE name = ?)
			   AND ss.expires > ? AND session.expires > ?`,
			hashToken(c.Value), site, site, time.Now().Unix(), time.Now().Unix()).Scan(&uid)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		return uid, err
	}
	return 0, nil
}

// redirectToLogin sends a GET or HEAD without credentials to the app host and sets the
// state cookie that the later redeem must present.
func (s *Server) redirectToLogin(w http.ResponseWriter, r *http.Request, site string) {
	state, err := generateToken()
	if err != nil {
		internalError(w, "generate state", err)
		return
	}
	s.setCookie(w, siteStateCookie, "/", state, int(siteLoginStateTTL.Seconds()))
	q := url.Values{"site": {site}, "return": {siteReturnPath(r.URL.RequestURI())}, "state": {state}}
	noStoreRedirect(w, s.originFor(s.cfg.AppHost, r)+siteLoginPath+"?"+q.Encode())
}

// handleSiteLogin runs on the app host. It issues a login code for the signed-in user and
// sends the browser on to the site, or to the UI to sign in first.
func (s *Server) handleSiteLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store") // whatever the outcome
	q := r.URL.Query()
	site, state, ret := q.Get("site"), q.Get("state"), siteReturnPath(q.Get("return"))
	if !names.ValidSite(site) {
		notFoundNoStore(w, r)
		return
	}
	if !validState(state) {
		w.Header().Set("Cache-Control", "no-store")
		writeError(w, http.StatusBadRequest, "invalid state")
		return
	}
	uid, sid, sessionEnd, err := s.sessionRow(r)
	if err != nil {
		internalError(w, "session lookup", err)
		return
	}
	if uid == 0 {
		// Come back through the UI: its script follows next after signing in.
		back := siteLoginPath + "?" + url.Values{"site": {site}, "return": {ret}, "state": {state}}.Encode()
		noStoreRedirect(w, "/index?next="+url.QueryEscape(back))
		return
	}
	rec, err := s.getSite(r.Context(), site)
	ok := false
	if err == nil {
		ok, err = s.canView(r.Context(), rec, uid)
	}
	if err != nil && !errors.Is(err, errSiteNotFound) {
		internalError(w, "check site access", err)
		return
	}
	if !ok {
		notFoundNoStore(w, r)
		return
	}
	code, err := generateToken()
	if err != nil {
		internalError(w, "generate code", err)
		return
	}
	now := time.Now().Unix()
	if _, err := s.db.ExecContext(r.Context(), "DELETE FROM site_login_code WHERE expires <= ?", now); err != nil {
		internalError(w, "prune site login codes", err)
		return
	}
	if _, err := s.db.ExecContext(r.Context(), "DELETE FROM site_session WHERE expires <= ?", now); err != nil {
		internalError(w, "prune site sessions", err)
		return
	}
	if _, err := s.db.ExecContext(r.Context(),
		`INSERT INTO site_login_code (hash, site, siteid, userid, sessionid, state_hash, return_to, expires)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		hashToken(code), site, rec.ID, uid, sid, hashToken(state), ret,
		min(now+int64(siteLoginCodeTTL.Seconds()), sessionEnd)); err != nil {
		internalError(w, "store login code", err)
		return
	}
	noStoreRedirect(w, s.originFor(site+"."+s.cfg.ContentDomain, r)+siteRedeemPath+"?"+url.Values{"code": {code}}.Encode())
}

func validState(s string) bool {
	if len(s) != siteLoginStateSize {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// handleSiteRedeem runs on the site host. It consumes the code (once, whatever the outcome),
// checks it belongs to this site and to the browser that started the login, and starts the
// site session.
func (s *Server) handleSiteRedeem(w http.ResponseWriter, r *http.Request, site string) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	states := r.CookiesNamed(s.cookieName(siteStateCookie))
	var (
		codeSite, stateHash, returnTo string
		siteID, uid, sid, expires     int64
	)
	err := s.db.QueryRowContext(r.Context(),
		`DELETE FROM site_login_code WHERE hash = ?
		 RETURNING site, siteid, userid, sessionid, state_hash, return_to, expires`,
		hashToken(r.URL.Query().Get("code"))).Scan(&codeSite, &siteID, &uid, &sid, &stateHash, &returnTo, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		notFoundNoStore(w, r)
		return
	}
	if err != nil {
		siteInternalError(w, "redeem login code", err)
		return
	}
	now := time.Now()
	if !slices.ContainsFunc(states, func(c *http.Cookie) bool {
		return subtle.ConstantTimeCompare([]byte(hashToken(c.Value)), []byte(stateHash)) == 1
	}) {
		// Not this browser's login (a stale or foreign code). The state cookie stays, so it
		// can't cancel a handoff that is still in progress, for example in another tab.
		notFoundNoStore(w, r)
		return
	}
	s.setCookie(w, siteStateCookie, "/", "", -1) // one attempt per nonce
	if expires <= now.Unix() || codeSite != site {
		notFoundNoStore(w, r)
		return
	}
	// The site must still be the one the code was issued for (not deleted, not recreated
	// under the same name).
	var current int64
	err = s.db.QueryRowContext(r.Context(), "SELECT docid FROM docs WHERE name = ?", site).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && current != siteID) {
		notFoundNoStore(w, r)
		return
	}
	if err != nil {
		siteInternalError(w, "look up site", err)
		return
	}
	// The site session must not outlive the app session it came from.
	var sessionEnd int64
	err = s.db.QueryRowContext(r.Context(), "SELECT expires FROM session WHERE sessionid = ?", sid).Scan(&sessionEnd)
	if errors.Is(err, sql.ErrNoRows) || sessionEnd <= now.Unix() {
		notFoundNoStore(w, r)
		return
	}
	if err != nil {
		siteInternalError(w, "look up session", err)
		return
	}
	token, err := generateToken()
	if err != nil {
		siteInternalError(w, "generate site token", err)
		return
	}
	end := min(now.Add(siteSessionTTL).Unix(), sessionEnd)
	if _, err := s.db.ExecContext(r.Context(),
		"INSERT INTO site_session (hash, site, siteid, userid, sessionid, expires) VALUES (?, ?, ?, ?, ?, ?)",
		hashToken(token), site, siteID, uid, sid, end); err != nil {
		siteInternalError(w, "store site session", err)
		return
	}
	s.setCookie(w, siteSessionCookie, "/", token, int(end-now.Unix()))
	h.Set("Location", siteReturnPath(returnTo))
	w.WriteHeader(http.StatusFound)
}

// siteInternalError is internalError for the content host, where the body is plain text like
// the rest of what a site host answers.
func siteInternalError(w http.ResponseWriter, msg string, err error) {
	slog.Error(msg, "err", err, "request_id", requestIDOf(w))
	http.Error(w, "internal error", http.StatusInternalServerError)
}
