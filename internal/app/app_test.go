package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	sdk "github.com/nginxui/plugin-sdk-go"
	"github.com/nginxui/plugin-sdk-go/protocol"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/config"
	"github.com/nginxui/plugin-log-analytics/internal/service"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// fakeHost is the host side of the handshake, with the calls the app makes.
type fakeHost struct {
	mu         sync.Mutex
	settings   map[string]any
	logs       []protocol.HostLogFile
	activities []bool
	kv         map[string]string
	dataDir    string
}

func (f *fakeHost) Ready() bool { return true }

func (f *fakeHost) Info() sdk.Info { return sdk.Info{DataDir: f.dataDir} }

func (f *fakeHost) Settings() map[string]any { return f.settings }

func (f *fakeHost) LogsList(context.Context) ([]protocol.HostLogFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]protocol.HostLogFile(nil), f.logs...), nil
}

func (f *fakeHost) setLogs(logs ...protocol.HostLogFile) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.logs = logs
}

func (f *fakeHost) ActivitySet(_ context.Context, key, label string, active bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if key != ActivityKey || label != ActivityLabel {
		panic("unexpected activity " + key + " " + label)
	}
	f.activities = append(f.activities, active)
	return nil
}

func (f *fakeHost) activityLog() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.activities...)
}

func (f *fakeHost) KVGet(_ context.Context, key string, out any) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.kv[key]
	if ok {
		*(out.(*string)) = value
	}
	return ok, nil
}

func (f *fakeHost) KVSet(_ context.Context, key string, value any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kv[key] = value.(string)
	return nil
}

func (f *fakeHost) KVDelete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.kv, key)
	return nil
}

// shortDir returns a data directory whose socket path fits the limit of the
// platform, which a long TMPDIR would not.
func shortDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "la-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// serve exposes the HTTP handler the plugin hands to the SDK.
func serve(t *testing.T, plugin *App) string {
	t.Helper()

	handler := plugin.Plugin().HTTP
	require.NotNil(t, handler)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

func get(t *testing.T, base, path string) (int, string) {
	t.Helper()

	resp, err := http.Get(base + path)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp.StatusCode, string(body)
}

func newHost(t *testing.T, dataDir string) *fakeHost {
	t.Helper()

	logPath := filepath.Join(dataDir, "logs", "access.log")
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o755))
	require.NoError(t, os.WriteFile(logPath, []byte(`203.0.113.1 - - [12/Aug/2026:16:00:00 +0000] "GET / HTTP/1.1" 200 5 "-" "t"`+"\n"), 0o644))

	return &fakeHost{
		dataDir:  dataDir,
		settings: map[string]any{config.KeyIncrementalIndexInterval: float64(30)},
		logs:     []protocol.HostLogFile{{Path: logPath, Type: "access", Source: "default"}},
		kv:       map[string]string{},
	}
}

func TestAPIAnswersBeforeTheServicesStart(t *testing.T) {
	plugin := New(shortDir(t))
	base := serve(t, plugin)

	// The services are not up yet: the list answers empty, queries say why not.
	status, body := get(t, base, "/logs/status")
	assert.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, `"items":[]`)

	status, body = get(t, base, "/preflight")
	assert.Equal(t, http.StatusInternalServerError, status)
	assert.Contains(t, body, `"code":50028`)

	status, _ = get(t, base, "/nothing-here")
	assert.Equal(t, http.StatusNotFound, status)
}

func TestPluginDeclaresTheHTTPCapabilityThroughTheSDK(t *testing.T) {
	plugin := New(shortDir(t)).Plugin()

	assert.NotNil(t, plugin.HTTP)
	assert.Empty(t, plugin.Capabilities, "the SDK derives the capability from the handler")
	assert.Contains(t, plugin.Events, protocol.EventLogPathsChanged)
}

func TestStartReadsTheHostAndFollowsItsEvents(t *testing.T) {
	dir := shortDir(t)
	host := newHost(t, dir)
	logPath := host.logs[0].Path

	plugin := New(dir)
	require.NoError(t, plugin.Start(host))
	t.Cleanup(func() { _ = plugin.Shutdown(context.Background()) })

	// Settings and the log list came from the host.
	assert.Equal(t, 30, config.Get().IncrementalIndexInterval)
	assert.Equal(t, dir, config.DataDir())
	assert.True(t, utils.IsValidLogPath(logPath))
	assert.Equal(t, logPath, utils.DefaultAccessLogPath())

	base := serve(t, plugin)
	status, body := get(t, base, "/logs/status")
	require.Equal(t, http.StatusOK, status)
	assert.Contains(t, body, logPath)

	// The host lists a second log and says so.
	second := filepath.Join(dir, "logs", "site.log")
	host.setLogs(host.logs[0], protocol.HostLogFile{Path: second, Type: "access", Source: "config", ConfigFile: "/etc/nginx/sites/site"})
	handler := plugin.Plugin().Events[protocol.EventLogPathsChanged]
	require.NotNil(t, handler)
	handler(context.Background(), protocol.EventNotification{Type: protocol.EventLogPathsChanged})

	assert.True(t, utils.IsValidLogPath(second))
	_, body = get(t, base, "/logs/status")
	assert.Contains(t, body, second)

	// A saved setting reaches the plugin.
	require.NoError(t, plugin.Plugin().Configure(context.Background(), map[string]any{config.KeyIncrementalIndexInterval: float64(5)}))
	assert.Equal(t, 5, config.Get().IncrementalIndexInterval)
}

func TestIndexingShowsTheHostIndicator(t *testing.T) {
	dir := shortDir(t)
	host := newHost(t, dir)

	plugin := New(dir)
	require.NoError(t, plugin.Start(host))
	t.Cleanup(func() { _ = plugin.Shutdown(context.Background()) })

	end := service.BeginRound(true)
	end()

	assert.Equal(t, []bool{true, false}, host.activityLog())
}

func TestStartTakesOverAHandoff(t *testing.T) {
	dir := shortDir(t)
	host := newHost(t, dir)

	importDir := filepath.Join(dir, "import")
	require.NoError(t, os.MkdirAll(importDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(importDir, "nginx_log_indices.json"), []byte(`[]`), 0o644))

	plugin := New(dir)
	require.NoError(t, plugin.Start(host))
	t.Cleanup(func() { _ = plugin.Shutdown(context.Background()) })

	assert.NoDirExists(t, importDir, "the host reads the removal as a finished import")
}

func TestShutdownStopsTheServices(t *testing.T) {
	dir := shortDir(t)
	host := newHost(t, dir)

	plugin := New(dir)
	require.NoError(t, plugin.Start(host))

	require.NoError(t, plugin.Shutdown(context.Background()))

	assert.Nil(t, service.GetIndexer(), "the services are stopped")
	assert.False(t, host.activityLog()[len(host.activityLog())-1], "the indicator is cleared")

	// A second call is harmless.
	require.NoError(t, plugin.Shutdown(context.Background()))
	time.Sleep(10 * time.Millisecond)
}

func TestWaitForHostStartsWhenTheHostIsReady(t *testing.T) {
	dir := shortDir(t)
	host := newHost(t, dir)

	plugin := New(dir)
	t.Cleanup(func() { _ = plugin.Shutdown(context.Background()) })

	var mu sync.Mutex
	var current Host
	go plugin.WaitForHost(func() Host {
		mu.Lock()
		defer mu.Unlock()
		return current
	})

	time.Sleep(120 * time.Millisecond)
	assert.Nil(t, service.GetIndexer(), "nothing starts before the host is there")

	mu.Lock()
	current = host
	mu.Unlock()

	require.Eventually(t, func() bool { return service.GetIndexer() != nil }, 10*time.Second, 20*time.Millisecond)
}
