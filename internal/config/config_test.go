package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gira0/open-pages/internal/logging"
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
max_concurrent_deploys = 4
deploy_timeout_seconds = 90
`
	if err := os.WriteFile(path, []byte(ini), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
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
	if cfg.MaxDeploys != 4 || cfg.DeployTimeout != 90*time.Second {
		t.Errorf("MaxDeploys = %d, DeployTimeout = %v", cfg.MaxDeploys, cfg.DeployTimeout)
	}
	if cfg.MaxExtractFile != Default().MaxExtractFile {
		t.Errorf("MaxExtractFile = %d, want default", cfg.MaxExtractFile)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.ini")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

// The shipped settings.ini must stay loadable.
func TestLoadShippedConfig(t *testing.T) {
	if _, err := Load("../../configs/settings.ini"); err != nil {
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
	cfg, err := Load(writeConfig(t, "[server]\nhttp_port = 8080\n"))
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
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	o := cfg.OIDC
	if !o.Enabled || o.ClientSecret != "from-file" || o.GroupsClaim != "roles" || o.EmailClaim != "email" ||
		o.AllowedEmailDomain != "corp.example" || !slices.Equal(o.Scopes, []string{"openid", "profile", "email"}) {
		t.Fatalf("unexpected OIDC config: %+v", o)
	}
	t.Setenv("OPEN_PAGES_OIDC_CLIENT_SECRET", "from-env")
	if cfg, err = Load(path); err != nil || cfg.OIDC.ClientSecret != "from-file" {
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
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	// A disabled section is ignored even when incomplete.
	if _, err := Load(writeConfig(t, "[oidc]\nenabled = false\nissuer = junk\n")); err != nil {
		t.Fatal(err)
	}
}

func TestCheckURL(t *testing.T) {
	for _, ok := range []string{"https://sso.example/realms/corp", "http://localhost:8080/cb", "http://127.0.0.1/cb", "http://[::1]:9/cb"} {
		if err := CheckURL("k", ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "/cb", "http://sso.example", "ftp://sso.example", "https://sso.example/#frag", "https://", "https://:443/acs"} {
		if err := CheckURL("k", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestLoadLogAndSites(t *testing.T) {
	cfg, err := Load(writeConfig(t, "[log]\nlevel = DEBUG\nformat = JSON\n[sites]\nurl_mode = Subdomain\nbase_domain = Pages.Corp.\nkeep_versions = 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != slog.LevelDebug || cfg.LogFormat != logging.FormatJSON {
		t.Errorf("log = %v %q", cfg.LogLevel, cfg.LogFormat)
	}
	if cfg.URLMode != URLModeSubdomain || cfg.BaseDomain != "pages.corp" || cfg.KeepVersions != 1 {
		t.Errorf("sites = %q %q %d", cfg.URLMode, cfg.BaseDomain, cfg.KeepVersions)
	}
	for _, bad := range []string{
		"[log]\nlevel = INFO+1\n",
		"[log]\nformat = xml\n",
		"[metrics]\ntoken = short\n",
		"[sites]\nurl_mode = bogus\n",
		"[sites]\nurl_mode = subdomain\n",
	} {
		if _, err := Load(writeConfig(t, bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	cfg, err = Load(writeConfig(t, ""))
	if err != nil || cfg.URLMode != URLModePath || cfg.LogFormat != logging.FormatText || cfg.MetricsListen != "" {
		t.Errorf("defaults = %+v, %v", cfg, err)
	}
}

func TestSAMLDefaultsToDisabled(t *testing.T) {
	cfg, err := Load(writeConfig(t, "[server]\nhttp_port = 8080\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SAML.Enabled {
		t.Fatal("SAML enabled without a [saml] section")
	}
}

func TestLoadSAMLConfig(t *testing.T) {
	dir := t.TempDir()
	cfg, err := Load(writeConfig(t, `
[saml]
enabled = true
idp_metadata_file = `+filepath.Join(dir, "idp.xml")+`
entity_id = https://pages.example.com/saml
acs_url = https://pages.example.com/v1/auth/saml/acs
groups_attribute = memberOf
nameid_attribute = employeeNumber
allowed_email_domain = @Corp.Example
key_file = `+filepath.Join(dir, "sp.key")+`
cert_file = `+filepath.Join(dir, "sp.crt")+`
`))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.SAML
	if !c.Enabled || c.IDPMetadataFile != filepath.Join(dir, "idp.xml") || c.EmailAttribute != "email" ||
		c.GroupsAttribute != "memberOf" || c.NameIDAttribute != "employeeNumber" || c.AllowedEmailDomain != "corp.example" ||
		c.KeyFile == "" || c.CertFile == "" {
		t.Fatalf("unexpected SAML config: %+v", c)
	}
	// SAML alone is enough to switch local login off.
	if _, err := Load(writeConfig(t, "[auth]\nlocal_login = false\n[saml]\nenabled = true\nidp_metadata_url = https://idp.example/metadata\n"+
		"entity_id = sp\nacs_url = https://p.example/v1/auth/saml/acs\n")); err != nil {
		t.Fatal(err)
	}
}

func TestLoadSAMLConfigErrors(t *testing.T) {
	const ok = "entity_id = sp\nacs_url = https://p.example/v1/auth/saml/acs\nidp_metadata_url = https://idp.example/metadata\n"
	cases := map[string]string{
		"typo enabled":      "[saml]\nenabled = yes please\n",
		"no metadata":       "[saml]\nenabled = true\nentity_id = sp\nacs_url = https://p.example/acs\n",
		"both metadata":     "[saml]\nenabled = true\nidp_metadata_file = /x.xml\n" + ok,
		"plain http url":    "[saml]\nenabled = true\nentity_id = sp\nacs_url = https://p.example/acs\nidp_metadata_url = http://idp.example/m\n",
		"missing entity id": "[saml]\nenabled = true\nacs_url = https://p.example/acs\nidp_metadata_url = https://idp.example/m\n",
		"missing acs":       "[saml]\nenabled = true\nentity_id = sp\nidp_metadata_url = https://idp.example/m\n",
		"relative acs":      "[saml]\nenabled = true\nentity_id = sp\nacs_url = /v1/auth/saml/acs\nidp_metadata_url = https://idp.example/m\n",
		"plain http acs":    "[saml]\nenabled = true\nentity_id = sp\nacs_url = http://p.example/acs\nidp_metadata_url = https://idp.example/m\n",
		"key without cert":  "[saml]\nenabled = true\nkey_file = /k.pem\n" + ok,
		"cert without key":  "[saml]\nenabled = true\ncert_file = /c.pem\n" + ok,
		"nobody can log in": "[auth]\nlocal_login = false\n[saml]\nenabled = false\n",
		"nobody (oidc off)": "[auth]\nlocal_login = false\n[oidc]\nenabled = false\n[saml]\nenabled = false\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
	// A disabled section is ignored even when incomplete.
	if _, err := Load(writeConfig(t, "[saml]\nenabled = false\nacs_url = junk\n")); err != nil {
		t.Fatal(err)
	}
}

func TestEmptyGroupsKeyDisablesMapping(t *testing.T) {
	saml := "[saml]\nenabled = true\nidp_metadata_url = https://idp.example/metadata\n" +
		"entity_id = https://pages.example.com/saml\nacs_url = https://pages.example.com/v1/auth/saml/acs\n"
	oidc := "[oidc]\nenabled = true\nissuer = https://idp.example\nclient_id = pages\nclient_secret = s\n" +
		"redirect_url = https://pages.example.com/v1/auth/oidc/callback\n"
	cfg, err := Load(writeConfig(t, saml+oidc))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SAML.GroupsAttribute != "groups" || cfg.OIDC.GroupsClaim != "groups" {
		t.Fatalf("absent keys: groups_attribute = %q, groups_claim = %q, want the default", cfg.SAML.GroupsAttribute, cfg.OIDC.GroupsClaim)
	}
	cfg, err = Load(writeConfig(t, saml+"groups_attribute =\n"+oidc+"groups_claim =\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SAML.GroupsAttribute != "" || cfg.OIDC.GroupsClaim != "" {
		t.Fatalf("empty keys: groups_attribute = %q, groups_claim = %q, want mapping off", cfg.SAML.GroupsAttribute, cfg.OIDC.GroupsClaim)
	}
}
