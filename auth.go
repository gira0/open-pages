package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

const (
	sessionCookie   = "auth_cookie"
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
	c, err := r.Cookie(sessionCookie)
	if errors.Is(err, http.ErrNoCookie) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var uid int64
	err = s.db.QueryRowContext(r.Context(),
		"SELECT userid FROM session WHERE token = ? AND expires > ?", c.Value, time.Now().Unix()).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return uid, err
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	// Secure is configurable so the service also works over plain HTTP on internal networks.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: see above
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		MaxAge:   maxAge,
		Secure:   s.cfg.CookieSecure,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

// requireAuth rejects requests without a valid session and passes the user id on in the context.
func (s *Server) requireAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uid, err := s.sessionUser(r)
		if err != nil {
			internalError(w, "session lookup", err)
			return
		}
		if uid == 0 {
			if _, err := r.Cookie(sessionCookie); err == nil {
				s.setSessionCookie(w, "", -1)
			}
			writeError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userIDKey, uid)))
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

	token, err := generateToken()
	if err != nil {
		internalError(w, "generate token", err)
		return
	}
	now := time.Now()
	if _, err := s.db.ExecContext(r.Context(), "DELETE FROM session WHERE expires <= ?", now.Unix()); err != nil {
		internalError(w, "prune sessions", err)
		return
	}
	if _, err := s.db.ExecContext(r.Context(),
		"INSERT INTO session (userid, token, expires) VALUES (?, ?, ?)",
		uid, token, now.Add(sessionLifetime).Unix()); err != nil {
		internalError(w, "insert session", err)
		return
	}
	s.setSessionCookie(w, token, int(sessionLifetime.Seconds()))
	writeJSON(w, http.StatusOK, map[string]string{"status": "successful login"})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	c, _ := r.Cookie(sessionCookie)
	if _, err := s.db.ExecContext(r.Context(), "DELETE FROM session WHERE token = ?", c.Value); err != nil {
		internalError(w, "delete session", err)
		return
	}
	s.setSessionCookie(w, "", -1)
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged out"})
}

func (s *Server) handleGetUser(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]int64{"userid": userID(r)})
}
