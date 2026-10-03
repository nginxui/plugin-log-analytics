package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// manifestDoc is the part of plugin.json the tests look at. Unknown members
// are fine: the spec schema checks the whole file in CI.
type manifestDoc struct {
	ID          string   `json:"id"`
	Version     string   `json:"version"`
	Permissions []string `json:"permissions"`
	Events      []string `json:"events"`
	Server      struct {
		Executables map[string]string `json:"executables"`
		Lifecycle   string            `json:"lifecycle"`
	} `json:"server"`
	Webapp struct {
		BundlePath string            `json:"bundle_path"`
		StylePath  string            `json:"style_path"`
		Shared     map[string]string `json:"shared"`
		Chunks     map[string]string `json:"chunks"`
	} `json:"webapp"`
	IconPath          string            `json:"icon_path"`
	PermissionReasons map[string]string `json:"permission_reasons"`
	I18n              map[string]struct {
		Name              string            `json:"name"`
		Description       string            `json:"description"`
		PermissionReasons map[string]string `json:"permission_reasons"`
	} `json:"i18n"`
	SettingsSchema struct {
		Settings []struct {
			Key string `json:"key"`
		} `json:"settings"`
	} `json:"settings_schema"`
}

func readPluginJSON(t *testing.T) ([]byte, manifestDoc) {
	t.Helper()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "plugin.json"))
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	var m manifestDoc
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("decode plugin.json: %v", err)
	}
	return data, m
}

// executablePath is the packaged path build.sh gives one platform's binary.
func executablePath(platform string) string {
	name := "log-analytics-" + platform
	if strings.HasPrefix(platform, "windows-") {
		name += ".exe"
	}
	return "server/dist/" + name
}

// The hand written file keeps the layout of the packaged ones, so a narrowed
// manifest differs from it only where the platform is concerned.
func TestPluginJSONIsLaidOut(t *testing.T) {
	data, _ := readPluginJSON(t)
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		t.Fatalf("compact: %v", err)
	}
	formatted, err := layout(compact.Bytes())
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	if !bytes.Equal(formatted, data) {
		t.Fatal("plugin.json is not laid out with two space indentation and a trailing newline")
	}
}

func TestManifestShape(t *testing.T) {
	_, m := readPluginJSON(t)

	if m.ID != "com.nginxui.log-analytics" || m.Version == "" {
		t.Fatalf("id = %q, version = %q", m.ID, m.Version)
	}
	for _, locale := range []string{"zh_CN", "zh_TW", "ja_JP"} {
		if tr := m.I18n[locale]; tr.Name == "" || tr.Description == "" {
			t.Fatalf("i18n[%s] = %+v", locale, tr)
		}
	}
	if m.Server.Lifecycle != "resident" || len(m.Server.Executables) == 0 {
		t.Fatalf("server = %+v", m.Server)
	}
	for platform, path := range m.Server.Executables {
		if path != executablePath(platform) {
			t.Fatalf("executable %s = %q, build.sh packages %q", platform, path, executablePath(platform))
		}
	}
	// The server reads these settings by key.
	var keys []string
	for _, field := range m.SettingsSchema.Settings {
		keys = append(keys, field.Key)
	}
	for _, key := range []string{"incremental_index_interval", "max_concurrent_index_tasks", "index_custom_mmdb"} {
		if !slices.Contains(keys, key) {
			t.Fatalf("settings schema misses %s", key)
		}
	}
	if !slices.Contains(m.Events, "log.paths_changed") {
		t.Fatalf("events = %v", m.Events)
	}
}

// A reason must explain a permission the plugin asks for, the host refuses
// the manifest otherwise.
func TestPermissionReasonsNameRequestedPermissions(t *testing.T) {
	_, m := readPluginJSON(t)
	check := func(where string, reasons map[string]string) {
		for permission := range reasons {
			if !slices.Contains(m.Permissions, permission) {
				t.Errorf("%s explains %q, which is not in permissions", where, permission)
			}
		}
	}
	check("permission_reasons", m.PermissionReasons)
	for locale, translated := range m.I18n {
		check(fmt.Sprintf("i18n.%s.permission_reasons", locale), translated.PermissionReasons)
	}
}

func TestFilterPlatformKeepsOnlyOneExecutable(t *testing.T) {
	data, m := readPluginJSON(t)

	for platform, path := range m.Server.Executables {
		narrowed, err := FilterPlatform(data, platform)
		if err != nil {
			t.Fatalf("filter %s: %v", platform, err)
		}

		var got manifestDoc
		if err := json.Unmarshal(narrowed, &got); err != nil {
			t.Fatalf("decode %s: %v", platform, err)
		}
		if len(got.Server.Executables) != 1 || got.Server.Executables[platform] != path {
			t.Fatalf("executables for %s = %v", platform, got.Server.Executables)
		}

		// Everything but the executables map survives byte for byte.
		restored, err := withExecutablesOf(narrowed, data)
		if err != nil {
			t.Fatalf("restore %s: %v", platform, err)
		}
		if !bytes.Equal(restored, data) {
			t.Fatalf("the %s manifest differs from plugin.json beyond server.executables", platform)
		}
	}

	if _, err := FilterPlatform(data, "plan9-386"); err == nil {
		t.Fatal("an undeclared platform must be refused")
	}
}

// The webapp build writes the paths of its bundle and the shared libraries it
// expects in a fragment. plugin.json has to name the same.
func TestManifestMatchesTheWebappFragment(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "webapp", "dist", "manifest.webapp.json"))
	if err != nil {
		t.Skip("the webapp is not built")
	}

	var fragment struct {
		BundlePath string            `json:"bundle_path"`
		StylePath  string            `json:"style_path"`
		Shared     map[string]string `json:"shared"`
		Chunks     map[string]string `json:"chunks"`
	}
	if err := json.Unmarshal(data, &fragment); err != nil {
		t.Fatalf("decode the fragment: %v", err)
	}

	_, m := readPluginJSON(t)
	w := m.Webapp
	if fragment.BundlePath != w.BundlePath || fragment.StylePath != w.StylePath {
		t.Fatalf("bundle %q, style %q, the webapp built %q and %q", w.BundlePath, w.StylePath, fragment.BundlePath, fragment.StylePath)
	}
	if fmt.Sprint(fragment.Chunks) != fmt.Sprint(w.Chunks) {
		t.Fatalf("chunks %v, the webapp built %v", w.Chunks, fragment.Chunks)
	}
	if fmt.Sprint(fragment.Shared) != fmt.Sprint(w.Shared) {
		t.Fatalf("shared %v, the webapp was built against %v; copy them into plugin.json", w.Shared, fragment.Shared)
	}
	paths := []string{w.BundlePath, w.StylePath, m.IconPath}
	for _, path := range w.Chunks {
		paths = append(paths, path)
	}
	for _, path := range paths {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatalf("%s is in the manifest but not built: %v", path, err)
		}
	}
}

// serverExecutables decodes a manifest down to its server block and finds
// server.executables in it.
func serverExecutables(data []byte) (manifest object, at int, server object, exeAt int, err error) {
	if manifest, err = decodeObject(data); err != nil {
		return
	}
	at = manifest.index("server")
	if server, err = decodeObject(manifest[at].Value); err != nil {
		return
	}
	exeAt = server.index("executables")
	return
}

// withExecutablesOf puts the server.executables of source back into a
// narrowed manifest.
func withExecutablesOf(narrowed, source []byte) ([]byte, error) {
	manifest, at, server, exeAt, err := serverExecutables(narrowed)
	if err != nil {
		return nil, err
	}
	_, _, sourceServer, sourceExeAt, err := serverExecutables(source)
	if err != nil {
		return nil, err
	}
	server[exeAt].Value = sourceServer[sourceExeAt].Value
	manifest[at].Value = server.encode()
	return layout(manifest.encode())
}
