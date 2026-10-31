package server

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie   = "auth_cookie" // see cookieName for the name actually used
	sessionLifetime = 7 * 24 * time.Hour
	minPasswordLen  = 8
	maxPasswordLen  = 72 // bcrypt ignores anything past 72 bytes
)

type ctxKey int

const userIDKey ctxKey = 0

type credentials struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (c credentials) validate() error {
	addr, err := mail.ParseAddress(c.Email)
	if err != nil || addr.Address != c.Email {
		return errors.New("invalid email")
	}
	if len(c.Password) < minPasswordLen || len(c.Password) > maxPasswordLen {
		return errors.New("password must be 8 to 72 characters")
	}
	return nil
}

// readCredentials accepts either a JSON body or an HTML form.
func readCredentials(r *http.Request) (credentials, error) {
	var c credentials
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<16)
	ct, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if ct == "application/json" {
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			return c, errors.New("invalid JSON body")
		}
	} else {
		if err := r.ParseForm(); err != nil {
			return c, errors.New("invalid form body")
		}
		c.Email, c.Password = r.PostForm.Get("email"), r.PostForm.Get("password")
	}
	c.Email = strings.TrimSpace(c.Email)
	return c, c.validate()
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// sessionUser returns the user id for a valid session cookie, or 0 if there is none.
func (s *Server) sessionUser(r *http.Request) (int64, error) {
	uid, _, _, err := s.sessionRow(r)
	return uid, err
}

// sessionRow returns the user id, session id and expiry (Unix time) of a valid session
// cookie, or zeros if there is none.
func (s *Server) sessionRow(r *http.Request) (uid, sid, expires int64, err error) {
	c, err := r.Cookie(s.cookieName(sessionCookie))
	if errors.Is(err, http.ErrNoCookie) {
		return 0, 0, 0, nil
	}
	if err != nil {
		return 0, 0, 0, err
	}
	err = s.db.QueryRowContext(r.Context(),
		"SELECT userid, sessionid, expires FROM session WHERE token = ? AND expires > ?",
		c.Value, time.Now().Unix()).Scan(&uid, &sid, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, nil
	}
	return uid, sid, expires, err
}

// cookieName returns the name a cookie is set and read under. With cookie_secure the
// browser-enforced __Host- prefix is added: such a cookie must be Secure, have Path=/ and
// no Domain, so a page on a sibling host cannot set or shadow it (cookie tossing).
func (s *Server) cookieName(base string) string {
	if s.cfg.CookieSecure {
		return "__Host-" + base
	}
	return base
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	s.setCookie(w, sessionCookie, "/", token, maxAge)
}

// setCookie sets a cookie under cookieName(base). With cookie_secure the path must be "/"
// for the __Host- prefix to be accepted, so callers' paths are overridden then.
func (s *Server) setCookie(w http.ResponseWriter, base, path, value string, maxAge int) {
	name := s.cookieName(base)
	if s.cfg.CookieSecure {
		path = "/"
	}
	// Secure is configurable so the service also works over plain HTTP on internal networks.
	s.setCookieAttrs(w, name, path, value, maxAge, s.cfg.CookieSecure, http.SameSiteLaxMode)
}

// setCookieAttrs sets an HttpOnly cookie with explicit Secure and SameSite attributes.
func (s *Server) setCookieAttrs(w http.ResponseWriter, name, path, value string, maxAge int, secure bool, sameSite http.SameSite) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure follows configuration, see setCookie
		Name:     name,
		Value:    value,
		Path:     path,
		MaxAge:   maxAge,
		Secure:   secure,
		HttpOnly: true,
		SameSite: sameSite,
	})
}

// requireAuth rejects requests without a valid session or API token and passes the user
// id on in the context.
func (s *Server) requireAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, err := s.authUser(r)
		if err != nil {
			internalError(w, "auth lookup", err)
			return
		}
		if uid == 0 {
			// A rejected API token says nothing about the session cookie.
			if _, bearer := bearerToken(r); !bearer {
				if _, err := r.Cookie(s.cookieName(sessionCookie)); err == nil {
					s.setSessionCookie(w, "", -1)
				}
			}
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		next(w, r.WithContext(context.WithValue(s.withAuditSource(r, viaOf(r)), userIDKey, uid)))
	})
}

func userID(r *http.Request) int64 {
	return r.Context().Value(userIDKey).(int64)
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	c, err := readCredentials(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(c.Password), bcrypt.DefaultCost)
	if err != nil {
		internalError(w, "hash password", err)
		return
	}
	res, err := s.db.ExecContext(r.Context(),
		"INSERT INTO user (email, password) VALUES (?, ?) ON CONFLICT (email) DO NOTHING", c.Email, string(hash))
	if err != nil {
		internalError(w, "insert user", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusConflict, "user already exists")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"status": "user created"})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	c, err := readCredentials(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	var uid int64
	var hash string
	err = s.db.QueryRowContext(r.Context(),
		"SELECT userid, password FROM user WHERE email = ?", c.Email).Scan(&uid, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}
	if err != nil {
		internalError(w, "lookup user", err)
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(hash), []byte(c.Password)); err != nil {
		writeError(w, http.StatusUnauthorized, "invalid email or password")
		return
	}

	if err := s.startSession(w, r, uid); err != nil {
		internalError(w, "start session", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "successful login"})
}

// startSession prunes expired sessions, stores a new one for uid and sets the cookie.
// It is the shared tail of local and OIDC login.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, uid int64) error {
	token, err := generateToken()
	if err != nil {
		return fmt.Errorf("generate token: %w", err)
	}
	now := time.Now()
	if _, err := s.db.ExecContext(r.Context(), "DELETE FROM session WHERE expires <= ?", now.Unix()); err != nil {
		return fmt.Errorf("prune sessions: %w", err)
	}
	if _, err := s.db.ExecContext(r.Context(),
		"INSERT INTO session (userid, token, expires) VALUES (?, ?, ?)",
		uid, token, now.Add(sessionLifetime).Unix()); err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	s.setSessionCookie(w, token, int(sessionLifetime.Seconds()))
	return nil
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie(s.cookieName(sessionCookie))
	// Site sessions and unredeemed login codes hang off the app session and die with it.
	for _, q := range []string{
		"DELETE FROM site_session WHERE sessionid IN (SELECT sessionid FROM session WHERE token = ?)",
		"DELETE FROM site_login_code WHERE sessionid IN (SELECT sessionid FROM session WHERE token = ?)",
		"DELETE FROM session WHERE token = ?",
	} {
		if _, err := s.db.ExecContext(r.Context(), q, c.Value); err != nil {
			internalError(w, "delete session", err)
			return
		}
	}
	s.setSessionCookie(w, "", -1)
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}
