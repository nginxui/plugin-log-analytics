package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/nginxui/plugin-sdk-go/protocol"
)

func TestRenderIsStable(t *testing.T) {
	first, err := Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	second, err := Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Fatal("Render() is not deterministic")
	}
	if !bytes.HasSuffix(first, []byte("\n")) {
		t.Fatal("the rendered manifest has no trailing newline")
	}
}

func TestCommittedManifestIsUpToDate(t *testing.T) {
	want, err := Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	root, err := repoRoot()
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(root, "plugin.json"))
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}

	if !bytes.Equal(want, got) {
		t.Fatal("plugin.json is stale, run: go run ./cmd/manifest")
	}
}

func TestManifestShape(t *testing.T) {
	data, err := Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	var m protocol.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if m.ID != PluginID || m.Version != PluginVersion {
		t.Fatalf("id = %q, version = %q", m.ID, m.Version)
	}
	if m.APIVersion != protocol.APIVersion {
		t.Fatalf("api_version = %d", m.APIVersion)
	}
	for _, locale := range []string{"zh_CN", "zh_TW", "ja_JP"} {
		if tr := m.I18n[locale]; tr.Name == "" || tr.Description == "" {
			t.Fatalf("i18n[%s] = %+v", locale, tr)
		}
	}
	if m.Server == nil || m.Server.Lifecycle != protocol.LifecycleResident {
		t.Fatalf("server = %+v", m.Server)
	}
	if len(m.Server.Executables) != len(platforms) {
		t.Fatalf("executables = %v", m.Server.Executables)
	}
	for _, p := range platforms {
		key := p.OS + "-" + p.Arch
		if m.Server.Executables[key] != ExecutablePath(p.OS, p.Arch) {
			t.Fatalf("executable %s = %q", key, m.Server.Executables[key])
		}
	}
	if m.Webapp == nil || m.Webapp.BundlePath != BundlePath || m.Webapp.StylePath != StylePath {
		t.Fatalf("webapp = %+v", m.Webapp)
	}
	if len(m.Webapp.Chunks) != 2 || m.Webapp.Chunks["search"] == "" || m.Webapp.Chunks["dashboard"] == "" {
		t.Fatalf("chunks = %v", m.Webapp.Chunks)
	}
	if len(m.Capabilities) != 1 || m.Capabilities[0] != protocol.CapabilityHTTP {
		t.Fatalf("capabilities = %v", m.Capabilities)
	}
	if m.HTTP == nil || m.HTTP.Listen != "unix" {
		t.Fatalf("http = %+v", m.HTTP)
	}
	want := map[string]bool{protocol.PermissionLogFiles: true, protocol.PermissionNetwork: true, protocol.PermissionKV: true}
	if len(m.Permissions) != len(want) {
		t.Fatalf("permissions = %v", m.Permissions)
	}
	for _, permission := range m.Permissions {
		if !want[permission] {
			t.Fatalf("unexpected permission %q", permission)
		}
	}
	if len(m.Events) != 1 || m.Events[0] != protocol.EventLogPathsChanged {
		t.Fatalf("events = %v", m.Events)
	}
	if m.SettingsSchema == nil || len(m.SettingsSchema.Settings) != 3 {
		t.Fatalf("settings schema = %+v", m.SettingsSchema)
	}
	keys := map[string]bool{}
	for _, field := range m.SettingsSchema.Settings {
		keys[field.Key] = true
	}
	for _, key := range []string{"incremental_index_interval", "max_concurrent_index_tasks", "index_custom_mmdb"} {
		if !keys[key] {
			t.Fatalf("settings schema misses %s", key)
		}
	}
}

func TestFilterPlatformKeepsOnlyOneExecutable(t *testing.T) {
	full, err := Render()
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	for _, p := range platforms {
		key := p.OS + "-" + p.Arch
		narrowed, err := FilterPlatform(full, key)
		if err != nil {
			t.Fatalf("filter %s: %v", key, err)
		}

		var m protocol.Manifest
		if err := json.Unmarshal(narrowed, &m); err != nil {
			t.Fatalf("unmarshal %s: %v", key, err)
		}
		if len(m.Server.Executables) != 1 || m.Server.Executables[key] != ExecutablePath(p.OS, p.Arch) {
			t.Fatalf("executables for %s = %v", key, m.Server.Executables)
		}

		// Everything but the executables map survives byte for byte.
		restored, err := restoreExecutables(narrowed)
		if err != nil {
			t.Fatalf("restore %s: %v", key, err)
		}
		if !bytes.Equal(restored, full) {
			t.Fatalf("the %s manifest differs from plugin.json beyond server.executables", key)
		}
	}

	if _, err := FilterPlatform(full, "plan9-386"); err == nil {
		t.Fatal("an undeclared platform must be refused")
	}
}

// restoreExecutables puts the full executables map back into a narrowed
// manifest, so the rest of the document can be compared.
func restoreExecutables(data []byte) ([]byte, error) {
	var m protocol.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	m.Server.Executables = make(map[string]string, len(platforms))
	for _, p := range platforms {
		m.Server.Executables[p.OS+"-"+p.Arch] = ExecutablePath(p.OS, p.Arch)
	}
	return encode(&m)
}

// The webapp build writes the paths of its bundle in a fragment. The manifest
// has to name the same files.
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
		Chunks     map[string]string `json:"chunks"`
	}
	if err := json.Unmarshal(data, &fragment); err != nil {
		t.Fatalf("decode the fragment: %v", err)
	}

	if fragment.BundlePath != BundlePath || fragment.StylePath != StylePath {
		t.Fatalf("bundle %q, style %q, the webapp built %q and %q", BundlePath, StylePath, fragment.BundlePath, fragment.StylePath)
	}
	if len(fragment.Chunks) != len(chunks) {
		t.Fatalf("chunks %v, the webapp built %v", chunks, fragment.Chunks)
	}
	for name, path := range chunks {
		if fragment.Chunks[name] != path {
			t.Fatalf("chunk %s is %q, the webapp built %q", name, path, fragment.Chunks[name])
		}
	}
	for _, path := range []string{BundlePath, StylePath, IconPath, chunks["search"], chunks["dashboard"]} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatalf("%s is in the manifest but not built: %v", path, err)
		}
	}
}
