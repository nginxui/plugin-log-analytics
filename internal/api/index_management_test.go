package api

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

	"github.com/nginxui/plugin-log-analytics/internal/config"
	"github.com/nginxui/plugin-log-analytics/internal/indexer"
	"github.com/nginxui/plugin-log-analytics/internal/service"
	"github.com/nginxui/plugin-log-analytics/internal/store"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// TestRebuildAllFilesDiscoversLogGroups is the regression test for the
// follow-up on issue #1787.
//
// The reporter turned advanced indexing off and on again and then triggered a
// full rebuild. Stopping the services dropped the LogFileManager holding every
// known log path, starting them created a new empty one, and the rebuild cleared
// the index metadata before enumerating the log groups. Both sources of log
// groups were therefore empty, and the rebuild logged a successful completion
// right after "Processing 0 log groups".
//
// In the plugin the paths come from the host list, which lives outside the
// manager. The subtests share one metadata database on purpose: the services
// install background goroutines, so swapping the global database handle between
// scenarios would race with them.
func TestRebuildAllFilesDiscoversLogGroups(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping index rebuild integration test in short mode")
	}

	tempDir := t.TempDir()
	logsDir := filepath.Join(tempDir, "logs")
	require.NoError(t, os.MkdirAll(logsDir, 0o755))

	setupRebuildTestEnvironment(t, tempDir)

	service.StopServices()
	t.Cleanup(service.StopServices)

	// The nginx configuration declares no access_log at all and the log file is
	// only known as the nginx default access log the host lists.
	t.Run("group discovered from the nginx default access log", func(t *testing.T) {
		logPath := writeAccessLog(t, logsDir, "default-access.log", 3)
		service.StopServices()

		service.SetHostLogs(nil)
		require.Empty(t, service.GetAllLogPaths(), "the host lists nothing, so nothing is registered yet")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		service.SetHostLogs([]utils.HostLog{{Path: logPath, Type: "access", Source: "default"}})
		t.Cleanup(func() { service.SetHostLogs(nil) })
		service.InitializeServices(ctx)

		modernIndexer := service.GetIndexer()
		require.NotNil(t, modernIndexer)
		logFileManager := service.GetLogFileManager()
		require.NotNil(t, logFileManager)
		require.NoError(t, logFileManager.DeleteAllIndexMetadata())

		require.Equal(t, []string{logPath}, groupedPaths(service.GetAllLogsWithIndexGrouped()),
			"the nginx default access log must be listed as a log group")

		rebuildAllFiles(modernIndexer, logFileManager, nil)

		assertIndexedGroup(t, logPath, 3)
		service.StopServices()
	})

	// A log group known only from the host list must be rebuilt even after the
	// plugin services have been stopped and started again.
	t.Run("group discovered from the host list", func(t *testing.T) {
		logPath := writeAccessLog(t, logsDir, "config-discovered.log", 4)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		service.StopServices()
		service.InitializeServices(ctx)
		service.SetHostLogs([]utils.HostLog{{
			Path: logPath, Type: "access", Source: "config", ConfigFile: filepath.Join(tempDir, "nginx.conf"),
		}})
		t.Cleanup(func() { service.SetHostLogs(nil) })
		require.NotEmpty(t, service.GetAllLogsWithIndexGrouped())

		// Stop and start again, then rebuild without waiting for the host to
		// list the logs once more.
		service.StopServices()
		service.InitializeServices(ctx)

		modernIndexer := service.GetIndexer()
		require.NotNil(t, modernIndexer)
		logFileManager := service.GetLogFileManager()
		require.NotNil(t, logFileManager)

		// No metadata at all, so the log group can only come from the host list.
		// This is the state the rebuild used to create for itself by deleting
		// all metadata before enumerating.
		require.NoError(t, logFileManager.DeleteAllIndexMetadata())

		rebuildAllFiles(modernIndexer, logFileManager, nil)

		assertIndexedGroup(t, logPath, 4)
		service.StopServices()
	})

	// A log group known only from previous index metadata must survive the
	// metadata wipe performed by the rebuild itself.
	t.Run("group known only from index metadata", func(t *testing.T) {
		logPath := writeAccessLog(t, logsDir, "metadata-only.log", 2)
		service.SetHostLogs([]utils.HostLog{{Path: logPath, Type: "access", Source: "config"}})
		t.Cleanup(func() { service.SetHostLogs(nil) })

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		service.StopServices()
		service.InitializeServices(ctx)

		modernIndexer := service.GetIndexer()
		require.NotNil(t, modernIndexer)
		logFileManager := service.GetLogFileManager()
		require.NotNil(t, logFileManager)

		require.NoError(t, logFileManager.DeleteAllIndexMetadata())
		require.NoError(t, logFileManager.SaveIndexMetadata(logPath, 0, time.Now(), 0, nil, nil))

		rebuildAllFiles(modernIndexer, logFileManager, nil)

		assertIndexedGroup(t, logPath, 2)
		service.StopServices()
	})
}

// groupedPaths returns the paths of a grouped log list in a stable order.
func groupedPaths(logs []*service.NginxLogWithIndex) []string {
	paths := make([]string, 0, len(logs))
	for _, log := range logs {
		paths = append(paths, log.Path)
	}
	sort.Strings(paths)
	return paths
}

// assertIndexedGroup checks that a full rebuild produced an indexed record with
// the expected document count for the given log group.
func assertIndexedGroup(t *testing.T, logPath string, wantDocuments uint64) {
	t.Helper()

	db, err := store.DB()
	require.NoError(t, err)
	record := &store.NginxLogIndex{}
	err = db.Where("path = ?", logPath).First(record).Error
	require.NoError(t, err, "the full rebuild must have indexed %s", logPath)
	require.Equal(t, string(indexer.IndexStatusIndexed), record.IndexStatus)
	require.Equal(t, wantDocuments, record.DocumentCount)
}

// writeAccessLog creates an nginx access log with the requested number of
// parseable entries and returns its path.
func writeAccessLog(t *testing.T, logsDir, name string, entries int) string {
	t.Helper()

	var contents strings.Builder
	baseTime := time.Date(2026, time.August, 12, 16, 0, 0, 0, time.UTC)
	for i := 0; i < entries; i++ {
		_, err := fmt.Fprintf(&contents,
			`203.0.113.%d - - [%s] "GET /rebuild/%d HTTP/1.1" 200 %d "-" "rebuild-regression-test"`+"\n",
			i+1,
			baseTime.Add(time.Duration(i)*time.Second).Format("02/Jan/2006:15:04:05 -0700"),
			i,
			100+i,
		)
		require.NoError(t, err)
	}

	logPath := filepath.Join(logsDir, name)
	require.NoError(t, os.WriteFile(logPath, []byte(contents.String()), 0o600))
	return logPath
}

// setupRebuildTestEnvironment points the data directory and the metadata
// database at a throwaway directory and restores the globals afterwards.
func setupRebuildTestEnvironment(t *testing.T, tempDir string) {
	t.Helper()

	config.Reset()
	config.SetDataDir(tempDir)
	t.Cleanup(config.Reset)

	_, err := store.Open(tempDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
}
