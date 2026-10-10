package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.ini")
	ini := `
[paths]
datapath = /srv/pages

[server]
host = 127.0.0.1
http_port = 9090
cookie_secure = true
cors_origins = https://a.example, https://b.example

[limits]
max_upload_mb = 5
`
	if err := os.WriteFile(path, []byte(ini), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != "127.0.0.1:9090" {
		t.Errorf("Listen = %q", cfg.Listen)
	}
	if !cfg.CookieSecure {
		t.Error("CookieSecure = false")
	}
	if want := []string{"https://a.example", "https://b.example"}; !slices.Equal(cfg.CORSOrigins, want) {
		t.Errorf("CORSOrigins = %v, want %v", cfg.CORSOrigins, want)
	}
	if cfg.DataPath != "/srv/pages" {
		t.Errorf("DataPath = %q", cfg.DataPath)
	}
	if cfg.MaxUploadBytes != 5<<20 {
		t.Errorf("MaxUploadBytes = %d", cfg.MaxUploadBytes)
	}
	if cfg.MaxExtractFile != defaultConfig().MaxExtractFile {
		t.Errorf("MaxExtractFile = %d, want default", cfg.MaxExtractFile)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := loadConfig(filepath.Join(t.TempDir(), "nope.ini")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

// The shipped settings.ini must stay loadable.
func TestLoadShippedConfig(t *testing.T) {
	if _, err := loadConfig("../../settings.ini"); err != nil {
		t.Fatal(err)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.ini")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOIDCDefaultsToDisabled(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, "[server]\nhttp_port = 8080\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OIDC.Enabled || !cfg.LocalLogin {
		t.Fatalf("OIDC.Enabled = %v, LocalLogin = %v; want disabled and enabled", cfg.OIDC.Enabled, cfg.LocalLogin)
	}
}

func TestLoadOIDCConfig(t *testing.T) {
	const ini = `
[oidc]
enabled = true
issuer = https://sso.example/realms/corp
client_id = open-pages
client_secret = from-file
redirect_url = https://pages.example/v1/auth/oidc/callback
scopes = profile, email
groups_claim = roles
allowed_email_domain = @Corp.Example
`
	path := writeConfig(t, ini)
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	o := cfg.OIDC
	if !o.Enabled || o.ClientSecret != "from-file" || o.GroupsClaim != "roles" || o.EmailClaim != "email" ||
		o.AllowedEmailDomain != "corp.example" || !slices.Equal(o.Scopes, []string{"openid", "profile", "email"}) {
		t.Fatalf("unexpected OIDC config: %+v", o)
	}
	t.Setenv("OPEN_PAGES_OIDC_CLIENT_SECRET", "from-env")
	if cfg, err = loadConfig(path); err != nil || cfg.OIDC.ClientSecret != "from-file" {
		t.Fatalf("the environment must not override the file: %q, %v", cfg.OIDC.ClientSecret, err)
	}
}

func TestLoadOIDCConfigErrors(t *testing.T) {
	cases := map[string]string{
		"missing secret":   "[oidc]\nenabled = true\nissuer = https://sso.example\nclient_id = x\nredirect_url = https://p.example/cb\n",
		"missing issuer":   "[oidc]\nenabled = true\nclient_id = x\nclient_secret = y\nredirect_url = https://p.example/cb\n",
		"plain http":       "[oidc]\nenabled = true\nissuer = http://sso.example\nclient_id = x\nclient_secret = y\nredirect_url = https://p.example/cb\n",
		"relative url":     "[oidc]\nenabled = true\nissuer = https://sso.example\nclient_id = x\nclient_secret = y\nredirect_url = /cb\n",
		"nobody can login": "[auth]\nlocal_login = false\n",
		"typo local_login": "[auth]\nlocal_login = flase\n",
		"typo enabled":     "[oidc]\nenabled = treu\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := loadConfig(writeConfig(t, body)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	// A disabled section is ignored even when incomplete.
	if _, err := loadConfig(writeConfig(t, "[oidc]\nenabled = false\nissuer = junk\n")); err != nil {
		t.Fatal(err)
	}
}
