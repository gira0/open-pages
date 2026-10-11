package main

import (
	"bytes"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/gira0/open-pages/api"
	"github.com/gira0/open-pages/internal/config"
)

// specOperations returns "METHOD /path" for every operation in api/openapi.yaml.
func specOperations(t *testing.T) []string {
	t.Helper()
	var doc struct {
		OpenAPI string                          `yaml:"openapi"`
		Paths   map[string]map[string]yaml.Node `yaml:"paths"`
	}
	if err := yaml.Unmarshal(api.OpenAPI, &doc); err != nil {
		t.Fatalf("parse openapi.yaml: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.1.") {
		t.Fatalf("openapi version = %q, want 3.1.x", doc.OpenAPI)
	}
	methods := []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}
	var ops []string
	for path, item := range doc.Paths {
		for key := range item {
			if slices.Contains(methods, key) {
				ops = append(ops, strings.ToUpper(key)+" "+path)
			}
		}
	}
	return ops
}

// TestOpenAPIMatchesRoutes fails when a route is registered but missing from the spec, or
// documented in the spec but not registered, with every optional route group turned on.
func TestOpenAPIMatchesRoutes(t *testing.T) {
	cfg := config.Default()
	cfg.LocalLogin = true
	cfg.MetricsToken = "m"
	s := &Server{cfg: cfg, oidc: &oidcClient{}, saml: &samlClient{}}

	// OpenAPI has no wildcard segments, so "{path...}" is documented as "{path}".
	var registered []string
	for _, r := range s.routeTable() {
		registered = append(registered, strings.ReplaceAll(r.pattern, "...}", "}"))
	}
	documented := specOperations(t)

	for _, r := range registered {
		if !slices.Contains(documented, r) {
			t.Errorf("route %q is registered but not in api/openapi.yaml", r)
		}
	}
	for _, op := range documented {
		// A GET pattern also serves HEAD, so a documented HEAD needs only the GET.
		want := op
		if path, ok := strings.CutPrefix(op, "HEAD "); ok {
			want = "GET " + path
		}
		if !slices.Contains(registered, want) {
			t.Errorf("api/openapi.yaml documents %q, which the server does not register", op)
		}
	}
}

func TestServeOpenAPI(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/v1/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("Content-Type = %q, want application/yaml", ct)
	}
	if !bytes.Equal(body, api.OpenAPI) {
		t.Error("served document differs from api/openapi.yaml")
	}
}
