package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/config"
	"github.com/nginxui/plugin-log-analytics/internal/searcher"
	"github.com/nginxui/plugin-log-analytics/internal/store"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// lifecycleEnv is a running set of services on a throwaway data directory.
type lifecycleEnv struct {
	dataDir  string
	logsDir  string
	freed    *atomic.Int32
	logPaths []string
}

// startLifecycleEnv starts the services with one indexed log group and
// restores every global when the test ends. The group is indexed through a
// round, the way the scheduler does it.
func startLifecycleEnv(t *testing.T, entries int) *lifecycleEnv {
	t.Helper()

	config.Reset()
	dataDir := t.TempDir()
	config.SetDataDir(dataDir)
	_, err := store.Open(dataDir)
	require.NoError(t, err)

	logsDir := filepath.Join(dataDir, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0o755))
	logPath := filepath.Join(logsDir, "access.log")
	writeTestAccessLog(t, logPath, entries)
	SetHostLogs([]utils.HostLog{{Path: logPath, Type: "access", Source: "config"}})

	freed := &atomic.Int32{}
	previousFree := freeOSMemory
	freeOSMemory = func() { freed.Add(1) }
	previousIdle := IdleTimeout
	lifecycleMu.Lock()
	lastActivity = time.Now()
	lifecycleMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	StopServices()
	InitializeServices(ctx)
	require.NotNil(t, GetIndexer(), "the services did not start")

	t.Cleanup(func() {
		cancel()
		StopServices()
		SetHostLogs(nil)
		freeOSMemory = previousFree
		IdleTimeout = previousIdle
		_ = store.Close()
		config.Reset()
	})

	env := &lifecycleEnv{dataDir: dataDir, logsDir: logsDir, freed: freed, logPaths: []string{logPath}}
	env.index(t, logPath)
	return env
}

// index indexes one log group inside a round and records its metadata.
func (e *lifecycleEnv) index(t *testing.T, logPath string) {
	t.Helper()

	end := BeginRound(false)
	defer end()

	idx := GetIndexer()
	counts, minTime, maxTime, err := idx.IndexLogGroupWithProgress(logPath, nil)
	require.NoError(t, err)

	var total uint64
	for _, count := range counts {
		total += count
	}
	require.NoError(t, GetLogFileManager().SaveIndexMetadata(logPath, total, time.Now(), time.Second, minTime, maxTime))
}

func writeTestAccessLog(t *testing.T, path string, entries int) {
	t.Helper()

	var contents strings.Builder
	base := time.Date(2026, time.August, 12, 16, 0, 0, 0, time.UTC)
	for i := 0; i < entries; i++ {
		fmt.Fprintf(&contents,
			`203.0.113.%d - - [%s] "GET /lifecycle/%d HTTP/1.1" 200 %d "-" "lifecycle-test"`+"\n",
			i%200+1, base.Add(time.Duration(i)*time.Second).Format("02/Jan/2006:15:04:05 -0700"), i, 100+i)
	}
	require.NoError(t, os.WriteFile(path, []byte(contents.String()), 0o644))
}

func openShards() int {
	if idx := GetIndexer(); idx != nil {
		return len(idx.GetAllShards())
	}
	return 0
}

func totalHits(t *testing.T, logPath string) uint64 {
	t.Helper()

	search := GetSearcher()
	require.NotNil(t, search, "no searcher while the shards are supposed to be loaded")
	result, err := search.Search(context.Background(), &searcher.SearchRequest{
		LogPaths: []string{logPath}, UseMainLogPath: true, Limit: 1,
	})
	require.NoError(t, err)
	return result.TotalHits
}

func TestNothingOpensAtStartAndTheRoundReleasesWhatItOpened(t *testing.T) {
	env := startLifecycleEnv(t, 50)

	assert.Nil(t, GetSearcher(), "no searcher before a query asks for one")
	assert.Nil(t, GetAnalytics())
	assert.Zero(t, openShards(), "the shards an indexing round opened are closed when it ends")
	assert.False(t, shardsLoaded.Load())
	assert.Positive(t, env.freed.Load(), "the memory goes back to the system after a round")

	// The metadata survived, so the list can show the group without any shard.
	logs := GetAllLogsWithIndexGrouped()
	require.Len(t, logs, 1)
	assert.Equal(t, uint64(50), logs[0].DocumentCount)
}

func TestFirstQueryOpensTheShardsAndTheIndexIsIntact(t *testing.T) {
	env := startLifecycleEnv(t, 50)

	release, err := AcquireQuery(context.Background())
	require.NoError(t, err)
	defer release()

	assert.True(t, shardsLoaded.Load())
	assert.Positive(t, openShards())
	assert.NotNil(t, GetAnalytics())
	assert.Equal(t, uint64(50), totalHits(t, env.logPaths[0]))
}

func TestConcurrentQueriesLoadOnce(t *testing.T) {
	startLifecycleEnv(t, 20)

	done := make(chan error, 8)
	for range 8 {
		go func() {
			release, err := AcquireQuery(context.Background())
			if err == nil {
				release()
			}
			done <- err
		}()
	}
	for range 8 {
		require.NoError(t, <-done)
	}
	assert.True(t, shardsLoaded.Load())
}

func TestIdleReleaseClosesShardsAndDropsCaches(t *testing.T) {
	env := startLifecycleEnv(t, 30)
	IdleTimeout = 50 * time.Millisecond

	release, err := AcquireQuery(context.Background())
	require.NoError(t, err)
	require.Equal(t, uint64(30), totalHits(t, env.logPaths[0]))

	// A running query keeps the shards, however long the timeout passed.
	time.Sleep(80 * time.Millisecond)
	assert.False(t, releaseIfIdle(), "a query is still running")
	assert.True(t, shardsLoaded.Load())

	release()
	assert.False(t, releaseIfIdle(), "the query just ended, the idle time starts now")

	time.Sleep(80 * time.Millisecond)
	before := env.freed.Load()
	assert.True(t, releaseIfIdle())
	assert.False(t, shardsLoaded.Load())
	assert.Zero(t, openShards())
	assert.Nil(t, GetSearcher(), "the query caches go with the searcher")
	assert.Nil(t, GetAnalytics())
	assert.Greater(t, env.freed.Load(), before, "the release hands memory back")

	// The next query opens everything again and finds the same data.
	release, err = AcquireQuery(context.Background())
	require.NoError(t, err)
	defer release()
	assert.Equal(t, uint64(30), totalHits(t, env.logPaths[0]))
}

func TestIdleReleaseWaitsForIndexing(t *testing.T) {
	startLifecycleEnv(t, 10)
	IdleTimeout = time.Nanosecond

	release, err := AcquireQuery(context.Background())
	require.NoError(t, err)
	release()

	end := BeginRound(false)
	time.Sleep(5 * time.Millisecond)
	assert.False(t, releaseIfIdle(), "an indexing round is running")
	end()

	assert.True(t, shardsLoaded.Load(), "a round that ends does not close shards a query opened")
	time.Sleep(5 * time.Millisecond)
	assert.True(t, releaseIfIdle())
}

func TestWarmOpensTheShardsInTheBackground(t *testing.T) {
	startLifecycleEnv(t, 10)

	Warm()

	require.Eventually(t, shardsLoaded.Load, 10*time.Second, 20*time.Millisecond)
	assert.NotNil(t, GetSearcher())
}

func TestRoundKeepsTheIndicatorUntilTheLastOneEnds(t *testing.T) {
	startLifecycleEnv(t, 5)

	var changes []bool
	SetActivityHook(func(indexing bool) { changes = append(changes, indexing) })
	t.Cleanup(func() { SetActivityHook(nil) })

	first := BeginRound(true)
	second := BeginRound(true)
	quiet := BeginRound(false)
	assert.True(t, Processing().Indexing())

	first()
	assert.True(t, Processing().Indexing(), "another round still shows the indicator")
	second()
	assert.False(t, Processing().Indexing())
	quiet()

	assert.Equal(t, []bool{true, false}, changes, "the indicator flips once per stretch of indexing")
}

func TestRoundWithoutIndicatorLeavesItOff(t *testing.T) {
	startLifecycleEnv(t, 5)

	end := BeginRound(false)
	defer end()
	assert.False(t, Processing().Indexing(), "the periodic incremental round stays silent")
}

func TestSetHostLogsReplacesTheList(t *testing.T) {
	startLifecycleEnv(t, 5)

	other := filepath.Join(t.TempDir(), "other.log")
	SetHostLogs([]utils.HostLog{{Path: other, Type: "access", Source: "default"}})

	paths := GetAllLogPaths()
	require.Len(t, paths, 1)
	assert.Equal(t, other, paths[0].Path)
	assert.Equal(t, other, utils.DefaultAccessLogPath())
	assert.False(t, utils.IsValidLogPath(filepath.Join(config.DataDir(), "logs", "access.log")), "a log the host stopped listing is no longer readable")
}

func TestExpandLogGroupPathOnlyReturnsListedGroups(t *testing.T) {
	dir := t.TempDir()
	access := filepath.Join(dir, "access.log")
	for _, name := range []string{"access.log", "access.log.1", "access.log.2.gz", "access.log.tmp", "other.log"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644))
	}
	utils.SetHostLogs([]utils.HostLog{{Path: access, Type: "access", Source: "config"}})
	t.Cleanup(func() { utils.SetHostLogs(nil) })

	files, err := ExpandLogGroupPath(access)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{
		access,
		filepath.Join(dir, "access.log.1"),
		filepath.Join(dir, "access.log.2.gz"),
	}, files, "the stray sibling and the unlisted log are not part of the group")
}

func TestMemoryGoesBackOnceWhenTheLastRoundEnds(t *testing.T) {
	env := startLifecycleEnv(t, 20)

	// Keep the shards loaded so the round end takes the plain free path.
	release, err := AcquireQuery(context.Background())
	require.NoError(t, err)
	defer release()

	before := env.freed.Load()
	first := BeginRound(false)
	second := BeginRound(false)

	first()
	assert.Equal(t, before, env.freed.Load(), "another round is still running")

	second()
	assert.Equal(t, before+1, env.freed.Load(), "the last round hands the memory back once")

	second()
	assert.Equal(t, before+1, env.freed.Load(), "ending a round twice changes nothing")
}
