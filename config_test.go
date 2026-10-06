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
	if _, err := loadConfig("settings.ini"); err != nil {
		t.Fatal(err)
	}
}
