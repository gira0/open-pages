// Package config reads and validates settings.ini into a Config.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gira0/open-pages/internal/logging"
	"github.com/gira0/open-pages/internal/names"
	"gopkg.in/ini.v1"
)

// Config holds all runtime settings, read from settings.ini.
type Config struct {
	Listen         string        // address to bind, e.g. ":8080"
	DataPath       string        // absolute base dir; sites live in <DataPath>/op_data
	TmpPath        string        // absolute base dir; uploads are staged in <TmpPath>/tmp
	CookieSecure   bool          // set the Secure flag on the session cookie (enable behind HTTPS)
	MaxUploadBytes int64         // max size of an uploaded archive
	MaxExtractSize int64         // max total uncompressed size of an archive
	MaxExtractFile int           // max number of entries in an archive
	MaxDeploys     int           // uploads extracted at once; more get 503 with Retry-After
	DeployTimeout  time.Duration // time budget for extracting one upload
	KeepVersions   int           // versions kept on disk per site, including the current one
	CORSOrigins    []string      // allowed cross-origin callers; empty means same-origin only
	AppHost        string        // bare host (no port) of the API and UI
	ContentDomain  string        // bare domain whose subdomains serve the sites: <site>.<ContentDomain>
	LogLevel       slog.Level    // minimum level written to the log
	LogFormat      string        // logging.FormatText or logging.FormatJSON
	MetricsListen  string        // dedicated address for /metrics, e.g. "127.0.0.1:9100"; empty means none
	MetricsToken   string        // bearer token for /metrics on the main listener; empty disables it there
	AuditRetention int           // days audit log entries are kept; 0 keeps them for ever
	LocalLogin     bool          // serve /v1/user/register and /v1/user/login; false for OIDC-only setups
	OIDC           OIDCConfig    // optional OpenID Connect sign-in; disabled by default
	SAML           SAMLConfig    // optional SAML 2.0 sign-in; disabled by default
}

// SAMLConfig is the [saml] section.
type SAMLConfig struct {
	Enabled            bool
	IDPMetadataURL     string // https URL of the identity provider's metadata; exclusive with IDPMetadataFile
	IDPMetadataFile    string // absolute path of the provider's metadata XML
	EntityID           string // this service's entity ID (the audience assertions must name)
	ACSURL             string // absolute URL of /v1/auth/saml/acs as registered at the provider
	EmailAttribute     string // assertion attribute holding the email address
	GroupsAttribute    string // assertion attribute holding the group names; empty disables group mapping
	NameIDAttribute    string // when set, the user's subject is this attribute instead of the NameID
	AllowedEmailDomain string // when set, only emails in this domain may sign in
	KeyFile            string // absolute path of the PEM key that signs AuthnRequests; empty means unsigned requests
	CertFile           string // absolute path of the matching PEM certificate
}

// OIDCConfig is the [oidc] section.
type OIDCConfig struct {
	Enabled            bool
	Issuer             string // issuer URL, exactly as the provider reports it
	ClientID           string
	ClientSecret       string   // from the settings file only; never logged
	RedirectURL        string   // absolute URL of /v1/auth/oidc/callback as registered at the provider
	Scopes             []string // always includes "openid"
	EmailClaim         string   // ID token claim holding the email address
	GroupsClaim        string   // ID token claim holding the group names; empty disables group mapping
	AllowedEmailDomain string   // when set, only emails in this domain may sign in
}

// maxTimeoutSecs is the longest timeout in seconds that fits a time.Duration.
const maxTimeoutSecs = int(math.MaxInt64 / int64(time.Second))

// minMetricsTokenLen keeps an obviously guessable token from being configured.
const minMetricsTokenLen = 16

// Default returns the settings used when settings.ini leaves a key out.
func Default() Config {
	return Config{
		Listen:         ":8080",
		DataPath:       ".",
		TmpPath:        ".",
		MaxUploadBytes: 100 << 20,
		MaxExtractSize: 500 << 20,
		MaxExtractFile: 10000,
		MaxDeploys:     2,
		DeployTimeout:  10 * time.Minute,
		KeepVersions:   5,
		LogLevel:       slog.LevelInfo,
		LogFormat:      logging.FormatText,
		AuditRetention: 365,
		LocalLogin:     true,
	}
}

// Load reads the settings file at path, applies defaults for missing keys and validates the
// result. Unparseable or inconsistent settings are an error rather than silently defaulted.
func Load(path string) (Config, error) {
	cfg := Default()
	f, err := ini.Load(path)
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}

	server := f.Section("server")
	host := server.Key("host").MustString("")
	port := server.Key("http_port").MustInt(8080)
	cfg.Listen = fmt.Sprintf("%s:%d", host, port)
	cfg.CookieSecure = server.Key("cookie_secure").MustBool(false)
	if origins := server.Key("cors_origins").String(); origins != "" {
		for _, o := range strings.Split(origins, ",") {
			if o = strings.TrimSpace(o); o != "" {
				if err := http.NewCrossOriginProtection().AddTrustedOrigin(o); err != nil {
					return cfg, fmt.Errorf("cors_origins entry %q: %w", o, err)
				}
				// AddTrustedOrigin tolerates userinfo, which no browser sends in an Origin.
				if u, err := url.Parse(o); err != nil || u.User != nil {
					return cfg, fmt.Errorf("cors_origins entry %q must be scheme://host[:port] without credentials", o)
				}
				cfg.CORSOrigins = append(cfg.CORSOrigins, o)
			}
		}
	}

	paths := f.Section("paths")
	cfg.DataPath = paths.Key("datapath").MustString(cfg.DataPath)
	cfg.TmpPath = paths.Key("tmppath").MustString(cfg.TmpPath)

	limits := f.Section("limits")
	cfg.MaxUploadBytes = limits.Key("max_upload_mb").MustInt64(cfg.MaxUploadBytes>>20) << 20
	cfg.MaxExtractSize = limits.Key("max_extract_mb").MustInt64(cfg.MaxExtractSize>>20) << 20
	cfg.MaxExtractFile = limits.Key("max_extract_files").MustInt(cfg.MaxExtractFile)
	if cfg.MaxDeploys, err = positiveInt(limits, "max_concurrent_deploys", cfg.MaxDeploys); err != nil {
		return cfg, err
	}
	timeoutSecs, err := positiveInt(limits, "deploy_timeout_seconds", int(cfg.DeployTimeout/time.Second))
	if err != nil {
		return cfg, err
	}
	if timeoutSecs > maxTimeoutSecs {
		return cfg, fmt.Errorf("limits.deploy_timeout_seconds: must be at most %d", maxTimeoutSecs)
	}
	cfg.DeployTimeout = time.Duration(timeoutSecs) * time.Second

	cfg.KeepVersions = max(f.Section("sites").Key("keep_versions").MustInt(cfg.KeepVersions), 1)

	sites := f.Section("sites")
	for _, old := range []string{"url_mode", "base_domain"} {
		if sites.HasKey(old) {
			return cfg, fmt.Errorf("sites.%s is no longer supported: sites are served from subdomains of a content domain; set sites.app_host and sites.content_domain instead", old)
		}
	}
	if cfg.AppHost, err = hostSetting(sites, "app_host"); err != nil {
		return cfg, err
	}
	if cfg.ContentDomain, err = hostSetting(sites, "content_domain"); err != nil {
		return cfg, err
	}
	// Neither host may be, or sit under, the other, at any depth. A site under the app host
	// could set Domain= cookies that reach the app; an app host under the content domain
	// would be a site host.
	switch {
	case cfg.AppHost == cfg.ContentDomain:
		return cfg, fmt.Errorf("sites.content_domain %q: must differ from sites.app_host", cfg.ContentDomain)
	case strings.HasSuffix(cfg.ContentDomain, "."+cfg.AppHost):
		return cfg, fmt.Errorf("sites.content_domain %q: must not be a subdomain of sites.app_host %q, or sites could set cookies for the app; use a sibling or a separate domain", cfg.ContentDomain, cfg.AppHost)
	case strings.HasSuffix(cfg.AppHost, "."+cfg.ContentDomain):
		return cfg, fmt.Errorf("sites.app_host %q: must not be under sites.content_domain %q, where site hosts live; use a separate domain for sites", cfg.AppHost, cfg.ContentDomain)
	}

	logSec := f.Section("log")
	if cfg.LogLevel, err = logging.ParseLevel(logSec.Key("level").MustString("info")); err != nil {
		return cfg, err
	}
	cfg.LogFormat = strings.ToLower(logSec.Key("format").MustString(cfg.LogFormat))
	if cfg.LogFormat != logging.FormatText && cfg.LogFormat != logging.FormatJSON {
		return cfg, fmt.Errorf("log.format %q: must be %q or %q", cfg.LogFormat, logging.FormatText, logging.FormatJSON)
	}

	metricsSec := f.Section("metrics")
	cfg.MetricsListen = metricsSec.Key("listen").String()
	cfg.MetricsToken = metricsSec.Key("token").String()
	if cfg.MetricsToken != "" && len(cfg.MetricsToken) < minMetricsTokenLen {
		return cfg, fmt.Errorf("metrics.token: must be at least %d characters", minMetricsTokenLen)
	}

	if cfg.AuditRetention, err = boundedInt(f.Section("audit"), "retention_days", cfg.AuditRetention, 0, maxAuditRetention); err != nil {
		return cfg, err
	}

	if cfg.LocalLogin, err = strictBool(f.Section("auth"), "local_login", cfg.LocalLogin); err != nil {
		return cfg, err
	}
	if cfg.OIDC, err = loadOIDCConfig(f.Section("oidc")); err != nil {
		return cfg, err
	}
	if cfg.SAML, err = loadSAMLConfig(f.Section("saml")); err != nil {
		return cfg, err
	}
	if !cfg.LocalLogin && !cfg.OIDC.Enabled && !cfg.SAML.Enabled {
		return cfg, errors.New("auth.local_login = false needs [oidc] or [saml] enabled = true, or nobody could sign in")
	}

	if cfg.DataPath, err = filepath.Abs(cfg.DataPath); err != nil {
		return cfg, fmt.Errorf("datapath: %w", err)
	}
	if cfg.TmpPath, err = filepath.Abs(cfg.TmpPath); err != nil {
		return cfg, fmt.Errorf("tmppath: %w", err)
	}
	return cfg, nil
}

// hostSetting reads a required bare host name: lowercased, one trailing dot trimmed, no
// scheme, port, path or spaces.
func hostSetting(sec *ini.Section, name string) (string, error) {
	raw := strings.TrimSpace(sec.Key(name).String())
	host := strings.TrimSuffix(strings.ToLower(raw), ".")
	if host == "" {
		return "", fmt.Errorf("sites.%s: required, a bare host name such as pages.corp", name)
	}
	if len(host) > 253 {
		return "", fmt.Errorf("sites.%s %q: host name too long", name, raw)
	}
	for _, label := range strings.Split(host, ".") {
		if !names.ValidSite(label) { // a DNS label: letters, digits, inner hyphens, 1 to 63 chars
			return "", fmt.Errorf("sites.%s %q: must be a bare host name (letters, digits and hyphens in each dot-separated label), without scheme, port or path", name, raw)
		}
	}
	return host, nil
}

// strictBool reads a boolean key, failing on anything that isn't a boolean instead of
// silently using the default (a typo must not leave a security setting at its default).
func strictBool(sec *ini.Section, name string, def bool) (bool, error) {
	if !sec.HasKey(name) {
		return def, nil
	}
	v, err := sec.Key(name).Bool()
	if err != nil {
		return def, fmt.Errorf("%s.%s: must be true or false", sec.Name(), name)
	}
	return v, nil
}

// positiveInt reads an integer key that must be at least 1, failing on a malformed or
// non-positive value instead of silently using the default.
func positiveInt(sec *ini.Section, name string, def int) (int, error) {
	if !sec.HasKey(name) {
		return def, nil
	}
	v, err := sec.Key(name).Int()
	if err != nil || v < 1 {
		return def, fmt.Errorf("%s.%s: must be a whole number of at least 1", sec.Name(), name)
	}
	return v, nil
}

// maxAuditRetention is 100 years, far below where a day count would overflow a time.
const maxAuditRetention = 36500

// boundedInt reads an integer key that must lie in [lo, hi], failing on a malformed or
// out-of-range value instead of silently using the default.
func boundedInt(sec *ini.Section, name string, def, lo, hi int) (int, error) {
	if !sec.HasKey(name) {
		return def, nil
	}
	v, err := sec.Key(name).Int()
	if err != nil || v < lo || v > hi {
		return def, fmt.Errorf("%s.%s: must be a whole number from %d to %d", sec.Name(), name, lo, hi)
	}
	return v, nil
}

// optionalString reads a key that defaults to def when absent, but stays empty when it is
// present and empty (MustString would substitute the default there too). An empty value
// turns the feature off, for example group mapping.
func optionalString(sec *ini.Section, name, def string) string {
	if !sec.HasKey(name) {
		return def
	}
	return strings.TrimSpace(sec.Key(name).String())
}

func loadOIDCConfig(sec *ini.Section) (OIDCConfig, error) {
	enabled, err := strictBool(sec, "enabled", false)
	if err != nil {
		return OIDCConfig{}, err
	}
	c := OIDCConfig{
		Enabled:            enabled,
		Issuer:             strings.TrimSpace(sec.Key("issuer").String()),
		ClientID:           strings.TrimSpace(sec.Key("client_id").String()),
		ClientSecret:       sec.Key("client_secret").String(),
		RedirectURL:        strings.TrimSpace(sec.Key("redirect_url").String()),
		EmailClaim:         sec.Key("email_claim").MustString("email"),
		GroupsClaim:        optionalString(sec, "groups_claim", "groups"),
		AllowedEmailDomain: strings.ToLower(strings.TrimPrefix(strings.TrimSpace(sec.Key("allowed_email_domain").String()), "@")),
	}
	if !c.Enabled {
		return OIDCConfig{}, nil
	}
	c.Scopes = strings.Fields(strings.ReplaceAll(sec.Key("scopes").MustString("openid email profile"), ",", " "))
	if !slices.Contains(c.Scopes, "openid") {
		c.Scopes = append([]string{"openid"}, c.Scopes...)
	}
	if c.Issuer == "" || c.ClientID == "" || c.ClientSecret == "" || c.RedirectURL == "" {
		return c, fmt.Errorf("oidc: issuer, client_id, redirect_url and and client_secret are required when enabled")
	}
	if err := CheckURL("oidc.issuer", c.Issuer); err != nil {
		return c, err
	}
	if err := CheckURL("oidc.redirect_url", c.RedirectURL); err != nil {
		return c, err
	}
	return c, nil
}

func loadSAMLConfig(sec *ini.Section) (SAMLConfig, error) {
	enabled, err := strictBool(sec, "enabled", false)
	if err != nil {
		return SAMLConfig{}, err
	}
	if !enabled {
		return SAMLConfig{}, nil
	}
	c := SAMLConfig{
		Enabled:            true,
		IDPMetadataURL:     strings.TrimSpace(sec.Key("idp_metadata_url").String()),
		IDPMetadataFile:    strings.TrimSpace(sec.Key("idp_metadata_file").String()),
		EntityID:           strings.TrimSpace(sec.Key("entity_id").String()),
		ACSURL:             strings.TrimSpace(sec.Key("acs_url").String()),
		EmailAttribute:     strings.TrimSpace(sec.Key("email_attribute").MustString("email")),
		GroupsAttribute:    optionalString(sec, "groups_attribute", "groups"),
		NameIDAttribute:    strings.TrimSpace(sec.Key("nameid_attribute").String()),
		AllowedEmailDomain: strings.ToLower(strings.TrimPrefix(strings.TrimSpace(sec.Key("allowed_email_domain").String()), "@")),
		KeyFile:            strings.TrimSpace(sec.Key("key_file").String()),
		CertFile:           strings.TrimSpace(sec.Key("cert_file").String()),
	}
	if c.EntityID == "" || c.ACSURL == "" {
		return c, errors.New("saml: entity_id and acs_url are required when enabled")
	}
	if (c.IDPMetadataURL == "") == (c.IDPMetadataFile == "") {
		return c, errors.New("saml: set exactly one of idp_metadata_url and idp_metadata_file")
	}
	if c.IDPMetadataURL != "" {
		if err := CheckURL("saml.idp_metadata_url", c.IDPMetadataURL); err != nil {
			return c, err
		}
	}
	if err := CheckURL("saml.acs_url", c.ACSURL); err != nil {
		return c, err
	}
	if (c.KeyFile == "") != (c.CertFile == "") {
		return c, errors.New("saml: key_file and cert_file must be set together")
	}
	for _, p := range []*string{&c.IDPMetadataFile, &c.KeyFile, &c.CertFile} {
		if *p == "" {
			continue
		}
		if *p, err = filepath.Abs(*p); err != nil {
			return c, fmt.Errorf("saml: %w", err)
		}
	}
	return c, nil
}

// CheckURL requires an absolute https URL; plain http is accepted only for loopback
// hosts (local development against a test provider). It is used for the OIDC and SAML URLs.
func CheckURL(key, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Fragment != "" {
		return fmt.Errorf("%s: must be an absolute URL", key)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if h := u.Hostname(); h == "localhost" || h == "127.0.0.1" || h == "::1" {
			return nil
		}
	}
	return fmt.Errorf("%s: must use https (http is only allowed for localhost)", key)
}
