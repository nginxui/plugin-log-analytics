package legacy

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/config"
	"github.com/nginxui/plugin-log-analytics/internal/store"
)

type memoryKV map[string]string

func (m memoryKV) KVGet(_ context.Context, key string, out any) (bool, error) {
	value, ok := m[key]
	if ok {
		*(out.(*string)) = value
	}
	return ok, nil
}

func (m memoryKV) KVSet(_ context.Context, key string, value any) error {
	m[key] = value.(string)
	return nil
}

func (m memoryKV) KVDelete(_ context.Context, key string) error {
	delete(m, key)
	return nil
}

// handoff is a prepared data directory with a legacy index, a city database and
// two exported rows.
type handoff struct {
	dataDir   string
	indexDir  string
	geoLite   string
	rows      []store.NginxLogIndex
	importDir string
}

func newHandoff(t *testing.T) *handoff {
	t.Helper()

	config.Reset()
	t.Cleanup(config.Reset)

	dataDir := t.TempDir()
	config.SetDataDir(dataDir)

	_, err := store.Open(dataDir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	legacyRoot := t.TempDir()
	indexDir := filepath.Join(legacyRoot, "log-index")
	groupID := uuid.New()
	require.NoError(t, os.MkdirAll(filepath.Join(indexDir, groupID.String(), "shard_0"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(indexDir, ".nginx-ui-index-version"), []byte("3\n"), 0o600))
	geoLite := filepath.Join(legacyRoot, "GeoLite2-City.mmdb")
	require.NoError(t, os.WriteFile(geoLite, []byte("mmdb"), 0o644))

	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rows := []store.NginxLogIndex{
		{
			BaseModelUUID: store.BaseModelUUID{ID: groupID, CreatedAt: created, UpdatedAt: created},
			Path:          "/var/log/nginx/access.log",
			MainLogPath:   "/var/log/nginx/access.log",
			LastSize:      1234,
			LastPosition:  1234,
			LastIndexed:   created,
			DocumentCount: 42,
			Enabled:       true,
			IndexStatus:   "indexed",
		},
		{
			BaseModelUUID: store.BaseModelUUID{ID: uuid.New(), CreatedAt: created, UpdatedAt: created},
			Path:          "/var/log/nginx/access.log.1",
			MainLogPath:   "/var/log/nginx/access.log",
			DocumentCount: 7,
			Enabled:       true,
			IndexStatus:   "indexed",
		},
	}

	importDir := filepath.Join(dataDir, ImportDir)
	require.NoError(t, os.MkdirAll(importDir, 0o755))
	writeJSON(t, filepath.Join(importDir, IndicesFile), rows)
	writeJSON(t, filepath.Join(importDir, LegacyFile), Legacy{IndexPath: indexDir, GeoLitePath: geoLite})

	return &handoff{dataDir: dataDir, indexDir: indexDir, geoLite: geoLite, rows: rows, importDir: importDir}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o644))
}

func TestConsumeWithoutHandoffDoesNothing(t *testing.T) {
	done, err := Consume(context.Background(), t.TempDir(), memoryKV{})
	require.NoError(t, err)
	assert.False(t, done)
}

func TestConsumeMovesIndexImportsRowsAndRemovesTheHandoff(t *testing.T) {
	h := newHandoff(t)

	done, err := Consume(context.Background(), h.dataDir, memoryKV{})
	require.NoError(t, err)
	require.True(t, done)

	// The index moved under the data directory, group directory included.
	moved := filepath.Join(h.dataDir, "index", h.rows[0].ID.String(), "shard_0")
	assert.DirExists(t, moved)
	assert.FileExists(t, filepath.Join(h.dataDir, "index", ".nginx-ui-index-version"))
	assert.NoDirExists(t, h.indexDir)
	assert.Equal(t, filepath.Join(h.dataDir, "index"), config.IndexPath())

	// The city database moved next to the others.
	assert.FileExists(t, filepath.Join(config.GeoLiteDir(), "GeoLite2-City.mmdb"))

	// Rows keep their ids, which name the group directories.
	db, err := store.DB()
	require.NoError(t, err)
	var got []store.NginxLogIndex
	require.NoError(t, db.Order("path").Find(&got).Error)
	require.Len(t, got, 2)
	assert.Equal(t, h.rows[0].ID, got[0].ID)
	assert.Equal(t, uint64(42), got[0].DocumentCount)
	assert.Equal(t, int64(1234), got[0].LastPosition)
	assert.Equal(t, "indexed", got[0].IndexStatus)
	assert.Equal(t, h.rows[1].ID, got[1].ID)

	// The removal of the directory is the sign the host waits for.
	assert.NoDirExists(t, h.importDir)
}

func TestConsumeReplacesARowThatAlreadyExists(t *testing.T) {
	h := newHandoff(t)

	db, err := store.DB()
	require.NoError(t, err)
	require.NoError(t, db.Create(&store.NginxLogIndex{Path: h.rows[0].Path, MainLogPath: h.rows[0].MainLogPath, DocumentCount: 1}).Error)

	_, err = Consume(context.Background(), h.dataDir, memoryKV{})
	require.NoError(t, err)

	var got []store.NginxLogIndex
	require.NoError(t, db.Where("path = ?", h.rows[0].Path).Find(&got).Error)
	require.Len(t, got, 1)
	assert.Equal(t, h.rows[0].ID, got[0].ID)
	assert.Equal(t, uint64(42), got[0].DocumentCount)
}

func TestConsumeKeepsAnExistingIndex(t *testing.T) {
	h := newHandoff(t)

	existing := filepath.Join(h.dataDir, "index", "keep")
	require.NoError(t, os.MkdirAll(existing, 0o755))

	_, err := Consume(context.Background(), h.dataDir, memoryKV{})
	require.NoError(t, err)

	assert.DirExists(t, existing)
	assert.DirExists(t, h.indexDir, "the old index is left alone when the plugin has one already")
}

func TestConsumeCanRunAgainAfterTheIndexMoved(t *testing.T) {
	h := newHandoff(t)
	require.NoError(t, os.Rename(h.indexDir, filepath.Join(h.dataDir, "index")))

	done, err := Consume(context.Background(), h.dataDir, memoryKV{})
	require.NoError(t, err)
	assert.True(t, done)
	assert.NoDirExists(t, h.importDir)
}

func TestConsumeKeepsTheHandoffWhenRowsCannotBeImported(t *testing.T) {
	h := newHandoff(t)
	require.NoError(t, os.WriteFile(filepath.Join(h.importDir, IndicesFile), []byte("{not json"), 0o644))

	done, err := Consume(context.Background(), h.dataDir, memoryKV{})
	require.Error(t, err)
	assert.False(t, done)
	assert.DirExists(t, h.importDir, "a failed import must stay retryable")
	assert.DirExists(t, h.indexDir, "nothing moves before the rows are in")
}

func TestConsumeWithEmptyLegacyFields(t *testing.T) {
	h := newHandoff(t)
	writeJSON(t, filepath.Join(h.importDir, LegacyFile), Legacy{})

	done, err := Consume(context.Background(), h.dataDir, memoryKV{})
	require.NoError(t, err)
	assert.True(t, done)
	assert.DirExists(t, h.indexDir)
	assert.NoDirExists(t, h.importDir)
}

func TestResolveKVAppliesAnExistingLocation(t *testing.T) {
	config.Reset()
	t.Cleanup(config.Reset)
	config.SetDataDir(t.TempDir())

	inPlace := t.TempDir()
	ResolveKV(context.Background(), memoryKV{KVIndexPath: inPlace})
	assert.Equal(t, inPlace, config.IndexPath())

	config.SetIndexPath("")
	ResolveKV(context.Background(), memoryKV{KVIndexPath: filepath.Join(inPlace, "missing")})
	assert.Equal(t, filepath.Join(config.DataDir(), "index"), config.IndexPath())
}
