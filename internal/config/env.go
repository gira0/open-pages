package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/ini.v1"
)

// envPrefix starts the name of every environment variable that overrides a setting:
// OPEN_PAGES_<SECTION>_<KEY>, for example OPEN_PAGES_OIDC_CLIENT_SECRET for [oidc]
// client_secret. OPEN_PAGES_<SECTION>_<KEY>_FILE reads the value from a file instead.
const envPrefix = "OPEN_PAGES_"

// envFileSuffix marks a variable whose value is the path of a file holding the setting.
const envFileSuffix = "_FILE"

// envSections are the sections of settings.ini. Only these are looked at: Kubernetes
// injects variables such as OPEN_PAGES_SERVICE_HOST for a Service called open-pages, and
// those must not be mistaken for settings. No section name contains an underscore, so the
// first word after the prefix names the section and the rest names the key. Add a new
// section here.
var envSections = []string{"server", "paths", "limits", "sites", "log", "metrics", "audit", "quotas", "auth", "oidc", "saml"}

// fileKeys are the settings whose own name ends in _file. OPEN_PAGES_SAML_KEY_FILE sets
// saml key_file directly (it is a path); the file form of that setting is
// OPEN_PAGES_SAML_KEY_FILE_FILE. Without this list the two readings could not be told apart.
var fileKeys = []string{"saml.idp_metadata_file", "saml.key_file", "saml.cert_file"}

// applyEnv overrides settings in f from the environment, so a deployment can set everything
// (secrets included) without writing them into the settings file. The environment wins
// over the file. Setting both NAME and NAME_FILE is an error. The values are applied before
// any parsing, so every setting, present or future, gets this without further code.
func applyEnv(f *ini.File, environ []string) error {
	type setting struct{ sec, key string }
	direct := map[setting]string{}
	fromFile := map[setting]string{}

	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		rest, ok := strings.CutPrefix(name, envPrefix)
		if !ok {
			continue
		}
		sec, key, ok := strings.Cut(strings.ToLower(rest), "_")
		if !ok || key == "" || !slices.Contains(envSections, sec) {
			continue
		}
		if k, isFile := strings.CutSuffix(key, strings.ToLower(envFileSuffix)); isFile && k != "" &&
			!slices.Contains(fileKeys, sec+"."+key) {
			fromFile[setting{sec, k}] = name
			continue
		}
		direct[setting{sec, key}] = value
	}

	// Sorted so that errors and the order of application do not depend on map iteration.
	var files []setting
	for s := range fromFile {
		files = append(files, s)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].sec+files[i].key < files[j].sec+files[j].key })

	for s, v := range direct {
		if _, both := fromFile[s]; both {
			return fmt.Errorf("%s%s_%s and %s are both set; use only one", envPrefix, strings.ToUpper(s.sec), strings.ToUpper(s.key), fromFile[s])
		}
		f.Section(s.sec).Key(s.key).SetValue(v)
	}
	for _, s := range files {
		name := fromFile[s]
		v, err := readSecretFile(os.Getenv(name))
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		f.Section(s.sec).Key(s.key).SetValue(v)
	}
	return nil
}

// readSecretFile returns the contents of the file at path without its trailing newline
// (editors and `echo` add one; Kubernetes Secret mounts do not). The path comes from the
// operator's environment, the same trust level as the settings file itself, and is
// cleaned before use.
func readSecretFile(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("the file path is empty")
	}
	b, err := os.ReadFile(filepath.Clean(path)) //nolint:gosec // G304: operator-supplied path, like -config
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(b), "\r\n"), nil
}
