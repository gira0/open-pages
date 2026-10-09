package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/ini.v1"
)

// Config holds all runtime settings, read from settings.ini.
type Config struct {
	Listen         string     // address to bind, e.g. ":8080"
	DataPath       string     // absolute base dir; sites live in <DataPath>/op_data
	TmpPath        string     // absolute base dir; uploads are staged in <TmpPath>/tmp
	CookieSecure   bool       // set the Secure flag on the session cookie (enable behind HTTPS)
	MaxUploadBytes int64      // max size of an uploaded archive
	MaxExtractSize int64      // max total uncompressed size of an archive
	MaxExtractFile int        // max number of entries in an archive
	KeepVersions   int        // versions kept on disk per site, including the current one
	CORSOrigins    []string   // allowed cross-origin callers; empty means same-origin only
	URLMode        string     // how a request names its site: urlModePath or urlModeSubdomain
	BaseDomain     string     // bare host (no port) of the API and UI; also the parent of site subdomains
	LogLevel       slog.Level // minimum level written to the log
	LogFormat      string     // logFormatText or logFormatJSON
	MetricsListen  string     // dedicated address for /metrics, e.g. "127.0.0.1:9100"; empty means none
	MetricsToken   string     // bearer token for /metrics on the main listener; empty disables it there
	LocalLogin     bool       // serve /v1/user/register and /v1/user/login; false for OIDC-only setups
	OIDC           OIDCConfig // optional OpenID Connect sign-in; disabled by default
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

// URL modes for [sites] url_mode.
const (
	urlModePath      = "path"      // <base_domain>/<site>/...
	urlModeSubdomain = "subdomain" // <site>.<base_domain>/...
)

// minMetricsTokenLen keeps an obviously guessable token from being configured.
const minMetricsTokenLen = 16

func defaultConfig() Config {
	return Config{
		Listen:         ":8080",
		DataPath:       ".",
		TmpPath:        ".",
		MaxUploadBytes: 100 << 20,
		MaxExtractSize: 500 << 20,
		MaxExtractFile: 10000,
		KeepVersions:   5,
		URLMode:        urlModePath,
		LogLevel:       slog.LevelInfo,
		LogFormat:      logFormatText,
		LocalLogin:     true,
	}
}

func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()
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

	cfg.KeepVersions = max(f.Section("sites").Key("keep_versions").MustInt(cfg.KeepVersions), 1)

	sites := f.Section("sites")
	cfg.URLMode = strings.ToLower(sites.Key("url_mode").MustString(cfg.URLMode))
	cfg.BaseDomain = strings.ToLower(strings.TrimSuffix(sites.Key("base_domain").String(), "."))
	if cfg.URLMode != urlModePath && cfg.URLMode != urlModeSubdomain {
		return cfg, fmt.Errorf("sites.url_mode %q: must be %q or %q", cfg.URLMode, urlModePath, urlModeSubdomain)
	}
	if cfg.URLMode == urlModeSubdomain && (cfg.BaseDomain == "" || strings.ContainsAny(cfg.BaseDomain, ":/")) {
		return cfg, fmt.Errorf("sites.base_domain %q: subdomain mode needs a bare host name, for example pages.corp", cfg.BaseDomain)
	}

	logSec := f.Section("log")
	if cfg.LogLevel, err = parseLogLevel(logSec.Key("level").MustString("info")); err != nil {
		return cfg, err
	}
	cfg.LogFormat = strings.ToLower(logSec.Key("format").MustString(cfg.LogFormat))
	if cfg.LogFormat != logFormatText && cfg.LogFormat != logFormatJSON {
		return cfg, fmt.Errorf("log.format %q: must be %q or %q", cfg.LogFormat, logFormatText, logFormatJSON)
	}

	metricsSec := f.Section("metrics")
	cfg.MetricsListen = metricsSec.Key("listen").String()
	cfg.MetricsToken = metricsSec.Key("token").String()
	if cfg.MetricsToken != "" && len(cfg.MetricsToken) < minMetricsTokenLen {
		return cfg, fmt.Errorf("metrics.token: must be at least %d characters", minMetricsTokenLen)
	}

	if cfg.LocalLogin, err = strictBool(f.Section("auth"), "local_login", cfg.LocalLogin); err != nil {
		return cfg, err
	}
	if cfg.OIDC, err = loadOIDCConfig(f.Section("oidc")); err != nil {
		return cfg, err
	}
	if !cfg.LocalLogin && !cfg.OIDC.Enabled {
		return cfg, errors.New("auth.local_login = false needs [oidc] enabled = true, or nobody could sign in")
	}

	if cfg.DataPath, err = filepath.Abs(cfg.DataPath); err != nil {
		return cfg, fmt.Errorf("datapath: %w", err)
	}
	if cfg.TmpPath, err = filepath.Abs(cfg.TmpPath); err != nil {
		return cfg, fmt.Errorf("tmppath: %w", err)
	}
	return cfg, nil
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
		GroupsClaim:        strings.TrimSpace(sec.Key("groups_claim").MustString("groups")),
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
	if err := checkOIDCURL("oidc.issuer", c.Issuer); err != nil {
		return c, err
	}
	if err := checkOIDCURL("oidc.redirect_url", c.RedirectURL); err != nil {
		return c, err
	}
	return c, nil
}

// checkOIDCURL requires an absolute https URL; plain http is accepted only for loopback
// hosts (local development against a test provider).
func checkOIDCURL(key, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.Fragment != "" {
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
