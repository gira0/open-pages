package main

import (
	"fmt"
	"log/slog"
	"path/filepath"
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

	if cfg.DataPath, err = filepath.Abs(cfg.DataPath); err != nil {
		return cfg, fmt.Errorf("datapath: %w", err)
	}
	if cfg.TmpPath, err = filepath.Abs(cfg.TmpPath); err != nil {
		return cfg, fmt.Errorf("tmppath: %w", err)
	}
	return cfg, nil
}
