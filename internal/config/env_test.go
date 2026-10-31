package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeSettings writes a minimal valid settings file and returns its path.
func writeSettings(t *testing.T, extra string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.ini")
	body := "[sites]\napp_host = pages.corp\ncontent_domain = pages-content.corp\n" + extra
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEnvOverridesFile(t *testing.T) {
	path := writeSettings(t, "[server]\nhttp_port = 9090\n[log]\nlevel = info\n")
	t.Setenv("OPEN_PAGES_SERVER_HTTP_PORT", "7070")
	t.Setenv("OPEN_PAGES_LOG_LEVEL", "debug")
	t.Setenv("OPEN_PAGES_LOG_FORMAT", "json")
	t.Setenv("OPEN_PAGES_SITES_KEEP_VERSIONS", "9")
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":7070" {
		t.Errorf("Listen = %q, env must win over the file", cfg.Listen)
	}
	if cfg.LogLevel.String() != "DEBUG" || cfg.LogFormat != "json" {
		t.Errorf("log = %v %q", cfg.LogLevel, cfg.LogFormat)
	}
	if cfg.KeepVersions != 9 {
		t.Errorf("KeepVersions = %d, want a key absent from the file set by env", cfg.KeepVersions)
	}
}

func TestEnvSecretsAndFiles(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "client_secret")
	if err := os.WriteFile(secret, []byte("s3cret value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("0123456789abcdef0123\r\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := writeSettings(t, "[oidc]\nclient_secret = from-file\n")
	t.Setenv("OPEN_PAGES_OIDC_ENABLED", "true")
	t.Setenv("OPEN_PAGES_OIDC_ISSUER", "https://sso.example.com")
	t.Setenv("OPEN_PAGES_OIDC_CLIENT_ID", "open-pages")
	t.Setenv("OPEN_PAGES_OIDC_REDIRECT_URL", "https://pages.corp/v1/auth/oidc/callback")
	t.Setenv("OPEN_PAGES_OIDC_CLIENT_SECRET_FILE", secret)
	t.Setenv("OPEN_PAGES_METRICS_TOKEN_FILE", token)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OIDC.ClientSecret != "s3cret value" {
		t.Errorf("client secret = %q, want the file's content without the newline", cfg.OIDC.ClientSecret)
	}
	if cfg.MetricsToken != "0123456789abcdef0123" {
		t.Errorf("metrics token = %q", cfg.MetricsToken)
	}
}

func TestEnvAndFileBothSetFails(t *testing.T) {
	f := filepath.Join(t.TempDir(), "v")
	if err := os.WriteFile(f, []byte("0123456789abcdef0123"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPEN_PAGES_METRICS_TOKEN", "0123456789abcdef0123")
	t.Setenv("OPEN_PAGES_METRICS_TOKEN_FILE", f)
	_, err := Load(writeSettings(t, ""))
	if err == nil || !strings.Contains(err.Error(), "both set") || !strings.Contains(err.Error(), "OPEN_PAGES_METRICS_TOKEN") {
		t.Fatalf("err = %v, want both-set error", err)
	}
}

func TestEnvFileProblems(t *testing.T) {
	t.Setenv("OPEN_PAGES_METRICS_TOKEN_FILE", filepath.Join(t.TempDir(), "missing"))
	if _, err := Load(writeSettings(t, "")); err == nil || !strings.Contains(err.Error(), "OPEN_PAGES_METRICS_TOKEN_FILE") {
		t.Errorf("missing file: err = %v", err)
	}
	t.Setenv("OPEN_PAGES_METRICS_TOKEN_FILE", "")
	if _, err := Load(writeSettings(t, "")); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("empty path: err = %v", err)
	}
}

// Kubernetes injects OPEN_PAGES_SERVICE_HOST, OPEN_PAGES_PORT and friends for a Service
// named open-pages; they are not settings.
func TestEnvIgnoresUnrelatedVariables(t *testing.T) {
	t.Setenv("OPEN_PAGES_SERVICE_HOST", "10.0.0.1")
	t.Setenv("OPEN_PAGES_SERVICE_PORT", "8080")
	t.Setenv("OPEN_PAGES_PORT", "tcp://10.0.0.1:8080")
	t.Setenv("OPEN_PAGES_PORT_8080_TCP_ADDR", "10.0.0.1")
	t.Setenv("OPEN_PAGES_SERVER", "x")
	t.Setenv("OPEN_PAGES_SERVER_", "x")
	cfg, err := Load(writeSettings(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
}

// A setting whose own name ends in _file is set directly; its file form has _FILE twice.
func TestEnvSettingsNamedFile(t *testing.T) {
	dir := t.TempDir()
	meta := filepath.Join(dir, "idp.xml")
	ptr := filepath.Join(dir, "ptr")
	if err := os.WriteFile(ptr, []byte(meta+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := writeSettings(t, "")
	t.Setenv("OPEN_PAGES_SAML_ENABLED", "true")
	t.Setenv("OPEN_PAGES_SAML_ENTITY_ID", "https://pages.corp/v1/auth/saml/metadata")
	t.Setenv("OPEN_PAGES_SAML_ACS_URL", "https://pages.corp/v1/auth/saml/acs")

	t.Setenv("OPEN_PAGES_SAML_IDP_METADATA_FILE", meta)
	cfg, err := Load(base)
	if err != nil || cfg.SAML.IDPMetadataFile != meta {
		t.Fatalf("direct: %q, %v", cfg.SAML.IDPMetadataFile, err)
	}

	os.Unsetenv("OPEN_PAGES_SAML_IDP_METADATA_FILE")
	t.Setenv("OPEN_PAGES_SAML_IDP_METADATA_FILE_FILE", ptr)
	cfg, err = Load(base)
	if err != nil || cfg.SAML.IDPMetadataFile != meta {
		t.Fatalf("via file: %q, %v", cfg.SAML.IDPMetadataFile, err)
	}
}

func TestLoadWithoutFile(t *testing.T) {
	t.Setenv("OPEN_PAGES_SITES_APP_HOST", "pages.corp")
	t.Setenv("OPEN_PAGES_SITES_CONTENT_DOMAIN", "pages-content.corp")
	cfg, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AppHost != "pages.corp" || cfg.Listen != ":8080" {
		t.Errorf("cfg = %+v", cfg)
	}
}

func TestLoadWithoutFileNeedsHosts(t *testing.T) {
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "app_host") {
		t.Fatalf("err = %v, want the missing app_host", err)
	}
}

func TestShutdownAndDrainSettings(t *testing.T) {
	cfg, err := Load(writeSettings(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ShutdownTimeout != 30*time.Second || cfg.DrainDelay != 0 {
		t.Errorf("defaults = %v, %v", cfg.ShutdownTimeout, cfg.DrainDelay)
	}
	cfg, err = Load(writeSettings(t, "[server]\nshutdown_timeout = 45\ndrain_delay = 5\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ShutdownTimeout != 45*time.Second || cfg.DrainDelay != 5*time.Second {
		t.Errorf("set = %v, %v", cfg.ShutdownTimeout, cfg.DrainDelay)
	}
	for _, bad := range []string{"shutdown_timeout = 0", "shutdown_timeout = x", "drain_delay = -1", "drain_delay = 1.5"} {
		if _, err := Load(writeSettings(t, "[server]\n"+bad+"\n")); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestTrustedProxies(t *testing.T) {
	cfg, err := Load(writeSettings(t, ""))
	if err != nil || len(cfg.TrustedProxies) != 0 {
		t.Fatalf("default: %v, %v", cfg.TrustedProxies, err)
	}
	cfg, err = Load(writeSettings(t, "[server]\ntrusted_proxies = 10.0.0.0/8, 192.0.2.7 ,fd00::/8\n"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range cfg.TrustedProxies {
		got = append(got, p.String())
	}
	if strings.Join(got, " ") != "10.0.0.0/8 192.0.2.7/32 fd00::/8" {
		t.Errorf("TrustedProxies = %v", got)
	}
	if _, err := Load(writeSettings(t, "[server]\ntrusted_proxies = 10.0.0.0/8, nope\n")); err == nil {
		t.Error("expected an error for a bad entry")
	}
}
