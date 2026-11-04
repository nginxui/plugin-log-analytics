package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/store"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// newStaleTestServices returns a running indexer and a log file manager on a
// throwaway data directory, with a directory for the log files.
func newStaleTestServices(t *testing.T) (*ParallelIndexer, *LogFileManager, string) {
	t.Helper()

	dir := t.TempDir()
	_, err := store.Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	config := DefaultIndexerConfig()
	config.IndexPath = filepath.Join(dir, "index")
	config.ShardCount = 1
	config.WorkerCount = 1

	pi := NewParallelIndexer(config, NewGroupedShardManager(config))
	require.NoError(t, pi.Start(context.Background()))
	t.Cleanup(func() { _ = pi.Stop() })

	lfm := NewLogFileManager()
	lfm.SetIndexer(pi)

	logDir := filepath.Join(dir, "logs")
	require.NoError(t, os.MkdirAll(logDir, 0o755))
	return pi, lfm, logDir
}

func staleAccessLogLines(first, count int) string {
	var lines strings.Builder
	base := time.Date(2026, time.August, 12, 16, 0, 0, 0, time.UTC)
	for i := first; i < first+count; i++ {
		fmt.Fprintf(&lines, `203.0.113.1 - - [%s] "GET /stale/%d HTTP/1.1" 200 10 "-" "stale-test"`+"\n",
			base.Add(time.Duration(i)*time.Second).Format("02/Jan/2006:15:04:05 -0700"), i)
	}
	return lines.String()
}

// indexGroup indexes every given file of a log group and records the metadata
// the way the indexing jobs do.
func indexGroup(t *testing.T, pi *ParallelIndexer, lfm *LogFileManager, mainPath string, files ...string) {
	t.Helper()
	for _, file := range files {
		docs, minTime, maxTime, err := pi.IndexSingleFileWithProgress(file, nil)
		require.NoError(t, err)
		require.NoError(t, lfm.SaveIndexMetadata(file, docs, time.Now(), time.Millisecond, minTime, maxTime))
	}
	require.Equal(t, mainPath, getMainLogPathFromFile(files[0]))
}

func listedGroups(lfm *LogFileManager) []string {
	var paths []string
	for _, log := range lfm.GetAllLogsWithIndexGrouped() {
		paths = append(paths, log.Path)
	}
	sort.Strings(paths)
	return paths
}

func groupRowCount(t *testing.T, mainPath string) int {
	t.Helper()
	rows, err := NewPersistenceManager(DefaultIncrementalConfig()).GetLogIndexesByGroup(mainPath)
	require.NoError(t, err)
	return len(rows)
}

func TestStaleLogGroupsAreHiddenAndRemoved(t *testing.T) {
	pi, lfm, logDir := newStaleTestServices(t)

	// Reading a file needs it on the list of the host. The manager keeps its own
	// list, which decides what counts as listed below.
	var hostLogs []utils.HostLog
	t.Cleanup(func() { utils.SetHostLogs(nil) })
	write := func(name string, first, count int) string {
		path := filepath.Join(logDir, name)
		require.NoError(t, os.WriteFile(path, []byte(staleAccessLogLines(first, count)), 0o600))
		hostLogs = append(hostLogs, utils.HostLog{Path: path, Type: "access", Source: "config"})
		utils.SetHostLogs(hostLogs)
		return path
	}

	// A deleted site: indexed with a rotated sibling, then no longer listed and
	// its files removed.
	deleted := write("deleted.access.log", 0, 3)
	deletedRotated := write("deleted.access.log.1", 10, 2)
	indexGroup(t, pi, lfm, deleted, deleted, deletedRotated)
	require.NoError(t, os.Remove(deleted))
	require.NoError(t, os.Remove(deletedRotated))

	// A listed log with a rotated file: everything must stay.
	live := write("live.access.log", 20, 2)
	liveRotated := write("live.access.log.1", 30, 2)
	lfm.AddLogPath(live, "access", filepath.Base(live), "/etc/nginx/sites-enabled/live.conf")
	indexGroup(t, pi, lfm, live, live, liveRotated)

	// A log the host no longer lists, but still on disk.
	unlisted := write("unlisted.access.log", 40, 1)
	indexGroup(t, pi, lfm, unlisted, unlisted)

	// A listed log whose file is temporarily missing.
	rotatedAway := write("rotated-away.access.log", 50, 1)
	lfm.AddLogPath(rotatedAway, "access", filepath.Base(rotatedAway), "/etc/nginx/sites-enabled/rotated.conf")
	indexGroup(t, pi, lfm, rotatedAway, rotatedAway)
	require.NoError(t, os.Remove(rotatedAway))

	require.Equal(t, []string{live, rotatedAway, unlisted}, listedGroups(lfm),
		"a group that is neither listed nor on disk must not be shown")

	stale, err := lfm.StaleLogGroups()
	require.NoError(t, err)
	require.Equal(t, []string{deleted}, stale)

	require.NoError(t, lfm.RemoveLogGroup(deleted))

	require.Zero(t, groupRowCount(t, deleted), "the metadata of the stale group must be deleted")
	count, err := pi.CountDocsByMainLogPath(deleted)
	require.NoError(t, err)
	require.Zero(t, count, "the documents of the stale group must be deleted")

	require.Equal(t, 2, groupRowCount(t, live), "a live group keeps its rotated files")
	count, err = pi.CountDocsByMainLogPath(live)
	require.NoError(t, err)
	require.EqualValues(t, 4, count)
	_, err = os.Stat(liveRotated)
	require.NoError(t, err, "log files are never deleted")

	require.Equal(t, 1, groupRowCount(t, unlisted))
	require.Equal(t, 1, groupRowCount(t, rotatedAway))
	require.Equal(t, []string{live, rotatedAway, unlisted}, listedGroups(lfm))
}

func TestStaleLogGroupsLeaveOutGroupsBeingIndexed(t *testing.T) {
	_, lfm, logDir := newStaleTestServices(t)
	persistence := lfm.GetPersistence()

	indexing := filepath.Join(logDir, "indexing.access.log")
	require.NoError(t, persistence.SetIndexStatus(indexing, string(IndexStatusIndexing), 0, ""))

	queued := filepath.Join(logDir, "queued.access.log")
	require.NoError(t, persistence.SetIndexStatus(queued, string(IndexStatusQueued), 0, ""))

	indexed := filepath.Join(logDir, "indexed.access.log")
	require.NoError(t, persistence.SetIndexStatus(indexed, string(IndexStatusIndexed), 0, ""))

	stale, err := lfm.StaleLogGroups()
	require.NoError(t, err)
	require.Equal(t, []string{indexed}, stale)
}
