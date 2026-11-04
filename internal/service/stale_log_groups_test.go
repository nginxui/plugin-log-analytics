package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// startWithDeletedSite starts the services with an indexed log group whose log
// file is gone and which the host no longer lists, next to a listed one.
func startWithDeletedSite(t *testing.T) (env *lifecycleEnv, deleted string) {
	t.Helper()

	env = startLifecycleEnv(t, 5)
	deleted = filepath.Join(env.logsDir, "deleted.access.log")
	writeTestAccessLog(t, deleted, 5)
	SetHostLogs([]utils.HostLog{
		{Path: env.logPaths[0], Type: "access", Source: "config"},
		{Path: deleted, Type: "access", Source: "config"},
	})
	env.index(t, deleted)

	SetHostLogs([]utils.HostLog{{Path: env.logPaths[0], Type: "access", Source: "config"}})
	require.NoError(t, os.Remove(deleted))

	// The services start the scheduler in the background.
	require.Eventually(t, func() bool { return GetTaskScheduler() != nil }, 10*time.Second, 10*time.Millisecond)
	return env, deleted
}

func staleGroupRows(t *testing.T, group string) int {
	t.Helper()
	rows, err := GetLogFileManager().GetPersistence().GetLogIndexesByGroup(group)
	require.NoError(t, err)
	return len(rows)
}

func TestPruneRemovesAGroupStaleInTwoRounds(t *testing.T) {
	env, deleted := startWithDeletedSite(t)

	first := pruneStaleLogGroups(nil)
	assert.Equal(t, map[string]bool{deleted: true}, first)
	assert.Equal(t, 1, staleGroupRows(t, deleted), "one round is not enough")
	assert.False(t, shardsLoaded.Load(), "a round that removes nothing opens no shard")

	assert.Empty(t, pruneStaleLogGroups(first))
	assert.Zero(t, staleGroupRows(t, deleted))
	assert.Equal(t, 1, staleGroupRows(t, env.logPaths[0]), "the listed group stays")

	count, err := GetIndexer().CountDocsByMainLogPath(deleted)
	require.NoError(t, err)
	assert.Zero(t, count)
	count, err = GetIndexer().CountDocsByMainLogPath(env.logPaths[0])
	require.NoError(t, err)
	assert.EqualValues(t, 5, count)
}

func TestPruneForgetsAGroupThatCameBack(t *testing.T) {
	env, deleted := startWithDeletedSite(t)

	first := pruneStaleLogGroups(nil)
	require.Contains(t, first, deleted)

	SetHostLogs([]utils.HostLog{
		{Path: env.logPaths[0], Type: "access", Source: "config"},
		{Path: deleted, Type: "access", Source: "config"},
	})
	assert.Empty(t, pruneStaleLogGroups(first))
	assert.Equal(t, 1, staleGroupRows(t, deleted))

	// Stale again: it has to wait for a second round again.
	SetHostLogs([]utils.HostLog{{Path: env.logPaths[0], Type: "access", Source: "config"}})
	assert.Contains(t, pruneStaleLogGroups(nil), deleted)
	assert.Equal(t, 1, staleGroupRows(t, deleted))
}

func TestPruneDoesNothingWithAnEmptyHostList(t *testing.T) {
	_, deleted := startWithDeletedSite(t)
	SetHostLogs(nil)

	seen := map[string]bool{deleted: true}
	assert.Equal(t, seen, pruneStaleLogGroups(seen))
	assert.Equal(t, 1, staleGroupRows(t, deleted))
}

func TestPruneDoesNothingDuringARebuild(t *testing.T) {
	_, deleted := startWithDeletedSite(t)

	end := BeginRound(true)
	defer end()

	seen := map[string]bool{deleted: true}
	assert.Equal(t, seen, pruneStaleLogGroups(seen))
	assert.Equal(t, 1, staleGroupRows(t, deleted))
}

func TestPruneRetriesAGroupHeldByATask(t *testing.T) {
	_, deleted := startWithDeletedSite(t)

	_, unlock := GetTaskScheduler().AcquireTaskLock(deleted)

	seen := map[string]bool{deleted: true}
	next := pruneStaleLogGroups(seen)
	assert.Equal(t, seen, next, "the group waits for the next round")
	assert.Equal(t, 1, staleGroupRows(t, deleted))

	unlock()
	assert.Empty(t, pruneStaleLogGroups(next))
	assert.Zero(t, staleGroupRows(t, deleted))
}
