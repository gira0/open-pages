package config

import (
	"os"
	"strings"
	"testing"
)

// TestChartSettings loads the settings.ini that the Helm chart renders, so a renamed key or a
// drifted value format breaks CI instead of the pod's startup. The helm CI job sets
// OPEN_PAGES_CHART_SETTINGS to the rendered file; without it the test is skipped.
//
// The chart delivers secrets as *_FILE variables pointing into a mounted Secret. The CI step
// reads those variables from the rendered Deployment, points each at a dummy file and lists
// them as NAME=PATH lines in the file named by OPEN_PAGES_CHART_ENV, so the test sees the
// environment the pod gets and a misspelled name in the Deployment fails here.
func TestChartSettings(t *testing.T) {
	path := os.Getenv("OPEN_PAGES_CHART_SETTINGS")
	if path == "" {
		t.Skip("OPEN_PAGES_CHART_SETTINGS is not set: no rendered chart settings to load")
	}
	if envPath := os.Getenv("OPEN_PAGES_CHART_ENV"); envPath != "" {
		data, err := os.ReadFile(envPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if name, value, ok := strings.Cut(line, "="); ok {
				t.Setenv(name, value)
			}
		}
	}

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
