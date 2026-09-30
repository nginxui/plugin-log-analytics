package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/store"
)

// A full rebuild destroys the shards and drops the metadata records. The group
// written afterwards must be named after the new record, or the shards cannot
// be found again once they are released and reopened from the records.
func TestGroupsAfterADestroyFollowTheNewRecords(t *testing.T) {
	database, err := store.OpenMemory()
	require.NoError(t, err)
	previous := store.Use(database)
	t.Cleanup(func() { store.Use(previous) })

	config := DefaultIndexerConfig()
	config.IndexPath = t.TempDir()
	config.ShardCount = 1
	gsm := NewGroupedShardManager(config)
	require.NoError(t, gsm.Initialize())
	t.Cleanup(func() { _ = gsm.Close() })

	const logPath = "/var/log/nginx/access.log"
	before, err := gsm.getOrCreateGroup(logPath)
	require.NoError(t, err)

	require.NoError(t, gsm.Destroy())
	require.NoError(t, NewPersistenceManager(nil).DeleteAllLogIndexes())

	after, err := gsm.getOrCreateGroup(logPath)
	require.NoError(t, err)
	require.NotEqual(t, before.UUID, after.UUID)

	require.NoError(t, gsm.ReleaseShards())
	require.NoError(t, gsm.LoadExistingGroups())
	require.Len(t, gsm.GetAllShards(), 1)
}
