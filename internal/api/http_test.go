package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/apierr"
	"github.com/nginxui/plugin-log-analytics/internal/config"
	"github.com/nginxui/plugin-log-analytics/internal/hub"
	"github.com/nginxui/plugin-log-analytics/internal/indexer"
	"github.com/nginxui/plugin-log-analytics/internal/service"
	"github.com/nginxui/plugin-log-analytics/internal/store"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// httpEnv is a router over running services with one indexed log group.
type httpEnv struct {
	router  http.Handler
	logPath string
	dataDir string
}

func startHTTPEnv(t *testing.T, entries int) *httpEnv {
	t.Helper()

	dataDir := t.TempDir()
	setupRebuildTestEnvironment(t, dataDir)

	logsDir := filepath.Join(dataDir, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0o755))
	logPath := writeAccessLog(t, logsDir, "access.log", entries)
	service.SetHostLogs([]utils.HostLog{{Path: logPath, Type: "access", Source: "default"}})

	ctx, cancel := context.WithCancel(context.Background())
	service.StopServices()
	service.InitializeServices(ctx)
	t.Cleanup(func() {
		cancel()
		service.StopServices()
		service.SetHostLogs(nil)
	})
	require.NotNil(t, service.GetIndexer())

	end := service.BeginRound(false)
	counts, minTime, maxTime, err := service.GetIndexer().IndexLogGroupWithProgress(logPath, nil)
	require.NoError(t, err)
	var total uint64
	for _, count := range counts {
		total += count
	}
	require.NoError(t, service.GetLogFileManager().SaveIndexMetadata(logPath, total, time.Now(), time.Second, minTime, maxTime))
	end()

	return &httpEnv{router: NewRouter(), logPath: logPath, dataDir: dataDir}
}

func (e *httpEnv) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()

	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	e.router.ServeHTTP(recorder, req)
	return recorder
}

func decode[T any](t *testing.T, recorder *httptest.ResponseRecorder) T {
	t.Helper()

	var out T
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &out), recorder.Body.String())
	return out
}

func TestLogsStatusListsGroupsWithTheIndexState(t *testing.T) {
	env := startHTTPEnv(t, 12)

	recorder := env.do(t, http.MethodGet, "/logs/status", nil)
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	// The raw shape the webapp reads.
	var raw struct {
		Items []map[string]any `json:"items"`
		Sum   map[string]any   `json:"summary"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &raw))
	require.Len(t, raw.Items, 1)
	item := raw.Items[0]
	assert.Equal(t, env.logPath, item["path"])
	assert.Equal(t, env.logPath, item["main_log_path"])
	assert.Equal(t, "indexed", item["index_status"])
	assert.EqualValues(t, 12, item["document_count"])
	assert.Equal(t, true, item["has_timerange"])
	timeRange, ok := item["timerange"].(map[string]any)
	require.True(t, ok, "timerange is an object")
	assert.Positive(t, timeRange["start"])
	assert.GreaterOrEqual(t, timeRange["end"], timeRange["start"])
	for _, key := range []string{
		"type", "name", "config_file", "last_indexed", "last_size", "timerange_start", "timerange_end",
		"index_duration", "queue_position", "error_message", "error_time", "retry_count",
	} {
		assert.Contains(t, item, key)
	}

	assert.EqualValues(t, 1, raw.Sum["total_files"])
	assert.EqualValues(t, 1, raw.Sum["indexed_files"])
	assert.EqualValues(t, 0, raw.Sum["indexing_files"])
	assert.EqualValues(t, 12, raw.Sum["document_count"])

	// The list is read from the metadata, no shard opens for it.
	assert.Zero(t, len(service.GetIndexer().GetAllShards()))
}

func TestLogsStatusFilters(t *testing.T) {
	env := startHTTPEnv(t, 3)

	assert.Len(t, decode[LogStatusResponse](t, env.do(t, http.MethodGet, "/logs/status?type=access", nil)).Items, 1)
	assert.Empty(t, decode[LogStatusResponse](t, env.do(t, http.MethodGet, "/logs/status?type=error", nil)).Items)
	assert.Len(t, decode[LogStatusResponse](t, env.do(t, http.MethodGet, "/logs/status?indexed=true", nil)).Items, 1)
	assert.Empty(t, decode[LogStatusResponse](t, env.do(t, http.MethodGet, "/logs/status?indexed=false", nil)).Items)
	assert.Len(t, decode[LogStatusResponse](t, env.do(t, http.MethodGet, "/logs/status?path="+utils.EncodePathParam(env.logPath), nil)).Items, 1)
}

func TestSearchOpensTheShardsAndAnswersLikeTheHostDid(t *testing.T) {
	env := startHTTPEnv(t, 30)

	recorder := env.do(t, http.MethodPost, "/search", map[string]any{"log_path": env.logPath, "limit": 10})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())

	response := decode[AdvancedSearchResponseAPI](t, recorder)
	assert.EqualValues(t, 30, response.Total)
	assert.Len(t, response.Entries, 10)
	assert.Equal(t, 30, response.Summary.PV)
	assert.NotEmpty(t, response.Entries[0]["ip"])
	assert.Contains(t, response.Entries[0], "ip_location_label")
}

func TestQueryRoutesAnswer(t *testing.T) {
	env := startHTTPEnv(t, 15)
	now := time.Now().Unix()

	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodGet, "/preflight?log_path=" + env.logPath, nil},
		{http.MethodGet, "/entries?path=" + env.logPath + "&limit=5", nil},
		{http.MethodPost, "/analytics", map[string]any{"path": env.logPath, "start_time": 1, "end_time": now, "limit": 5}},
		{http.MethodPost, "/dashboard", map[string]any{"log_path": env.logPath, "start_date": "2026-08-01", "end_date": "2026-08-31"}},
		{http.MethodPost, "/geo/world", map[string]any{"path": env.logPath, "start_time": 1, "end_time": now}},
		{http.MethodPost, "/geo/china", map[string]any{"path": env.logPath, "start_time": 1, "end_time": now}},
		{http.MethodPost, "/geo/china/city", map[string]any{"path": env.logPath, "start_time": 1, "end_time": now, "province": "北京"}},
		{http.MethodPost, "/geo/stats", map[string]any{"path": env.logPath, "start_time": 1, "end_time": now}},
	} {
		recorder := env.do(t, tc.method, tc.path, tc.body)
		assert.Equal(t, http.StatusOK, recorder.Code, "%s %s: %s", tc.method, tc.path, recorder.Body.String())
	}
}

func TestPreflightReportsTheIndexedGroup(t *testing.T) {
	env := startHTTPEnv(t, 9)

	response := decode[PreflightResponse](t, env.do(t, http.MethodGet, "/preflight?log_path="+env.logPath, nil))
	assert.True(t, response.Available)
	assert.Equal(t, "indexed", response.IndexStatus)
	require.NotNil(t, response.TimeRange)
	require.NotNil(t, response.FileInfo)
	assert.True(t, response.FileInfo.Exists)

	// Without a path the default access log the host lists is used.
	response = decode[PreflightResponse](t, env.do(t, http.MethodGet, "/preflight", nil))
	assert.Equal(t, "indexed", response.IndexStatus)
}

func TestQueryOnAPathTheHostDoesNotListIsRefused(t *testing.T) {
	env := startHTTPEnv(t, 3)

	recorder := env.do(t, http.MethodPost, "/search", map[string]any{"log_path": "/etc/passwd"})
	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.Contains(t, recorder.Body.String(), "not under whitelist")
}

func TestQueryBeforeTheServicesStartAnswersWithTheCodedError(t *testing.T) {
	service.StopServices()
	router := NewRouter()

	req := httptest.NewRequest(http.MethodPost, "/search", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	body := decode[apierr.Error](t, recorder)
	assert.Equal(t, int32(50028), body.Code)
	assert.Equal(t, "nginx_log", body.Scope)
	assert.Equal(t, "modern indexer service not available", body.Message)
}

func TestBadBodyIsRefusedLikeTheHostDid(t *testing.T) {
	env := startHTTPEnv(t, 2)

	req := httptest.NewRequest(http.MethodPost, "/search", bytes.NewReader([]byte(`{not json`)))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	env.router.ServeHTTP(recorder, req)

	assert.Equal(t, http.StatusNotAcceptable, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"scope":"validate"`)
}

func TestWarmAnswersAtOnceAndOpensTheShards(t *testing.T) {
	env := startHTTPEnv(t, 5)
	require.Zero(t, len(service.GetIndexer().GetAllShards()))

	recorder := env.do(t, http.MethodPost, "/warm", nil)
	assert.Equal(t, http.StatusAccepted, recorder.Code)

	require.Eventually(t, func() bool { return service.GetSearcher() != nil }, 10*time.Second, 20*time.Millisecond)
	assert.Positive(t, len(service.GetIndexer().GetAllShards()))
}

func TestRebuildRefusesAnUnlistedPathAndAnIndexRunning(t *testing.T) {
	env := startHTTPEnv(t, 5)

	recorder := env.do(t, http.MethodPost, "/index/rebuild", map[string]any{"path": "/etc/passwd"})
	assert.Equal(t, http.StatusInternalServerError, recorder.Code)
	assert.EqualValues(t, 50015, decode[apierr.Error](t, recorder).Code)

	end := service.BeginRound(true)
	defer end()
	recorder = env.do(t, http.MethodPost, "/index/rebuild", map[string]any{})
	assert.EqualValues(t, 50018, decode[apierr.Error](t, recorder).Code, "a rebuild while indexing runs is refused")
}

func TestRebuildOfOneGroupIndexesItAgain(t *testing.T) {
	env := startHTTPEnv(t, 8)
	// More lines arrive before the rebuild.
	f, err := os.OpenFile(env.logPath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(`203.0.113.250 - - [12/Aug/2026:17:00:00 +0000] "GET /late HTTP/1.1" 200 5 "-" "late"` + "\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	recorder := env.do(t, http.MethodPost, "/index/rebuild", map[string]any{"path": env.logPath})
	require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
	assert.Equal(t, "started", decode[IndexRebuildResponse](t, recorder).Status)

	require.Eventually(t, func() bool {
		return !service.Processing().Indexing()
	}, 30*time.Second, 50*time.Millisecond)

	db, err := store.DB()
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		var row store.NginxLogIndex
		if err := db.Where("path = ?", env.logPath).First(&row).Error; err != nil {
			return false
		}
		return row.DocumentCount == 9 && row.IndexStatus == string(indexer.IndexStatusIndexed)
	}, 20*time.Second, 100*time.Millisecond)
}

func TestGeoBoundaryFileIsServedFromTheMapDirectory(t *testing.T) {
	env := startHTTPEnv(t, 1)

	mapsDir := filepath.Join(env.dataDir, "maps")
	require.NoError(t, os.MkdirAll(mapsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(mapsDir, "110000_full.json"), []byte(`{"type":"FeatureCollection"}`), 0o644))

	recorder := env.do(t, http.MethodGet, "/geo/boundary/110000_full.json", nil)
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
	assert.JSONEq(t, `{"type":"FeatureCollection"}`, recorder.Body.String())

	assert.Equal(t, http.StatusNotFound, env.do(t, http.MethodGet, "/geo/boundary/120000_full.json", nil).Code)
	assert.Equal(t, http.StatusNotFound, env.do(t, http.MethodGet, "/geo/boundary/..%2Fpasswd", nil).Code, "an encoded slash never reaches the handler")
	assert.Equal(t, http.StatusBadRequest, env.do(t, http.MethodGet, "/geo/boundary/evil.json", nil).Code)

	// A configured folder is used instead.
	custom := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(custom, "330000_full.json"), []byte(`{}`), 0o644))
	config.Set(config.Settings{GeoMapPath: custom})
	assert.Equal(t, http.StatusOK, env.do(t, http.MethodGet, "/geo/boundary/330000_full.json", nil).Code)
}

func TestGeoLiteStatus(t *testing.T) {
	env := startHTTPEnv(t, 1)

	status := decode[StatusResp](t, env.do(t, http.MethodGet, "/geolite/status", nil))
	assert.False(t, status.Exists)
	assert.Equal(t, filepath.Join(env.dataDir, "geolite", "GeoLite2-City.mmdb"), status.Path)

	require.NoError(t, os.MkdirAll(filepath.Join(env.dataDir, "geolite"), 0o755))
	require.NoError(t, os.WriteFile(status.Path, []byte("mmdb"), 0o644))

	status = decode[StatusResp](t, env.do(t, http.MethodGet, "/geolite/status", nil))
	assert.True(t, status.Exists)
	assert.EqualValues(t, 4, status.Size)
	assert.NotEmpty(t, status.LastModified)
}

func TestEventsSocketStreamsTheEventsTheHostBusCarried(t *testing.T) {
	env := startHTTPEnv(t, 1)

	server := httptest.NewServer(env.router)
	defer server.Close()

	url := "ws" + server.URL[len("http"):] + "/events"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	require.NoError(t, err)
	defer conn.Close()

	read := func() map[string]any {
		t.Helper()
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
		var message map[string]any
		require.NoError(t, conn.ReadJSON(&message))
		return message
	}

	// The current status comes first.
	first := read()
	assert.Equal(t, hub.TypeProcessingStatus, first["type"])
	assert.Equal(t, map[string]any{"nginx_log_indexing": false}, first["data"])

	service.Events().Publish(hub.Event{Type: hub.TypeNginxLogIndexProgress, Data: hub.NginxLogIndexProgressData{
		LogPath: "/var/log/nginx/access.log", Progress: 42.5, Stage: "indexing", Status: "running", ElapsedTime: 1500, EstimatedRemain: 2000,
	}})
	progress := read()
	assert.Equal(t, "nginx_log_index_progress", progress["type"])
	assert.Equal(t, map[string]any{
		"log_path": "/var/log/nginx/access.log", "progress": 42.5, "stage": "indexing", "status": "running",
		"elapsed_time": 1500.0, "estimated_remain": 2000.0,
	}, progress["data"])

	service.Events().Publish(hub.Event{Type: hub.TypeNginxLogIndexComplete, Data: hub.NginxLogIndexCompleteData{
		LogPath: "/var/log/nginx/access.log", Success: true, Duration: 900, TotalLines: 10, IndexedSize: 2048,
	}})
	complete := read()
	assert.Equal(t, "nginx_log_index_complete", complete["type"])
	assert.Equal(t, true, complete["data"].(map[string]any)["success"])

	service.Events().Publish(hub.Event{Type: hub.TypeNginxLogIndexReady, Data: hub.NginxLogIndexReadyData{
		LogPath: "/var/log/nginx/access.log", StartTime: 1, EndTime: 2, Available: true, IndexStatus: "ready",
	}})
	assert.Equal(t, "nginx_log_index_ready", read()["type"])

	// A round that shows the indicator pushes the processing status.
	end := service.BeginRound(true)
	assert.Equal(t, map[string]any{"nginx_log_indexing": true}, read()["data"])
	end()
	assert.Equal(t, map[string]any{"nginx_log_indexing": false}, read()["data"])
}
