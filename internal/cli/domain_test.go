package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDomainCommand(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"site":"blog","domains":["a.corp.example"],"allowed_suffixes":["corp.example"],"limit":5}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/new.corp.example"):
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodPut:
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"that domain is already used by another site"}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	env := func(k string) string {
		return map[string]string{envToken: "opt_x", envServerURL: srv.URL}[k]
	}
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := DomainCommand(t.Context(), args, env, &out)
		return out.String(), err
	}
	if out, err := run("list", "blog"); err != nil || !strings.Contains(out, "a.corp.example") {
		t.Errorf("list = %q, %v", out, err)
	}
	if out, err := run("add", "blog", "New.corp.example."); err != nil || !strings.Contains(out, "added new.corp.example") {
		t.Errorf("add = %q, %v", out, err)
	}
	if _, err := run("add", "blog", "taken.corp.example"); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Errorf("conflict error = %v", err)
	}
	if out, err := run("remove", "blog", "a.corp.example"); err != nil || !strings.Contains(out, "removed") {
		t.Errorf("remove = %q, %v", out, err)
	}
	for _, bad := range [][]string{{"add", "blog"}, {"list"}, {"add", "blog", "nodots"}, {"add", "Bad_Site", "a.corp.example"}, {"frob", "blog"}} {
		before := len(calls)
		if _, err := run(bad...); err == nil || len(calls) != before {
			t.Errorf("%v: err = %v, calls made = %d", bad, err, len(calls)-before)
		}
	}
	if calls[0] != "GET /v1/auth/sites/blog/domains Bearer opt_x" {
		t.Errorf("first call = %q", calls[0])
	}
}
