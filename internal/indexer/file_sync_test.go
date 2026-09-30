package indexer

import (
	"compress/gzip"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/blevesearch/bleve/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/store"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// syncEnv is an indexer over a throwaway index and state database, with one
// listed log in a temporary directory.
type syncEnv struct {
	t       *testing.T
	pi      *ParallelIndexer
	dir     string
	logPath string
	cfg     *Config
	written map[int]struct{}
	nextID  int
}

func newSyncEnv(t *testing.T) *syncEnv {
	t.Helper()

	database, err := store.OpenMemory()
	require.NoError(t, err)
	previous := store.Use(database)
	t.Cleanup(func() { store.Use(previous) })

	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	utils.SetHostLogs([]utils.HostLog{{Path: logPath, Type: "access", Source: "config"}})
	t.Cleanup(func() { utils.SetHostLogs(nil) })

	cfg := DefaultIndexerConfig()
	cfg.IndexPath = filepath.Join(dir, "index")
	env := &syncEnv{t: t, dir: dir, logPath: logPath, cfg: cfg, written: map[int]struct{}{}, nextID: 1}
	env.startIndexer()
	return env
}

func (e *syncEnv) startIndexer() {
	e.t.Helper()
	pi := NewParallelIndexer(e.cfg, NewGroupedShardManager(e.cfg))
	require.NoError(e.t, pi.Start(context.Background()))
	e.pi = pi
	e.t.Cleanup(func() { _ = pi.Stop() })
}

// restart stops the indexer and opens a new one on the same index.
func (e *syncEnv) restart() {
	e.t.Helper()
	require.NoError(e.t, e.pi.Stop())
	e.startIndexer()
}

var recentStamp = regexp.MustCompile(`/id/(\d+) `)

func (e *syncEnv) line(id int) string {
	stamp := time.Now().UTC().Format("02/Jan/2006:15:04:05 -0700")
	return fmt.Sprintf(`10.0.0.%d - - [%s] "GET /id/%06d HTTP/1.1" 200 123 "-" "sync-test"`, id%250+1, stamp, id)
}

// lines returns count complete lines with new ids and records them as written.
func (e *syncEnv) lines(count int) string {
	out := ""
	for i := 0; i < count; i++ {
		id := e.nextID
		e.nextID++
		e.written[id] = struct{}{}
		out += e.line(id) + "\n"
	}
	return out
}

func (e *syncEnv) path(name string) string { return filepath.Join(e.dir, name) }

func (e *syncEnv) write(name, content string) {
	e.t.Helper()
	require.NoError(e.t, os.WriteFile(e.path(name), []byte(content), 0o644))
}

func (e *syncEnv) appendTo(name, content string) {
	e.t.Helper()
	f, err := os.OpenFile(e.path(name), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	require.NoError(e.t, err)
	_, err = f.WriteString(content)
	require.NoError(e.t, err)
	require.NoError(e.t, f.Close())
}

func (e *syncEnv) rename(from, to string) {
	e.t.Helper()
	require.NoError(e.t, os.Rename(e.path(from), e.path(to)))
}

func (e *syncEnv) gzip(name string) {
	e.t.Helper()
	data, err := os.ReadFile(e.path(name))
	require.NoError(e.t, err)
	out, err := os.Create(e.path(name + ".gz"))
	require.NoError(e.t, err)
	zw := gzip.NewWriter(out)
	_, err = zw.Write(data)
	require.NoError(e.t, err)
	require.NoError(e.t, zw.Close())
	require.NoError(e.t, out.Close())
	require.NoError(e.t, os.Remove(e.path(name)))
}

// sync runs one round over the group.
func (e *syncEnv) sync() *GroupSyncResult {
	e.t.Helper()
	group, err := e.pi.SyncLogGroup(e.logPath, nil)
	require.NoError(e.t, err)
	return group
}

type indexedDoc struct {
	id       string
	raw      string
	filePath string
}

func (e *syncEnv) docs() []indexedDoc {
	e.t.Helper()
	var out []indexedDoc
	for _, shard := range e.pi.shardManager.GetAllShards() {
		request := bleve.NewSearchRequest(bleve.NewMatchAllQuery())
		request.Size = 100000
		request.Fields = []string{"raw", "file_path"}
		result, err := shard.Search(request)
		require.NoError(e.t, err)
		for _, hit := range result.Hits {
			doc := indexedDoc{id: hit.ID}
			doc.raw, _ = hit.Fields["raw"].(string)
			doc.filePath, _ = hit.Fields["file_path"].(string)
			out = append(out, doc)
		}
	}
	return out
}

// assertExactlyOnce checks that the index holds every written line once, and
// nothing else.
func (e *syncEnv) assertExactlyOnce(step string) {
	e.t.Helper()
	counts := map[int]int{}
	for _, doc := range e.docs() {
		match := recentStamp.FindStringSubmatch(doc.raw + " ")
		if match == nil {
			e.t.Errorf("%s: document %s has an unexpected line %q", step, doc.id, doc.raw)
			continue
		}
		id, _ := strconv.Atoi(match[1])
		counts[id]++
	}

	var missing, duplicated []int
	for id := range e.written {
		switch counts[id] {
		case 0:
			missing = append(missing, id)
		case 1:
		default:
			duplicated = append(duplicated, id)
		}
	}
	sort.Ints(missing)
	sort.Ints(duplicated)
	assert.Empty(e.t, missing, "%s: lines that are not indexed", step)
	assert.Empty(e.t, duplicated, "%s: lines that are indexed more than once", step)
	total := 0
	for _, c := range counts {
		total += c
	}
	assert.Equal(e.t, len(e.written), total, "%s: documents in the index", step)
}

func TestSyncIndexesAppendedLinesOnce(t *testing.T) {
	e := newSyncEnv(t)

	e.write("access.log", e.lines(100))
	e.sync()
	e.assertExactlyOnce("initial")

	// Nothing changed: the round reads nothing.
	group := e.sync()
	require.NotNil(t, group)
	assert.Empty(t, group.Files, "an unchanged file is not read")

	e.appendTo("access.log", e.lines(50))
	group = e.sync()
	assert.Equal(t, uint64(50), group.Files[e.logPath].Docs, "only the new lines are read")
	e.assertExactlyOnce("append")
}

func TestSyncWaitsForTheEndOfAPartialLine(t *testing.T) {
	e := newSyncEnv(t)

	e.write("access.log", e.lines(5))
	e.sync()

	id := e.nextID
	e.nextID++
	full := e.line(id)
	e.appendTo("access.log", full[:len(full)/2])
	e.sync()
	e.assertExactlyOnce("partial line is not indexed yet")

	e.appendTo("access.log", full[len(full)/2:]+"\n"+e.lines(3))
	e.written[id] = struct{}{}
	e.sync()
	e.assertExactlyOnce("partial line completed")
}

func TestSyncRenameRotationWithUnindexedTail(t *testing.T) {
	e := newSyncEnv(t)

	e.write("access.log", e.lines(100))
	e.sync()

	// Lines that arrive between the last round and the rotation.
	e.appendTo("access.log", e.lines(20))
	e.rename("access.log", "access.log.1")
	e.write("access.log", e.lines(10))
	e.sync()
	e.assertExactlyOnce("rotation with tail")

	group := e.sync()
	assert.Empty(t, group.Files)

	e.appendTo("access.log", e.lines(10))
	e.sync()
	e.assertExactlyOnce("append after rotation")
}

func TestSyncRenameRotationWithoutTail(t *testing.T) {
	e := newSyncEnv(t)

	e.write("access.log", e.lines(100))
	e.sync()

	e.rename("access.log", "access.log.1")
	e.write("access.log", e.lines(5))
	group := e.sync()
	assert.Equal(t, uint64(0), group.Files[e.path("access.log.1")].Docs, "the renamed file continues from where it ended")
	assert.Equal(t, uint64(5), group.Files[e.logPath].Docs)
	e.assertExactlyOnce("rotation")
}

func TestSyncCopyTruncate(t *testing.T) {
	e := newSyncEnv(t)

	e.write("access.log", e.lines(100))
	e.sync()

	e.appendTo("access.log", e.lines(7))
	data, err := os.ReadFile(e.logPath)
	require.NoError(t, err)
	e.write("access.log.1", string(data))
	require.NoError(t, os.Truncate(e.logPath, 0))
	e.appendTo("access.log", e.lines(3))
	e.sync()
	e.assertExactlyOnce("copytruncate")

	// The live file keeps growing from its new start.
	e.appendTo("access.log", e.lines(4))
	e.sync()
	e.assertExactlyOnce("after copytruncate")
}

func TestSyncCopyTruncateToEmptyFile(t *testing.T) {
	e := newSyncEnv(t)

	e.write("access.log", e.lines(30))
	e.sync()

	data, err := os.ReadFile(e.logPath)
	require.NoError(t, err)
	e.write("access.log.1", string(data))
	require.NoError(t, os.Truncate(e.logPath, 0))
	e.sync()
	e.assertExactlyOnce("truncated to empty")

	group := e.sync()
	assert.Empty(t, group.Files, "an empty file is not read again and again")

	e.appendTo("access.log", e.lines(5))
	e.sync()
	e.assertExactlyOnce("first lines after truncate")
}

func TestSyncCompressedCopyOfAnIndexedFile(t *testing.T) {
	e := newSyncEnv(t)

	e.write("access.log", e.lines(100))
	e.sync()
	e.rename("access.log", "access.log.1")
	e.write("access.log", e.lines(10))
	e.sync()
	e.assertExactlyOnce("rotated")

	// logrotate compresses the rotated file on the next cycle and shifts names.
	e.rename("access.log.1", "access.log.2")
	e.gzip("access.log.2")
	e.rename("access.log", "access.log.1")
	e.write("access.log", e.lines(5))
	group := e.sync()
	assert.Equal(t, uint64(0), group.Files[e.path("access.log.2.gz")].Docs, "the compressed copy holds what was indexed already")
	e.assertExactlyOnce("compressed")

	// A second round finds nothing to do.
	assert.Empty(t, e.sync().Files)
}

func TestSyncCompressedFileWithAnUnindexedTail(t *testing.T) {
	e := newSyncEnv(t)

	e.write("access.log", e.lines(50))
	e.sync()

	// Lines after the last round, and the file is compressed at once.
	e.appendTo("access.log", e.lines(12))
	e.rename("access.log", "access.log.1")
	e.gzip("access.log.1")
	e.write("access.log", e.lines(2))
	e.sync()
	e.assertExactlyOnce("compressed with tail")
}

func TestSyncTwoRotationsBetweenRounds(t *testing.T) {
	e := newSyncEnv(t)

	e.write("access.log", e.lines(40))
	e.sync()

	e.appendTo("access.log", e.lines(5))
	e.rename("access.log", "access.log.1")
	e.write("access.log", e.lines(6))
	// The second rotation: .1 becomes .2 and is compressed, the live file .1.
	e.rename("access.log.1", "access.log.2")
	e.gzip("access.log.2")
	e.rename("access.log", "access.log.1")
	e.write("access.log", e.lines(7))
	e.sync()
	e.assertExactlyOnce("two rotations")

	assert.Empty(t, e.sync().Files)
}

func TestSyncSurvivesARestart(t *testing.T) {
	e := newSyncEnv(t)

	e.write("access.log", e.lines(60))
	e.sync()

	e.appendTo("access.log", e.lines(5))
	e.restart()
	e.appendTo("access.log", e.lines(5))
	e.sync()
	e.assertExactlyOnce("after restart")

	e.rename("access.log", "access.log.1")
	e.write("access.log", e.lines(3))
	e.restart()
	e.sync()
	e.assertExactlyOnce("rotation after restart")
}

func TestSyncReplacesDocumentsOfAnOlderVersion(t *testing.T) {
	e := newSyncEnv(t)

	content := e.lines(30)
	e.write("access.log", content)

	// Documents as an older version wrote them, with ids built from the path,
	// and a state row without content tracking.
	parsed, err := ParseLogStream(context.Background(), strings.NewReader(content), e.logPath)
	require.NoError(t, err)
	require.Len(t, parsed, 30)
	docs := make([]*Document, 0, len(parsed))
	for i, doc := range parsed {
		docs = append(docs, &Document{ID: fmt.Sprintf("%s-%d", e.logPath, i), Fields: doc})
	}
	require.NoError(t, e.pi.IndexDocuments(context.Background(), docs))
	database, err := store.DB()
	require.NoError(t, err)
	// Creating the shard group inserted a placeholder row for the group.
	require.NoError(t, database.Model(&store.NginxLogIndex{}).Where("path = ?", e.logPath).Updates(map[string]any{
		"last_size": len(content), "last_indexed": time.Now(), "document_count": 30,
	}).Error)
	require.Len(t, e.docs(), 30)

	e.sync()
	e.assertExactlyOnce("old documents replaced")
}

func TestSyncRewrittenFileWithTheSameFirstLine(t *testing.T) {
	e := newSyncEnv(t)

	first := e.lines(1)
	e.write("access.log", first+e.lines(40))
	e.sync()

	// The file is rewritten shorter, starting with the same line.
	for id := 2; id <= 41; id++ {
		delete(e.written, id)
	}
	e.write("access.log", first+e.lines(3))
	e.sync()
	e.assertExactlyOnce("rewritten")
}

func TestSyncFirstLineArrivesLater(t *testing.T) {
	e := newSyncEnv(t)

	e.write("access.log", "")
	group := e.sync()
	assert.Equal(t, uint64(0), group.Files[e.logPath].Docs)
	assert.Empty(t, e.sync().Files, "an empty file is left alone")

	e.appendTo("access.log", e.lines(4))
	e.sync()
	e.assertExactlyOnce("first lines")
}

func TestRebuildReplacesDocuments(t *testing.T) {
	e := newSyncEnv(t)

	e.write("access.log", e.lines(80))
	e.sync()
	e.rename("access.log", "access.log.1")
	e.write("access.log", e.lines(10))
	e.sync()

	for i := 0; i < 2; i++ {
		counts, _, _, err := e.pi.IndexLogGroupWithProgress(e.logPath, nil)
		require.NoError(t, err)
		assert.Equal(t, uint64(80), counts[e.path("access.log.1")])
		assert.Equal(t, uint64(10), counts[e.logPath])
		e.assertExactlyOnce(fmt.Sprintf("rebuild %d", i+1))
	}

	// After a rebuild the incremental state is intact.
	e.appendTo("access.log", e.lines(5))
	e.sync()
	e.assertExactlyOnce("append after rebuild")
}

func TestSyncSkipsLinesItCannotParseOrRead(t *testing.T) {
	e := newSyncEnv(t)

	long := make([]byte, 2*syncReadBuffer)
	for i := range long {
		long[i] = 'x'
	}
	e.write("access.log", e.lines(3)+string(long)+"\n\n"+e.lines(3))
	e.sync()
	e.assertExactlyOnce("oversized line")
}

func TestNeedsSync(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	require.NoError(t, os.WriteFile(path, []byte("abc\n"), 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)

	assert.True(t, NeedsSync(info, nil), "no row")
	assert.True(t, NeedsSync(info, &store.NginxLogIndex{LastSize: info.Size(), LastModified: info.ModTime()}), "a row from before content tracking")

	row := &store.NginxLogIndex{SyncVersion: syncVersion, LastSize: info.Size(), LastModified: info.ModTime(), LastIndexed: time.Now()}
	assert.False(t, NeedsSync(info, row), "same size and time")

	row.LastSize--
	assert.True(t, NeedsSync(info, row), "size changed")

	row.LastSize = info.Size()
	row.LastModified = info.ModTime().Add(-time.Second)
	assert.True(t, NeedsSync(info, row), "time changed")
}

func TestFingerprintOf(t *testing.T) {
	first, err := fingerprintOf(strings.NewReader("first line\nsecond"), false)
	require.NoError(t, err)
	again, err := fingerprintOf(strings.NewReader("first line\nmuch more content\n"), false)
	require.NoError(t, err)
	other, err := fingerprintOf(strings.NewReader("other line\n"), false)
	require.NoError(t, err)
	assert.NotEmpty(t, first)
	assert.Equal(t, first, again, "the fingerprint does not change while the file grows")
	assert.NotEqual(t, first, other)

	unfinished, err := fingerprintOf(strings.NewReader("first li"), false)
	require.NoError(t, err)
	assert.Empty(t, unfinished, "a line that is still being written has no fingerprint")

	blanks, err := fingerprintOf(strings.NewReader("\n\nfirst line\n"), false)
	require.NoError(t, err)
	assert.Equal(t, first, blanks, "leading empty lines do not count")

	compressedTail, err := fingerprintOf(strings.NewReader("first line"), true)
	require.NoError(t, err)
	assert.Equal(t, first, compressedTail, "a compressed file is complete")
}

func TestIsTrackedDocID(t *testing.T) {
	assert.True(t, isTrackedDocID("0123456789abcdef-12345"))
	assert.False(t, isTrackedDocID("/var/log/nginx/access.log-12"))
	assert.False(t, isTrackedDocID("/var/log/nginx/access.log_0_12"))
	assert.False(t, isTrackedDocID("0123456789abcdef-"))
	assert.False(t, isTrackedDocID("0123456789ABCDEF-1"))
}

func TestThrottledOnlyForBigRecentlyIndexedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "access.log")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(largeFileThreshold+1))
	require.NoError(t, f.Close())
	info, err := os.Stat(path)
	require.NoError(t, err)

	row := &store.NginxLogIndex{SyncVersion: syncVersion, LastSize: 1, LastModified: info.ModTime().Add(-time.Hour), LastIndexed: time.Now()}
	assert.True(t, NeedsSync(info, row), "the round reads a big file that changed, even right after its status was written")
	assert.True(t, Throttled(info, row))

	row.LastIndexed = time.Now().Add(-2 * recentIndexWindow)
	assert.False(t, Throttled(info, row))
	assert.False(t, Throttled(info, nil))
}
