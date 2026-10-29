package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

[sites]
app_host = pages.corp
content_domain = pages-content.corp
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

// validSites are the required [sites] settings; writeConfig prepends them so each test
// only states what it is about.
const validSites = "[sites]\napp_host = pages.corp\ncontent_domain = pages-content.corp\n"

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	return writeRawConfig(t, validSites+body)
}

func writeRawConfig(t *testing.T, body string) string {
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
	cfg, err := Load(writeRawConfig(t, "[log]\nlevel = DEBUG\nformat = JSON\n[sites]\napp_host = Pages.Corp.\ncontent_domain = Pages-Content.Corp\nkeep_versions = 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LogLevel != slog.LevelDebug || cfg.LogFormat != logging.FormatJSON {
		t.Errorf("log = %v %q", cfg.LogLevel, cfg.LogFormat)
	}
	if cfg.AppHost != "pages.corp" || cfg.ContentDomain != "pages-content.corp" || cfg.KeepVersions != 1 {
		t.Errorf("sites = %q %q %d", cfg.AppHost, cfg.ContentDomain, cfg.KeepVersions)
	}
	for _, bad := range []string{
		"[log]\nlevel = INFO+1\n",
		"[log]\nformat = xml\n",
		"[metrics]\ntoken = short\n",
	} {
		if _, err := Load(writeConfig(t, bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	cfg, err = Load(writeConfig(t, ""))
	if err != nil || cfg.LogFormat != logging.FormatText || cfg.MetricsListen != "" {
		t.Errorf("defaults = %+v, %v", cfg, err)
	}
}

func TestLoadSiteHosts(t *testing.T) {
	sites := func(app, content string) string {
		return "[sites]\napp_host = " + app + "\ncontent_domain = " + content + "\n"
	}
	// A separate registrable domain and siblings under a shared parent work.
	for _, ok := range [][2]string{
		{"pages.corp", "pages-content.corp"},
		{"app.pages.corp", "sites.pages.corp"},
		{"pages.corp", "sites.corp"},
		{"a.b.corp", "b.corp.example"}, // unrelated hosts
	} {
		if _, err := Load(writeRawConfig(t, sites(ok[0], ok[1]))); err != nil {
			t.Errorf("%v: %v", ok, err)
		}
	}
	for name, tc := range map[string]struct{ body, want string }{
		"missing app_host":       {"[sites]\ncontent_domain = c.corp\n", "app_host"},
		"missing content_domain": {"[sites]\napp_host = pages.corp\n", "content_domain"},
		"no sites section":       {"[paths]\ndatapath = .\n", "app_host"},
		"port":                   {sites("pages.corp:8080", "c.corp"), "app_host"},
		"scheme":                 {sites("https://pages.corp", "c.corp"), "app_host"},
		"path":                   {sites("pages.corp", "c.corp/x"), "content_domain"},
		"underscore label":       {sites("_bad.corp", "c.corp"), "app_host"},
		"leading hyphen":         {sites("pages.corp", "-bad.corp"), "content_domain"},
		"double trailing dot":    {sites("pages.corp..", "c.corp"), "app_host"},
		"too long":               {sites("pages.corp", strings.Repeat("a.", 130)+"corp"), "content_domain"},
		"equal hosts":            {sites("pages.corp", "Pages.Corp."), "differ"},
		"app host is a site":     {sites("app.c.corp", "c.corp"), "under sites.content_domain"},
		"app host deep under it": {sites("a.b.c.corp", "c.corp"), "under sites.content_domain"},
		"content under app":      {sites("pages.corp", "sites.pages.corp"), "subdomain of sites.app_host"},
		"content deep under app": {sites("pages.corp", "x.y.pages.corp"), "subdomain of sites.app_host"},
		"url_mode":               {sites("pages.corp", "c.corp") + "url_mode = path\n", "url_mode"},
		"base_domain":            {sites("pages.corp", "c.corp") + "base_domain = pages.corp\n", "base_domain"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeRawConfig(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
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

func TestLoadDeployLimitErrors(t *testing.T) {
	for _, line := range []string{
		"max_concurrent_deploys = many",
		"max_concurrent_deploys = 0",
		"max_concurrent_deploys = -1",
		"deploy_timeout_seconds = 10m",
		"deploy_timeout_seconds = 0",
		"deploy_timeout_seconds = 9223372037",
	} {
		path := filepath.Join(t.TempDir(), "settings.ini")
		if err := os.WriteFile(path, []byte("[limits]\n"+line+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Errorf("%q: expected an error", line)
		}
	}
}

func TestLoadRejectsMalformedCORSOrigin(t *testing.T) {
	for _, origin := range []string{"example.com", "https://a.example/path", "*", "https://user@example.com", "https://user:pw@example.com"} {
		p := filepath.Join(t.TempDir(), "settings.ini")
		if err := os.WriteFile(p, []byte("[server]\ncors_origins = "+origin+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(p); err == nil {
			t.Errorf("origin %q accepted", origin)
		}
	}
}

func TestLoadAuditRetention(t *testing.T) {
	cfg, err := Load(writeConfig(t, ""))
	if err != nil || cfg.AuditRetention != 365 {
		t.Fatalf("default retention = %d, err %v", cfg.AuditRetention, err)
	}
	for body, want := range map[string]int{"[audit]\nretention_days = 30\n": 30, "[audit]\nretention_days = 0\n": 0} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil || cfg.AuditRetention != want {
			t.Errorf("%q: retention = %d, err %v; want %d", body, cfg.AuditRetention, err, want)
		}
	}
	for _, line := range []string{"retention_days = -1", "retention_days = forever", "retention_days = 36501", "retention_days ="} {
		if _, err := Load(writeConfig(t, "[audit]\n"+line+"\n")); err == nil {
			t.Errorf("%q: expected an error", line)
		}
	}
}

func TestLoadQuotas(t *testing.T) {
	cfg, err := Load(writeConfig(t, ""))
	if err != nil || cfg.SiteQuota != 0 || cfg.UserQuota != 0 || cfg.SitesPerUser != 0 {
		t.Fatalf("defaults = %d, %d, %d, err %v; want no limits", cfg.SiteQuota, cfg.UserQuota, cfg.SitesPerUser, err)
	}
	cfg, err = Load(writeConfig(t, "[quotas]\nsite_mb = 50\nuser_mb = 200\nsites_per_user = 10\n"))
	if err != nil || cfg.SiteQuota != 50<<20 || cfg.UserQuota != 200<<20 || cfg.SitesPerUser != 10 {
		t.Fatalf("quotas = %d, %d, %d, err %v", cfg.SiteQuota, cfg.UserQuota, cfg.SitesPerUser, err)
	}
	for _, line := range []string{
		"site_mb = -1", "site_mb = lots", "site_mb = 8796093022208", "site_mb =",
		"user_mb = -5", "user_mb = 1.5",
		"sites_per_user = -1", "sites_per_user = many", "sites_per_user = 2147483648",
	} {
		if _, err := Load(writeConfig(t, "[quotas]\n"+line+"\n")); err == nil {
			t.Errorf("%q: expected an error", line)
		}
	}
}
