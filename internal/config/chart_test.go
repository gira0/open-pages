package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestChartSettings loads the settings.ini that the Helm chart renders, so a renamed key or a
// drifted value format breaks CI instead of the pod's startup. The helm CI job sets
// OPEN_PAGES_CHART_SETTINGS to the rendered file; without it the test is skipped.
//
// The chart delivers secrets as *_FILE variables pointing into a mounted Secret, so this sets
// the same variables to dummy files.
func TestChartSettings(t *testing.T) {
	path := os.Getenv("OPEN_PAGES_CHART_SETTINGS")
	if path == "" {
		t.Skip("OPEN_PAGES_CHART_SETTINGS is not set: no rendered chart settings to load")
	}
	dir := t.TempDir()
	dummy := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	t.Setenv("OPEN_PAGES_OIDC_CLIENT_SECRET_FILE", dummy("oidc-client-secret", "not-a-real-secret\n"))
	t.Setenv("OPEN_PAGES_METRICS_TOKEN_FILE", dummy("metrics-token", "ci-token-0123456789\n"))
	t.Setenv("OPEN_PAGES_SAML_KEY_FILE", dummy("saml.key", "placeholder\n"))
	t.Setenv("OPEN_PAGES_SAML_CERT_FILE", dummy("saml.crt", "placeholder\n"))

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("the chart's settings.ini does not load: %v", err)
	}
	if cfg.AppHost == "" || cfg.ContentDomain == "" {
		t.Errorf("app_host %q and content_domain %q must both come through", cfg.AppHost, cfg.ContentDomain)
	}
	if cfg.DataPath != "/data" {
		t.Errorf("DataPath = %q, want /data", cfg.DataPath)
	}
}
