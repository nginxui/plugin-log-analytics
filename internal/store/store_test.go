package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpenCreatesTheDatabaseAndMigrates(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Close() })

	assert.FileExists(t, filepath.Join(dir, DatabaseFile))
	assert.True(t, db.Migrator().HasTable("nginx_log_indices"), "the table keeps the name the host used")

	current, err := DB()
	require.NoError(t, err)
	assert.Same(t, db, current)
}

func TestDBBeforeOpenReportsNotOpen(t *testing.T) {
	previous := Use(nil)
	t.Cleanup(func() { Use(previous) })

	_, err := DB()
	assert.ErrorIs(t, err, ErrNotOpen)
}

func TestRowsSurviveAReopen(t *testing.T) {
	dir := t.TempDir()

	db, err := Open(dir)
	require.NoError(t, err)

	started := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	duration := int64(1500)
	row := NginxLogIndex{
		Path:           "/var/log/nginx/access.log",
		MainLogPath:    "/var/log/nginx/access.log",
		LastModified:   started,
		LastSize:       99,
		LastPosition:   98,
		LastIndexed:    started.Add(time.Minute),
		IndexStartTime: &started,
		IndexDuration:  &duration,
		TimeRangeStart: &started,
		TimeRangeEnd:   ptr(started.Add(time.Hour)),
		DocumentCount:  12,
		Enabled:        true,
		IndexStatus:    "indexed",
	}
	require.NoError(t, db.Create(&row).Error)
	require.NotEqual(t, uuid.Nil, row.ID)
	require.NoError(t, Close())

	db, err = Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = Close() })

	var got NginxLogIndex
	require.NoError(t, db.Where("path = ?", row.Path).First(&got).Error)
	assert.Equal(t, row.ID, got.ID)
	assert.True(t, started.Equal(got.LastModified), "got %s", got.LastModified)
	assert.True(t, started.Add(time.Minute).Equal(got.LastIndexed))
	require.NotNil(t, got.TimeRangeEnd)
	assert.True(t, started.Add(time.Hour).Equal(*got.TimeRangeEnd))
	assert.Equal(t, duration, *got.IndexDuration)
	assert.Equal(t, uint64(12), got.DocumentCount)
}

// Time columns compare as text, so a range filter has to agree with the order
// of the instants.
func TestTimeComparisonsFollowTheInstants(t *testing.T) {
	db, err := OpenMemory()
	require.NoError(t, err)

	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		require.NoError(t, db.Create(&NginxLogIndex{
			Path:        filepath.Join("/logs", string(rune('a'+i))),
			LastIndexed: base.Add(time.Duration(i) * 24 * time.Hour),
		}).Error)
	}

	var older int64
	require.NoError(t, db.Model(&NginxLogIndex{}).Where("last_indexed < ?", base.Add(48*time.Hour)).Count(&older).Error)
	assert.Equal(t, int64(2), older)
}

func TestConcurrentUpsertsDoNotLockTheDatabase(t *testing.T) {
	db, err := Open(t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = Close() })

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 8 {
				path := filepath.Join("/logs", string(rune('a'+worker)), string(rune('a'+i)))
				row := &NginxLogIndex{}
				err := db.Where("path = ?", path).
					Assign(&NginxLogIndex{Path: path, MainLogPath: path, Enabled: true, DocumentCount: uint64(i + 1)}).
					FirstOrCreate(row).Error
				if err != nil {
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	var count int64
	require.NoError(t, db.Model(&NginxLogIndex{}).Count(&count).Error)
	assert.Equal(t, int64(64), count)
}

func TestNeedsIndexing(t *testing.T) {
	now := time.Now()

	fresh := &NginxLogIndex{}
	assert.True(t, fresh.NeedsIndexing(now, 10), "never indexed")

	indexed := &NginxLogIndex{LastIndexed: now, LastModified: now, LastSize: 100}
	assert.False(t, indexed.NeedsIndexing(now, 100), "unchanged")
	assert.True(t, indexed.NeedsIndexing(now.Add(time.Minute), 200), "grown")
	assert.True(t, indexed.NeedsIndexing(now, 50), "shrunk, probably rotated")
}

func ptr[T any](v T) *T { return &v }
