package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/indexer"
	"github.com/nginxui/plugin-log-analytics/internal/store"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// listLog makes the host "list" a log path for the duration of a test, which is
// what lets utils.IsValidLogPath accept it.
func listLog(t *testing.T, path string) {
	t.Helper()
	utils.SetHostLogs([]utils.HostLog{{Path: path, Type: "access", Source: "config"}})
	t.Cleanup(func() { utils.SetHostLogs(nil) })
}

// groupRows is a stored state for a group, keyed by path.
type groupRows []*store.NginxLogIndex

func (g groupRows) GetLogIndexesByGroup(string) ([]*store.NginxLogIndex, error) { return g, nil }

// trackedRow is the stored state of a file that was read up to its end.
func trackedRow(path string, info os.FileInfo) *store.NginxLogIndex {
	return &store.NginxLogIndex{
		Path:         path,
		MainLogPath:  path,
		LastModified: info.ModTime(),
		LastSize:     info.Size(),
		LastIndexed:  time.Now(),
		Fingerprint:  "0123456789abcdef0123456789abcdef",
		SyncVersion:  1,
	}
}

func TestNeedsIncrementalIndexingSkipsWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	listLog(t, logPath)
	require.NoError(t, os.WriteFile(logPath, []byte("initial\n"), 0o644))
	info, err := os.Stat(logPath)
	require.NoError(t, err)

	logData := &NginxLogWithIndex{Path: logPath, Type: "access", IndexStatus: string(indexer.IndexStatusIndexed)}
	assert.False(t, needsIncrementalIndexing(logData, groupRows{trackedRow(logPath, info)}),
		"expected no incremental indexing when file metadata is unchanged")
}

func TestNeedsIncrementalIndexingDetectsGrowth(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	listLog(t, logPath)
	require.NoError(t, os.WriteFile(logPath, []byte("initial\n"), 0o644))
	info, err := os.Stat(logPath)
	require.NoError(t, err)
	rows := groupRows{trackedRow(logPath, info)}

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString("more data\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	logData := &NginxLogWithIndex{Path: logPath, Type: "access", IndexStatus: string(indexer.IndexStatusIndexed)}
	assert.True(t, needsIncrementalIndexing(logData, rows), "expected incremental indexing when file grew")
}

func TestNeedsIncrementalIndexingDetectsAReplacedFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	listLog(t, logPath)
	require.NoError(t, os.WriteFile(logPath, []byte("a long first file with many lines\n"), 0o644))
	info, err := os.Stat(logPath)
	require.NoError(t, err)
	rows := groupRows{trackedRow(logPath, info)}

	// Rotation: a new file at the same path, smaller than the one before.
	require.NoError(t, os.Remove(logPath))
	require.NoError(t, os.WriteFile(logPath, []byte("new\n"), 0o644))

	logData := &NginxLogWithIndex{Path: logPath, Type: "access", IndexStatus: string(indexer.IndexStatusIndexed)}
	assert.True(t, needsIncrementalIndexing(logData, rows))
}

func TestNeedsIncrementalIndexingSeesARotatedFileThatWasNeverRead(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	listLog(t, logPath)
	require.NoError(t, os.WriteFile(logPath, []byte("live\n"), 0o644))
	require.NoError(t, os.WriteFile(logPath+".1", []byte("rotated\n"), 0o644))
	info, err := os.Stat(logPath)
	require.NoError(t, err)

	logData := &NginxLogWithIndex{Path: logPath, Type: "access", IndexStatus: string(indexer.IndexStatusIndexed)}
	assert.True(t, needsIncrementalIndexing(logData, groupRows{trackedRow(logPath, info)}),
		"the live file is unchanged, the rotated file has no state")
}

func TestNeedsIncrementalIndexingReadsFilesFromBeforeContentTracking(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	listLog(t, logPath)
	require.NoError(t, os.WriteFile(logPath, []byte("initial\n"), 0o644))
	info, err := os.Stat(logPath)
	require.NoError(t, err)

	row := trackedRow(logPath, info)
	row.SyncVersion = 0
	row.Fingerprint = ""
	logData := &NginxLogWithIndex{Path: logPath, Type: "access", IndexStatus: string(indexer.IndexStatusIndexed)}
	assert.True(t, needsIncrementalIndexing(logData, groupRows{row}))
}

func TestNeedsIncrementalIndexingIgnoresUnlistedFiles(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	require.NoError(t, os.WriteFile(logPath, []byte("initial\n"), 0o644))

	// The file exists but the host does not list it.
	utils.SetHostLogs(nil)

	logData := &NginxLogWithIndex{
		Path:        logPath,
		Type:        "access",
		IndexStatus: string(indexer.IndexStatusNotIndexed),
	}
	assert.False(t, needsIncrementalIndexing(logData, nil), "a file the host does not list must never be read")
}

// incrementalLines returns lines with ids that are unique across the test.
func incrementalLines(first, count int) string {
	var out strings.Builder
	base := time.Date(2026, time.August, 12, 16, 0, 0, 0, time.UTC)
	for i := first; i < first+count; i++ {
		fmt.Fprintf(&out, `198.51.100.%d - - [%s] "GET /rot/%d HTTP/1.1" 200 %d "-" "rotation-test"`+"\n",
			i%200+1, base.Add(time.Duration(i)*time.Second).Format("02/Jan/2006:15:04:05 -0700"), i, 100+i)
	}
	return out.String()
}

// TestIncrementalRoundsKeepEveryLineOnce runs the rounds of the scheduler over
// a group that is indexed, appended to and rotated, and counts the documents
// after each one.
func TestIncrementalRoundsKeepEveryLineOnce(t *testing.T) {
	env := startLifecycleEnv(t, 50)
	logPath := env.logPaths[0]
	logsDir := env.logsDir

	appendText := func(name, text string) {
		f, err := os.OpenFile(filepath.Join(logsDir, name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		require.NoError(t, err)
		_, err = f.WriteString(text)
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	round := func() uint64 {
		performIncrementalIndexing()
		release, err := AcquireQuery(context.Background())
		require.NoError(t, err)
		defer release()
		return totalHits(t, logPath)
	}

	// The first round after the group was indexed reads nothing twice.
	assert.Equal(t, uint64(50), round())

	appendText("access.log", incrementalLines(1000, 10))
	assert.Equal(t, uint64(60), round(), "append")

	// Rotation with lines that arrived after the last round.
	appendText("access.log", incrementalLines(2000, 5))
	require.NoError(t, os.Rename(logPath, logPath+".1"))
	appendText("access.log", incrementalLines(3000, 4))
	assert.Equal(t, uint64(69), round(), "rename rotation")

	assert.Equal(t, uint64(69), round(), "a round without changes")

	appendText("access.log", incrementalLines(4000, 3))
	assert.Equal(t, uint64(72), round(), "append after rotation")
}
