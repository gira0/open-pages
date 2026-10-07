package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// bearerStatus sends a request authenticated only by the Authorization header value.
func bearerStatus(t *testing.T, method, url, authorization string, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// mintToken creates a token through the API and returns the response fields.
func mintToken(t *testing.T, ts *httptest.Server, c *http.Client, body string) map[string]any {
	t.Helper()
	status, m := doJSON(t, c, http.MethodPost, ts.URL+"/v1/auth/tokens", body)
	expectStatus(t, status, http.StatusCreated)
	return m
}

func TestTokenLifecycle(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)

	m := mintToken(t, ts, c, `{"name":"ci"}`)
	token, _ := m["token"].(string)
	prefix, _ := m["prefix"].(string)
	if !strings.HasPrefix(token, tokenPrefix) || len(token) != len(tokenPrefix)+64 {
		t.Fatalf("unexpected token shape %q", token)
	}
	if prefix != token[:len(tokenPrefix)+tokenShownChars] || m["expires"] != nil {
		t.Fatalf("prefix %q, expires %v", prefix, m["expires"])
	}

	// Only the hash is stored.
	var stored string
	if err := s.db.QueryRow("SELECT hash FROM api_token").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored == token || stored != hashToken(token) {
		t.Fatalf("stored hash %q is not the SHA-256 of the token", stored)
	}

	// The list never contains the secret.
	var buf bytes.Buffer
	resp, err := c.Get(ts.URL + "/v1/auth/tokens")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !strings.Contains(buf.String(), prefix) || strings.Contains(buf.String(), token) || strings.Contains(buf.String(), stored) {
		t.Fatalf("bad list response: %s", buf.String())
	}

	// A cookie-less client authenticates with the token and gets the same user.
	auth := "Bearer " + token
	expectStatus(t, bearerStatus(t, http.MethodGet, ts.URL+"/v1/auth/user", auth, nil), http.StatusOK)
	expectStatus(t, bearerStatus(t, http.MethodGet, ts.URL+"/v1/auth/user", "bearer "+token, nil), http.StatusOK)
	expectStatus(t, bearerStatus(t, http.MethodPost, ts.URL+"/v1/auth/sites", auth, []byte(`{"name":"blog"}`)), http.StatusCreated)
	expectStatus(t, bearerStatus(t, http.MethodPost, ts.URL+"/v1/auth/sites/blog/upload", auth,
		zipArchive(t, map[string]string{"index.html": "hi"})), http.StatusOK)

	id := strconv.Itoa(int(m["id"].(float64)))
	status, _ := doJSON(t, c, http.MethodDelete, ts.URL+"/v1/auth/tokens/"+id, "")
	expectStatus(t, status, http.StatusOK)
	expectStatus(t, bearerStatus(t, http.MethodGet, ts.URL+"/v1/auth/user", auth, nil), http.StatusUnauthorized)
	status, _ = doJSON(t, c, http.MethodDelete, ts.URL+"/v1/auth/tokens/"+id, "")
	expectStatus(t, status, http.StatusNotFound)
}

func TestTokenAuthFailures(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	good, _ := mintToken(t, ts, c, `{"name":"ci","expires_in_days":1}`)["token"].(string)

	// An expired token is rejected even though its hash matches.
	expired := tokenPrefix + strings.Repeat("a", 64)
	if _, err := s.db.Exec(
		"INSERT INTO api_token (userid, name, prefix, hash, created, expires) VALUES (1, 'old', 'opt_aaaaaaaa', ?, ?, ?)",
		hashToken(expired), time.Now().Add(-48*time.Hour).Unix(), time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}

	for name, header := range map[string]string{
		"expired":      "Bearer " + expired,
		"unknown":      "Bearer " + tokenPrefix + strings.Repeat("b", 64),
		"empty":        "Bearer ",
		"wrong scheme": "Basic " + good,
		"raw token":    good,
		"truncated":    "Bearer " + good[:len(good)-1],
		"hash itself":  "Bearer " + hashToken(good),
	} {
		t.Run(name, func(t *testing.T) {
			expectStatus(t, bearerStatus(t, http.MethodGet, ts.URL+"/v1/auth/user", header, nil), http.StatusUnauthorized)
		})
	}

	// A bad bearer header doesn't fall back to a valid session cookie.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/auth/user", nil)
	req.Header.Set("Authorization", "Bearer nope")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	expectStatus(t, resp.StatusCode, http.StatusUnauthorized)
	expectStatus(t, get(t, c, ts.URL+"/v1/auth/user"), http.StatusOK)
}

func TestTokenCannotManageTokens(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts)
	m := mintToken(t, ts, c, `{"name":"ci"}`)
	auth := "Bearer " + m["token"].(string)
	id := strconv.Itoa(int(m["id"].(float64)))

	expectStatus(t, bearerStatus(t, http.MethodPost, ts.URL+"/v1/auth/tokens", auth, []byte(`{"name":"evil"}`)), http.StatusUnauthorized)
	expectStatus(t, bearerStatus(t, http.MethodGet, ts.URL+"/v1/auth/tokens", auth, nil), http.StatusUnauthorized)
	expectStatus(t, bearerStatus(t, http.MethodDelete, ts.URL+"/v1/auth/tokens/"+id, auth, nil), http.StatusUnauthorized)
	expectStatus(t, bearerStatus(t, http.MethodPost, ts.URL+"/v1/auth/logout", auth, nil), http.StatusUnauthorized)

	// Nothing changed: the token still works and is still the only one.
	expectStatus(t, bearerStatus(t, http.MethodGet, ts.URL+"/v1/auth/user", auth, nil), http.StatusOK)
	status, list := doJSON(t, c, http.MethodGet, ts.URL+"/v1/auth/tokens", "")
	expectStatus(t, status, http.StatusOK)
	if n := len(list["tokens"].([]any)); n != 1 {
		t.Fatalf("got %d tokens, want 1", n)
	}

	// Without any credentials the management endpoints are closed too.
	expectStatus(t, post(t, newClient(t), ts.URL+"/v1/auth/tokens", "application/json", []byte(`{"name":"x"}`)), http.StatusUnauthorized)
}

func TestTokensArePerUser(t *testing.T) {
	ts, _ := newTestServer(t)
	alice := loggedInClient(t, ts)
	bob := secondUser(t, ts)

	m := mintToken(t, ts, alice, `{"name":"alice ci"}`)
	id := strconv.Itoa(int(m["id"].(float64)))

	status, list := doJSON(t, bob, http.MethodGet, ts.URL+"/v1/auth/tokens", "")
	expectStatus(t, status, http.StatusOK)
	if n := len(list["tokens"].([]any)); n != 0 {
		t.Fatalf("bob sees %d of alice's tokens", n)
	}
	status, _ = doJSON(t, bob, http.MethodDelete, ts.URL+"/v1/auth/tokens/"+id, "")
	expectStatus(t, status, http.StatusNotFound)
	expectStatus(t, bearerStatus(t, http.MethodGet, ts.URL+"/v1/auth/user", "Bearer "+m["token"].(string), nil), http.StatusOK)

	// Alice's token acts as alice: it can't touch bob's site.
	createSiteVia(t, ts, bob, "bobs")
	expectStatus(t, bearerStatus(t, http.MethodPost, ts.URL+"/v1/auth/sites/bobs/upload", "Bearer "+m["token"].(string),
		zipArchive(t, map[string]string{"index.html": "x"})), http.StatusForbidden)
}

func TestTokenCreateValidation(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts)
	for _, body := range []string{
		`{}`,
		`{"name":"  "}`,
		`{"name":"` + strings.Repeat("x", maxTokenNameLen+1) + `"}`,
		`{"name":"ci","expires_in_days":-1}`,
		`{"name":"ci","expires_in_days":99999}`,
		`not json`,
	} {
		status, _ := doJSON(t, c, http.MethodPost, ts.URL+"/v1/auth/tokens", body)
		expectStatus(t, status, http.StatusBadRequest)
	}
	for _, id := range []string{"abc", "99999"} {
		status, _ := doJSON(t, c, http.MethodDelete, ts.URL+"/v1/auth/tokens/"+id, "")
		expectStatus(t, status, http.StatusNotFound)
	}
}

func TestTokenLimit(t *testing.T) {
	ts, s := newTestServer(t)
	c := loggedInClient(t, ts)
	for i := range maxTokensPerUser {
		mintToken(t, ts, c, `{"name":"t`+strconv.Itoa(i)+`"}`)
	}
	status, _ := doJSON(t, c, http.MethodPost, ts.URL+"/v1/auth/tokens", `{"name":"one too many"}`)
	expectStatus(t, status, http.StatusConflict)

	// Expired tokens are pruned and free up room.
	if _, err := s.db.Exec("UPDATE api_token SET expires = ? WHERE tokenid = 1", time.Now().Add(-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	mintToken(t, ts, c, `{"name":"fits now"}`)
}

func TestBareBearerSchemeNeverFallsBackToCookie(t *testing.T) {
	ts, _ := newTestServer(t)
	c := loggedInClient(t, ts) // holds a valid session cookie
	expectStatus(t, get(t, c, ts.URL+"/v1/auth/user"), http.StatusOK)

	for _, header := range []string{"Bearer", "Bearer ", "bearer  ", "BEARER\t"} {
		req, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/auth/user", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", header)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		expectStatus(t, resp.StatusCode, http.StatusUnauthorized)
	}
	// The rejected requests didn't log the session out.
	expectStatus(t, get(t, c, ts.URL+"/v1/auth/user"), http.StatusOK)
}
