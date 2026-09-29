package service

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/store"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// listLogFiles makes the host "list" the given log paths for the duration of a
// test, which is what lets utils.IsValidLogPath accept them.
func listLogFiles(t *testing.T, paths ...string) {
	t.Helper()

	logs := make([]utils.HostLog, 0, len(paths))
	for _, path := range paths {
		logs = append(logs, utils.HostLog{Path: path, Type: "access", Source: "config"})
	}
	utils.SetHostLogs(logs)
	t.Cleanup(func() { utils.SetHostLogs(nil) })
}

// listLogsInDir lists every file that exists in dir at the time of the call.
func listLogsInDir(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)

	var paths []string
	for _, entry := range entries {
		if entry.Type().IsRegular() {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	listLogFiles(t, paths...)
}

// useTestMetadataDB installs a fresh in-memory metadata database and restores
// the previous one when the test ends.
func useTestMetadataDB(t *testing.T) {
	t.Helper()

	db, err := store.OpenMemory()
	require.NoError(t, err)
	previous := store.Use(db)
	t.Cleanup(func() { store.Use(previous) })
}
