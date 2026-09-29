package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/config"
	"github.com/nginxui/plugin-log-analytics/internal/service"
	"github.com/nginxui/plugin-log-analytics/internal/store"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// The perf harness times the HTTP handlers over a real dataset. It only runs
// when LOG_ANALYTICS_PERF_DATA points at a directory holding access.log,
// its rotations and meta.json.
//
//	LOG_ANALYTICS_PERF_DATA     dataset directory
//	LOG_ANALYTICS_PERF_INDEX    data dir that keeps the built index (default <data>/perf-index)
//	LOG_ANALYTICS_PERF_PROFILE  directory for pprof files and response digests
//	LOG_ANALYTICS_PERF_TAG      label for the output files (default "run")
//	LOG_ANALYTICS_PERF_ONLY     comma separated query names to run
//	LOG_ANALYTICS_PERF_RUNS     runs per phase (default 5)
//
//	go test -run TestPerfQueries -count=1 -timeout 60m ./internal/api
const perfDay = 86400

type perfMeta struct {
	FirstTS int64 `json:"first_ts"`
	LastTS  int64 `json:"last_ts"`
}

type perfQuery struct {
	name   string
	method string
	url    string
	body   func(i int) map[string]any
}

func perfEnvDir(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func perfDate(ts int64) string {
	return time.Unix(ts, 0).UTC().Format("2006-01-02")
}

func perfQueries(logPath string, meta perfMeta, topIP string) []perfQuery {
	end := meta.LastTS
	start30 := meta.FirstTS
	start7 := end - 7*perfDay
	search := func(name string, start int64, extra map[string]any) perfQuery {
		return perfQuery{name: name, method: http.MethodPost, url: "/search", body: func(i int) map[string]any {
			body := map[string]any{
				"log_path": logPath, "limit": 50, "offset": 0,
				"start_time": start, "end_time": end + int64(i)*60,
				"sort_by": "timestamp", "sort_order": "desc",
			}
			for k, v := range extra {
				body[k] = v
			}
			return body
		}}
	}
	dashboard := func(name string, start int64) perfQuery {
		return perfQuery{name: name, method: http.MethodPost, url: "/dashboard", body: func(i int) map[string]any {
			return map[string]any{
				"log_path":   logPath,
				"start_date": perfDate(start),
				"end_date":   perfDate(end + int64(i)*perfDay),
			}
		}}
	}
	geo := func(name string, start int64) perfQuery {
		return perfQuery{name: name, method: http.MethodPost, url: "/geo/world", body: func(i int) map[string]any {
			return map[string]any{"path": logPath, "start_time": start, "end_time": end + int64(i)*60}
		}}
	}
	return []perfQuery{
		search("search_time_30d", start30, nil),
		search("search_status_5xx_30d", start30, map[string]any{"status": []int{500, 502, 503}}),
		search("search_ip_top_30d", start30, map[string]any{"ip": topIP}),
		search("search_time_7d", start7, nil),
		search("search_sort_bytes_30d", start30, map[string]any{"sort_by": "bytes_sent"}),
		search("search_browser_chrome_7d", start7, map[string]any{"browser": "Chrome"}),
		dashboard("dashboard_7d", start7),
		dashboard("dashboard_30d", start30),
		geo("geo_world_30d", start30),
	}
}

func perfStartServices(t testing.TB) {
	t.Helper()
	service.StopServices()
	service.InitializeServices(context.Background())
	require.NotNil(t, service.GetIndexer())
}

// perfPrepareIndex indexes the dataset into dir once. A marker file records a
// finished build, so later runs reuse it.
func perfPrepareIndex(t *testing.T, dataDir, logPath string) {
	t.Helper()

	marker := filepath.Join(config.DataDir(), "perf-index.done")
	if _, err := os.Stat(marker); err == nil {
		return
	}

	perfStartServices(t)
	start := time.Now()
	end := service.BeginRound(false)
	counts, minTime, maxTime, err := service.GetIndexer().IndexLogGroupWithProgress(logPath, nil)
	require.NoError(t, err)
	var total uint64
	for _, count := range counts {
		total += count
	}
	require.NoError(t, service.GetLogFileManager().SaveIndexMetadata(logPath, total, time.Now(), time.Since(start), minTime, maxTime))
	end()
	t.Logf("indexed %d documents in %s", total, time.Since(start).Round(time.Millisecond))
	service.StopServices()

	require.NoError(t, os.WriteFile(marker, []byte(strconv.FormatUint(total, 10)), 0o600))
}

func perfDo(router http.Handler, q perfQuery, body map[string]any) (int, []byte, time.Duration) {
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(q.method, q.url, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	start := time.Now()
	router.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes(), time.Since(start)
}

func perfMedian(d []time.Duration) time.Duration {
	c := append([]time.Duration(nil), d...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}

// perfDigest reduces a response to a stable form: the timing field is dropped
// and the rest is hashed, next to the summary that is worth reading by eye.
func perfDigest(q perfQuery, raw []byte) map[string]any {
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return map[string]any{"error": string(raw)}
	}
	delete(doc, "took")
	stable, _ := json.Marshal(doc)
	sum := sha256.Sum256(stable)
	out := map[string]any{"sha256": hex.EncodeToString(sum[:8])}
	switch q.url {
	case "/search":
		out["total"] = doc["total"]
		out["summary"] = doc["summary"]
		if entries, ok := doc["entries"].([]any); ok {
			out["entries"] = len(entries)
		}
	case "/dashboard":
		out["summary"] = doc["summary"]
	default:
		if data, ok := doc["data"].([]any); ok {
			out["items"] = len(data)
		}
	}
	return out
}

func TestPerfQueries(t *testing.T) {
	dataDir := os.Getenv("LOG_ANALYTICS_PERF_DATA")
	if dataDir == "" {
		t.Skip("LOG_ANALYTICS_PERF_DATA is not set")
	}
	indexDir := perfEnvDir("LOG_ANALYTICS_PERF_INDEX", filepath.Join(dataDir, "perf-index"))
	profileDir := os.Getenv("LOG_ANALYTICS_PERF_PROFILE")
	tag := perfEnvDir("LOG_ANALYTICS_PERF_TAG", "run")
	runs := 5
	if v, err := strconv.Atoi(os.Getenv("LOG_ANALYTICS_PERF_RUNS")); err == nil && v > 0 {
		runs = v
	}
	only := map[string]bool{}
	for _, name := range splitCommaSeparated(os.Getenv("LOG_ANALYTICS_PERF_ONLY")) {
		only[name] = true
	}

	var meta perfMeta
	rawMeta, err := os.ReadFile(filepath.Join(dataDir, "meta.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(rawMeta, &meta))
	var keys struct {
		TopIP string `json:"top_ip"`
	}
	if rawKeys, err := os.ReadFile(filepath.Join(dataDir, "keys.json")); err == nil {
		_ = json.Unmarshal(rawKeys, &keys)
	}

	require.NoError(t, os.MkdirAll(indexDir, 0o755))
	config.Reset()
	config.SetDataDir(indexDir)
	t.Cleanup(config.Reset)
	_, err = store.Open(indexDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	logPath := filepath.Join(dataDir, "access.log")
	service.SetHostLogs([]utils.HostLog{{Path: logPath, Type: "access", Source: "default"}})
	t.Cleanup(func() {
		service.StopServices()
		service.SetHostLogs(nil)
	})

	perfPrepareIndex(t, dataDir, logPath)

	if profileDir != "" {
		require.NoError(t, os.MkdirAll(profileDir, 0o755))
	}

	router := NewRouter()
	digests := map[string]any{}
	var report bytes.Buffer

	for _, q := range perfQueries(logPath, meta, keys.TopIP) {
		if len(only) > 0 && !only[q.name] {
			continue
		}

		// Cold: fresh services, so the first request opens the shards.
		perfStartServices(t)
		code, raw, cold := perfDo(router, q, q.body(0))
		require.Equal(t, http.StatusOK, code, string(raw))
		digests[q.name] = perfDigest(q, raw)
		if profileDir != "" && q.url != "/search" {
			require.NoError(t, os.WriteFile(filepath.Join(profileDir, fmt.Sprintf("%s-%s.json", tag, q.name)), raw, 0o600))
		}

		// Warm, varied: the time range moves a little on each run, so no
		// result cache can answer it.
		var cpuFile *os.File
		if profileDir != "" {
			cpuFile, err = os.Create(filepath.Join(profileDir, fmt.Sprintf("%s-%s.cpu", tag, q.name)))
			require.NoError(t, err)
			require.NoError(t, pprof.StartCPUProfile(cpuFile))
		}
		var varied []time.Duration
		for i := 1; i <= runs; i++ {
			code, raw, d := perfDo(router, q, q.body(i))
			require.Equal(t, http.StatusOK, code, string(raw))
			varied = append(varied, d)
		}
		if cpuFile != nil {
			pprof.StopCPUProfile()
			_ = cpuFile.Close()
			runtime.GC()
			allocFile, err := os.Create(filepath.Join(profileDir, fmt.Sprintf("%s-%s.alloc", tag, q.name)))
			require.NoError(t, err)
			require.NoError(t, pprof.Lookup("allocs").WriteTo(allocFile, 0))
			_ = allocFile.Close()
		}

		// Warm, repeated: the same request again.
		var repeated []time.Duration
		for i := 0; i < runs; i++ {
			code, raw, d := perfDo(router, q, q.body(0))
			require.Equal(t, http.StatusOK, code, string(raw))
			repeated = append(repeated, d)
		}

		line := fmt.Sprintf("%-26s cold %8s   varied p50 %8s   repeat p50 %8s",
			q.name, cold.Round(time.Millisecond), perfMedian(varied).Round(time.Millisecond), perfMedian(repeated).Round(time.Millisecond))
		t.Log(line)
		fmt.Fprintln(&report, line)
	}

	if profileDir != "" {
		out, _ := json.MarshalIndent(digests, "", "  ")
		require.NoError(t, os.WriteFile(filepath.Join(profileDir, tag+"-responses.json"), out, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(profileDir, tag+"-timings.txt"), report.Bytes(), 0o600))
	}
}
