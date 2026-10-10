package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// API tokens let CI pipelines call the API without a browser session. A token is 32 random
// bytes, so a plain SHA-256 is enough to store it (there is nothing to brute-force). Only the
// hash and a short display prefix are kept; the token itself is shown once, at creation.
// A token authenticates as the user who created it, with that user's full rights, except
// that it can't manage tokens or log out: those need a real session.

const (
	tokenPrefix      = "opt_"
	tokenShownChars  = 8 // random characters kept after tokenPrefix for display
	maxTokensPerUser = 50
	maxTokenNameLen  = 64
	maxTokenDays     = 3650
)

// hashToken returns the hex SHA-256 of an API token.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// bearerToken extracts the token of an "Authorization: Bearer <token>" header.
func bearerToken(r *http.Request) (string, bool) {
	// The scheme alone marks token auth, even with no credential after it ("Bearer", "Bearer "),
	// so such a request is rejected rather than falling back to the session cookie.
	scheme, rest, _ := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// authUser returns the user id behind the request's API token if it carries one, otherwise
// the one behind its session cookie. It returns 0 when the credentials are missing, unknown,
// expired or revoked. A request with a bearer header never falls back to the cookie.
func (s *Server) authUser(r *http.Request) (int64, error) {
	if token, ok := bearerToken(r); ok {
		return s.tokenUser(r.Context(), token)
	}
	return s.sessionUser(r)
}

// tokenUser returns the user id owning the live API token, or 0 if there is none.
func (s *Server) tokenUser(ctx context.Context, token string) (int64, error) {
	want := hashToken(token)
	var uid int64
	var stored string
	err := s.db.QueryRowContext(ctx,
		"SELECT userid, hash FROM api_token WHERE hash = ? AND (expires IS NULL OR expires > ?)",
		want, time.Now().Unix()).Scan(&uid, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("lookup token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(want)) != 1 {
		return 0, nil
	}
	return uid, nil
}

// requireSession is like requireAuth but accepts only a session cookie, never an API token.
func (s *Server) requireSession(next http.HandlerFunc) http.Handler {
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
			writeError(w, http.StatusUnauthorized, "a login session is required")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userIDKey, uid)))
	})
}

type tokenCreate struct {
	Name          string `json:"name"`
	ExpiresInDays int    `json:"expires_in_days"` // 0 or absent: never expires
}

type tokenInfo struct {
	ID      int64   `json:"id"`
	Name    string  `json:"name"`
	Prefix  string  `json:"prefix"`
	Created string  `json:"created"`
	Expires *string `json:"expires"` // null when the token never expires
}

type tokenCreated struct {
	tokenInfo
	Token string `json:"token"` // only ever returned by the create call
}

func formatUnix(sec int64) string { return time.Unix(sec, 0).UTC().Format(time.RFC3339) }

func (s *Server) handleTokenCreate(w http.ResponseWriter, r *http.Request) {
	var d tokenCreate
	r.Body = http.MaxBytesReader(w, r.Body, 1<<16)
	if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	d.Name = strings.TrimSpace(d.Name)
	if d.Name == "" || len(d.Name) > maxTokenNameLen {
		writeError(w, http.StatusBadRequest, "name is required and must be at most "+strconv.Itoa(maxTokenNameLen)+" characters")
		return
	}
	if d.ExpiresInDays < 0 || d.ExpiresInDays > maxTokenDays {
		writeError(w, http.StatusBadRequest, "expires_in_days must be between 0 (never) and "+strconv.Itoa(maxTokenDays))
		return
	}

	random, err := generateToken()
	if err != nil {
		internalError(w, "generate api token", err)
		return
	}
	token := tokenPrefix + random
	prefix := token[:len(tokenPrefix)+tokenShownChars]

	now := time.Now()
	var expires any
	info := tokenInfo{Name: d.Name, Prefix: prefix, Created: formatUnix(now.Unix())}
	if d.ExpiresInDays > 0 {
		exp := now.AddDate(0, 0, d.ExpiresInDays).Unix()
		expires = exp
		e := formatUnix(exp)
		info.Expires = &e
	}

	uid := userID(r)
	// Expired tokens don't count against the limit.
	if _, err := s.db.ExecContext(r.Context(),
		"DELETE FROM api_token WHERE userid = ? AND expires IS NOT NULL AND expires <= ?", uid, now.Unix()); err != nil {
		internalError(w, "prune api tokens", err)
		return
	}
	// The limit check and the insert are one statement, so concurrent creates can't overshoot it.
	res, err := s.db.ExecContext(r.Context(),
		`INSERT INTO api_token (userid, name, prefix, hash, created, expires)
		 SELECT ?, ?, ?, ?, ?, ? WHERE (SELECT COUNT(*) FROM api_token WHERE userid = ?) < ?`,
		uid, d.Name, prefix, hashToken(token), now.Unix(), expires, uid, maxTokensPerUser)
	if err != nil {
		internalError(w, "insert api token", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusConflict, "token limit reached; revoke one first")
		return
	}
	if info.ID, err = res.LastInsertId(); err != nil {
		internalError(w, "api token id", err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusCreated, tokenCreated{tokenInfo: info, Token: token})
}

func (s *Server) handleTokenList(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(),
		"SELECT tokenid, name, prefix, created, expires FROM api_token WHERE userid = ? ORDER BY tokenid", userID(r))
	if err != nil {
		internalError(w, "list api tokens", err)
		return
	}
	defer rows.Close()

	tokens := []tokenInfo{}
	for rows.Next() {
		var t tokenInfo
		var created int64
		var expires sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &created, &expires); err != nil {
			internalError(w, "scan api token", err)
			return
		}
		t.Created = formatUnix(created)
		if expires.Valid {
			e := formatUnix(expires.Int64)
			t.Expires = &e
		}
		tokens = append(tokens, t)
	}
	if err := rows.Err(); err != nil {
		internalError(w, "list api tokens", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, "token not found")
		return
	}
	// Scoping the delete to the caller makes other users' tokens indistinguishable from missing ones.
	res, err := s.db.ExecContext(r.Context(), "DELETE FROM api_token WHERE tokenid = ? AND userid = ?", id, userID(r))
	if err != nil {
		internalError(w, "revoke api token", err)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeError(w, http.StatusNotFound, "token not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "token revoked"})
}
