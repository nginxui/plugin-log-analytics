// Command manifest regenerates plugin.json.
//
//	go run ./cmd/manifest
//
// The generated file is committed, so the plugin can be packaged without
// running the generator.
//
// With -platform it writes the plugin.json of one per-platform package
// instead: the committed manifest with server.executables narrowed to that
// platform, which is what build.sh puts into each archive.
//
//	go run ./cmd/manifest -platform linux-amd64 -out dist/stage/linux-amd64/plugin.json
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/nginxui/plugin-sdk-go/protocol"
)

// Plugin identity. Version is the single source of truth for the release
// artifacts; build.sh reads it back from the generated plugin.json.
const (
	PluginID          = "com.nginxui.log-analytics"
	PluginName        = "Log Analytics"
	PluginVersion     = "1.0.0"
	PluginDescription = "Search nginx access logs with structured queries and see traffic on a dashboard with a visitor map."
	MinNginxUIVersion = "2.7.0"
	// RecommendedMemoryMB is the machine memory advised for the plugin.
	RecommendedMemoryMB = 512
)

// Package relative paths of the browser bundle. The entry and the styles are
// what @nginxui/plugin-sdk/vite writes, the chunks are built by the two extra
// configurations of the webapp.
const (
	BundlePath = "webapp/dist/main.js"
	StylePath  = "webapp/dist/style.css"
	IconPath   = "webapp/dist/icon.svg"
)

// chunks are the views the entry loads on demand.
var chunks = map[string]string{
	"search":    "webapp/dist/chunks/search.js",
	"dashboard": "webapp/dist/chunks/dashboard.js",
}

// translations are the name and description in every language the plugin
// ships besides English, keyed by host locale code.
var translations = map[string]protocol.ManifestI18n{
	"zh_CN": {
		Name:        "日志分析",
		Description: "对 Nginx 访问日志做结构化搜索，在面板和访客地图上查看流量。",
	},
	"zh_TW": {
		Name:        "日誌分析",
		Description: "對 Nginx 存取日誌做結構化搜尋，在面板和訪客地圖上檢視流量。",
	},
	"ja_JP": {
		Name:        "ログ分析",
		Description: "Nginx のアクセスログを構造化検索し、ダッシュボードと訪問者マップでトラフィックを確認します。",
	},
}

// platforms are the targets build.sh cross compiles, in manifest order.
var platforms = []struct{ OS, Arch string }{
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"darwin", "amd64"},
	{"darwin", "arm64"},
	{"windows", "amd64"},
	{"windows", "arm64"},
}

// ExecutablePath returns the packaged path of one platform's binary.
func ExecutablePath(goos, goarch string) string {
	name := fmt.Sprintf("log-analytics-%s-%s", goos, goarch)
	if goos == "windows" {
		name += ".exe"
	}
	return "server/dist/" + name
}

// Build assembles the manifest. It is deterministic: every map is encoded in
// key order.
func Build() (*protocol.Manifest, error) {
	executables := make(map[string]string, len(platforms))
	for _, p := range platforms {
		executables[p.OS+"-"+p.Arch] = ExecutablePath(p.OS, p.Arch)
	}

	root, err := repoRoot()
	if err != nil {
		return nil, err
	}
	shared, err := webappSharedRanges(root)
	if err != nil {
		return nil, err
	}

	return &protocol.Manifest{
		ID:                PluginID,
		Name:              PluginName,
		Version:           PluginVersion,
		Description:       PluginDescription,
		APIVersion:        protocol.APIVersion,
		MinNginxUIVersion: MinNginxUIVersion,
		I18n:              translations,
		Server: &protocol.ManifestServer{
			Executables: executables,
			// The plugin indexes on a schedule, so it stays up.
			Lifecycle: protocol.LifecycleResident,
			// Indexing a large log with nginx-ui on the same machine
			// needs about this much.
			Resources: &protocol.ManifestResources{RecommendedMemoryMB: RecommendedMemoryMB},
		},
		IconPath: IconPath,
		Webapp: &protocol.ManifestWebapp{
			BundlePath: BundlePath,
			StylePath:  StylePath,
			Shared:     shared,
			Chunks:     chunks,
		},
		Capabilities: []string{protocol.CapabilityHTTP},
		Permissions: []string{
			// The nginx log files, and the event when their set changes.
			protocol.PermissionLogFiles,
			// The download of the IP location database.
			protocol.PermissionNetwork,
			// Where the index is kept when it stays outside the data directory.
			protocol.PermissionKV,
		},
		// The other implementation of the same pages and routes.
		Conflicts:    []string{"com.nginxui.log-analytics-rs"},
		Events:       []string{protocol.EventLogPathsChanged},
		NetworkHosts: []string{"cloud.nginxui.com"},
		// A socket keeps websockets and streaming responses possible.
		HTTP: &protocol.ManifestHTTP{Listen: "unix"},
		SettingsSchema: &protocol.SettingsSchema{
			Settings: []protocol.SettingsField{
				{
					Key:         "incremental_index_interval",
					Type:        "number",
					DisplayName: "Indexing interval (minutes)",
					HelpText:    "How often the logs are checked for new lines. Zero or empty means 15 minutes.",
					Default:     15,
				},
				{
					Key:         "max_concurrent_index_tasks",
					Type:        "number",
					DisplayName: "Logs indexed at once",
					HelpText:    "The most log files indexed at the same time. A lower number uses less memory. Zero picks a value from the available CPUs.",
					Default:     0,
				},
				{
					Key:         "index_custom_mmdb",
					Type:        "text",
					DisplayName: "Custom IP location database",
					HelpText:    "Path of your own IP location database file. Empty uses the downloaded one.",
				},
				{
					Key:         "geo_map_path",
					Type:        "text",
					DisplayName: "Map files folder",
					HelpText:    "Folder with the map outline files. Empty uses the plugin's own folder. Files not found there are loaded online.",
				},
			},
		},
	}, nil
}

// webappManifestFragment is the shape @nginxui/plugin-sdk/vite writes to
// webapp/dist/manifest.webapp.json. Only Shared is consumed here: the bundle,
// style and chunk paths are fixed by the layout build.sh packages.
type webappManifestFragment struct {
	Shared map[string]string `json:"shared"`
}

// webappSharedRanges reads the semver ranges the webapp bundle was built
// against, if the bundle has been built. A missing file is not an error: the
// Go tests and cross compilation do not depend on the JS toolchain having run.
func webappSharedRanges(root string) (map[string]string, error) {
	path := filepath.Join(root, "webapp", "dist", "manifest.webapp.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var fragment webappManifestFragment
	if err := json.Unmarshal(data, &fragment); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return fragment.Shared, nil
}

// Render encodes the manifest the way it is committed: two space indentation
// and a trailing newline.
func Render() ([]byte, error) {
	manifest, err := Build()
	if err != nil {
		return nil, err
	}
	return encode(manifest)
}

// encode writes a manifest in the committed layout.
func encode(manifest *protocol.Manifest) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(manifest); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// FilterPlatform narrows a rendered manifest to one "<goos>-<goarch>"
// executable. A per-platform package must declare exactly the platform it
// ships, see the plugin spec PKG-12, while the catalog release keeps the full
// map in its manifest snapshot.
func FilterPlatform(data []byte, platform string) ([]byte, error) {
	var manifest protocol.Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if manifest.Server == nil {
		return nil, fmt.Errorf("the manifest has no server block")
	}
	executable, ok := manifest.Server.Executables[platform]
	if !ok {
		return nil, fmt.Errorf("the manifest declares no executable for %s", platform)
	}
	manifest.Server.Executables = map[string]string{platform: executable}
	return encode(&manifest)
}

// repoRoot resolves the module root from this file's location.
func repoRoot() (string, error) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("manifest: unable to locate the generator source")
	}
	return filepath.Abs(filepath.Join(filepath.Dir(file), "..", ".."))
}

func main() {
	platform := flag.String("platform", "", `write the manifest of the "<goos>-<goarch>" package instead of regenerating plugin.json`)
	in := flag.String("in", "", "manifest to narrow with -platform (default: the committed plugin.json)")
	out := flag.String("out", "", "output file (default: the committed plugin.json, required with -platform)")
	flag.Parse()

	if err := run(*platform, *in, *out); err != nil {
		fmt.Fprintln(os.Stderr, "manifest:", err)
		os.Exit(1)
	}
}

func run(platform, in, out string) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}

	var data []byte
	switch {
	case platform == "":
		if data, err = Render(); err != nil {
			return err
		}
		if out == "" {
			out = filepath.Join(root, "plugin.json")
		}
	case out == "":
		return fmt.Errorf("-platform needs -out, the committed plugin.json keeps every platform")
	default:
		if in == "" {
			in = filepath.Join(root, "plugin.json")
		}
		source, err := os.ReadFile(in)
		if err != nil {
			return err
		}
		if data, err = FilterPlatform(source, platform); err != nil {
			return err
		}
	}

	if err = os.WriteFile(out, data, 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %s (%d bytes)\n", out, len(data))
	return nil
}
