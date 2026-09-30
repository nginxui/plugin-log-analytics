package indexer

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nginxui/plugin-log-analytics/internal/logger"
	"github.com/nginxui/plugin-log-analytics/internal/parser"
	"github.com/nginxui/plugin-log-analytics/internal/store"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// Content tracking of log files.
//
// A log line is identified by the content it belongs to and its byte offset in
// that content: the document id is "<fingerprint>-<offset>", where the
// fingerprint hashes the first line of the file and the offset is the start of
// the line in the decompressed stream. The path is not part of the id, so the
// same line gets the same id wherever it is read from. A file that was renamed
// by a rotation, copied by copytruncate or compressed to .gz is the same
// content, and reading it again overwrites its documents instead of adding a
// second set.
//
// The state of a file is its fingerprint and the offset after the last line
// that was indexed. A file whose fingerprint is already known under another
// path continues from that offset, which keeps the work to the new lines.
// Nothing depends on that shortcut for correctness: a wrong guess only costs a
// re-read, because the ids are stable.
const (
	// syncVersion marks the rows written by this tracking.
	syncVersion = 1

	// fingerprintProbe is how much of the start of a file is read to find its
	// first line.
	fingerprintProbe = 4096

	// docIDFingerprintLen is the number of hex characters of the fingerprint in
	// a document id.
	docIDFingerprintLen = 16

	// syncReadBuffer is the read buffer of a file.
	syncReadBuffer = 256 * 1024

	// largeFileThreshold and recentIndexWindow keep a big live log from being
	// read again right after its last round.
	largeFileThreshold = 100 * 1024 * 1024
	recentIndexWindow  = 30 * time.Minute
)

// FileSyncResult is what one file contributed to the index in a round.
type FileSyncResult struct {
	Docs        uint64     // documents written in this run
	MinTime     *time.Time // earliest timestamp of those documents
	MaxTime     *time.Time // latest timestamp of those documents
	Fingerprint string     // fingerprint of the content, empty when it has no first line yet
	Position    int64      // offset after the last line that was consumed
	Size        int64      // size of the file when it was read
	ModTime     time.Time  // modification time of the file when it was read
	Skipped     bool       // the file did not change and was not read
}

// GroupSyncResult is the outcome of a round over one log group.
type GroupSyncResult struct {
	Files   map[string]*FileSyncResult
	MinTime *time.Time
	MaxTime *time.Time
}

// GroupFiles lists the files of a log group: the base path and its rotated
// files, each one a regular file the host lets the plugin read.
func GroupFiles(basePath string) ([]string, error) {
	matches, err := filepath.Glob(basePath + "*")
	if err != nil {
		return nil, err
	}
	// Glob does not report the base file when the pattern needs no wildcard
	// match, so it is added explicitly.
	matches = append(matches, basePath)

	seen := make(map[string]struct{}, len(matches))
	files := make([]string, 0, len(matches))
	for _, match := range matches {
		if _, ok := seen[match]; ok {
			continue
		}
		seen[match] = struct{}{}
		if !utils.IsValidLogPath(match) {
			continue
		}
		info, err := os.Stat(match)
		if err == nil && info.Mode().IsRegular() {
			files = append(files, match)
		}
	}
	return files, nil
}

// NeedsSync reports whether a file has to be read again, given its stored row.
// A file that matches its row by size and modification time was read up to its
// end already. A row without the current tracking is always read, so the old
// documents of that file can be replaced.
func NeedsSync(info os.FileInfo, row *store.NginxLogIndex) bool {
	if row == nil || row.SyncVersion < syncVersion {
		return true
	}
	if row.LastSize == info.Size() && row.LastModified.UnixNano() == info.ModTime().UnixNano() {
		return false
	}
	return true
}

// Throttled reports whether a file that changed should still wait: a big live
// log that was indexed a moment ago is left for its next round. It is meant
// for the decision to start a round. The round itself reads every file that
// changed, because starting it already refreshed the last indexed time.
func Throttled(info os.FileInfo, row *store.NginxLogIndex) bool {
	return row != nil && info.Size() > largeFileThreshold && !row.LastIndexed.IsZero() &&
		time.Since(row.LastIndexed) < recentIndexWindow &&
		!strings.HasSuffix(strings.ToLower(info.Name()), ".gz")
}

// groupSnapshot is the stored state of a log group at the start of a round. It
// is read once, so the files of the round all see the state from before any of
// them was updated: after a rotation the new file replaces the row of the old
// path, and the renamed file still has to find it.
type groupSnapshot struct {
	byPath map[string]*store.NginxLogIndex
	byFP   map[string][]*store.NginxLogIndex
}

func newGroupSnapshot(rows []*store.NginxLogIndex) *groupSnapshot {
	snap := &groupSnapshot{
		byPath: make(map[string]*store.NginxLogIndex, len(rows)),
		byFP:   make(map[string][]*store.NginxLogIndex),
	}
	for _, row := range rows {
		snap.byPath[row.Path] = row
		if row.SyncVersion >= syncVersion && row.Fingerprint != "" {
			snap.byFP[row.Fingerprint] = append(snap.byFP[row.Fingerprint], row)
		}
	}
	return snap
}

// loadGroupSnapshot reads the stored rows of a group. Without a database the
// snapshot is empty and every file is read from its start.
func loadGroupSnapshot(mainLogPath string) *groupSnapshot {
	db, err := persistenceDB()
	if err != nil {
		return newGroupSnapshot(nil)
	}
	var rows []*store.NginxLogIndex
	if err := db.Where("main_log_path = ?", mainLogPath).Find(&rows).Error; err != nil {
		logger.Warnf("Could not read the index state of %s: %v", mainLogPath, err)
		return newGroupSnapshot(nil)
	}
	return newGroupSnapshot(rows)
}

// inherit returns the row of another path that holds the same content, the one
// that got furthest.
func (s *groupSnapshot) inherit(fingerprint, path string) *store.NginxLogIndex {
	var best *store.NginxLogIndex
	for _, row := range s.byFP[fingerprint] {
		if row.Path == path {
			continue
		}
		if best == nil || row.LastPosition > best.LastPosition {
			best = row
		}
	}
	return best
}

// stateMu serializes the writes of the file state. The files of a group are
// read by several goroutines.
var stateMu sync.Mutex

// saveFileState stores what a round learned about one file. The fields are
// written as a map, so zero values (an empty file, position zero) are stored
// too.
func saveFileState(path, mainLogPath string, res *FileSyncResult) error {
	db, err := persistenceDB()
	if err != nil {
		return err
	}

	stateMu.Lock()
	defer stateMu.Unlock()

	row := &store.NginxLogIndex{}
	if err := db.Where("path = ?", path).
		Attrs(&store.NginxLogIndex{Path: path, MainLogPath: mainLogPath, Enabled: true}).
		FirstOrCreate(row).Error; err != nil {
		return fmt.Errorf("could not create the index state of %s: %w", path, err)
	}

	return db.Model(&store.NginxLogIndex{}).Where("path = ?", path).Updates(map[string]any{
		"main_log_path": mainLogPath,
		"fingerprint":   res.Fingerprint,
		"sync_version":  syncVersion,
		"last_position": res.Position,
		"last_size":     res.Size,
		"last_modified": res.ModTime,
		"last_indexed":  time.Now(),
	}).Error
}

// purgeMode says which documents of a file are removed before it is read.
type purgeMode int

const (
	purgeNone   purgeMode = iota
	purgeLegacy           // only when the file has documents with the old ids
	purgeAll              // every document of the file
)

// syncOptions are the knobs of a read.
type syncOptions struct {
	// force ignores the stored state and reads the file from its start.
	force bool
	// onBatch reports the progress: lines read so far and the offset reached.
	onBatch func(lines int64, offset int64)
}

// syncFile reads the lines of one file that are not in the index yet and
// indexes them. It does not write the state, the caller stores the result once
// the file is done. An error leaves the state as it was, and the next read
// starts over, which is safe because the ids are stable.
func (pi *ParallelIndexer) syncFile(ctx context.Context, filePath, mainLogPath string, snap *groupSnapshot, opts syncOptions) (*FileSyncResult, error) {
	if !utils.IsValidLogPath(filePath) {
		return nil, fmt.Errorf("invalid log path: %s", filePath)
	}

	parserInstance := getLogParser()
	if parserInstance == nil {
		return nil, ErrLogParserNotInitialized
	}

	file, err := os.Open(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open log file %s: %w", filePath, err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat log file %s: %w", filePath, err)
	}
	size := info.Size()

	compressed := isGzipFile(file, filePath)
	result := &FileSyncResult{Size: size, ModTime: info.ModTime()}

	var cur *store.NginxLogIndex
	if !opts.force {
		cur = snap.byPath[filePath]
	}

	stream, closeStream, err := openContent(file, size, compressed, 0)
	if err != nil {
		return nil, err
	}
	fingerprint, err := fingerprintOf(stream, compressed)
	closeStream()
	if err != nil {
		return nil, fmt.Errorf("failed to read the start of %s: %w", filePath, err)
	}
	result.Fingerprint = fingerprint
	if fingerprint == "" {
		// No complete first line yet: nothing to index, and whatever the row
		// said about the content that was here before no longer applies.
		return result, nil
	}

	start := int64(0)
	purge := purgeNone
	legacyRow := cur == nil || cur.SyncVersion < syncVersion
	if legacyRow {
		purge = purgeLegacy
	}

	switch {
	case !legacyRow && cur.Fingerprint == fingerprint:
		start = cur.LastPosition
		if !compressed && start > size {
			// The file is shorter than what was read of it: it was rewritten.
			start = 0
			purge = purgeAll
		}
	case !opts.force:
		if known := snap.inherit(fingerprint, filePath); known != nil {
			start = known.LastPosition
			if !compressed && start > size {
				start = size
			}
		}
	}

	if purge != purgeNone {
		if err := pi.purgeDocsOfFile(filePath, purge == purgeLegacy); err != nil {
			return nil, err
		}
	}

	// A compressed copy that holds exactly what was read before needs no
	// decompression: the size of the content is in its trailer.
	if compressed && start > 0 && gzipContentSize(file, size) == uint32(start) && start < 1<<32 {
		result.Position = start
		result.Skipped = true
		return result, nil
	}

	stream, closeStream, err = openContent(file, size, compressed, start)
	if err != nil {
		return nil, err
	}
	defer closeStream()

	offset, docs, minTime, maxTime, err := pi.indexStream(ctx, parserInstance, stream, compressed, start, fingerprint, filePath, mainLogPath, opts)
	if err != nil {
		return nil, err
	}

	result.Docs = docs
	result.MinTime = minTime
	result.MaxTime = maxTime
	result.Position = offset
	return result, nil
}

// indexStream reads lines from the stream, which starts at the given offset of
// the content, and indexes them. It returns the offset after the last line
// that was consumed. An unterminated last line is consumed only in a
// compressed file, because a plain file may still be growing and the rest of
// the line is written later.
func (pi *ParallelIndexer) indexStream(ctx context.Context, p *parser.Parser, stream io.Reader, compressed bool, start int64, fingerprint, filePath, mainLogPath string, opts syncOptions) (int64, uint64, *time.Time, *time.Time, error) {
	reader := bufio.NewReaderSize(stream, syncReadBuffer)
	batch := pi.StartBatch()
	idPrefix := fingerprint[:docIDFingerprintLen] + "-"

	batchSize := pi.config.BatchSize
	if batchSize <= 0 {
		batchSize = 1000
	}
	lines := make([]string, 0, batchSize)
	offsets := make([]int64, 0, batchSize)

	var (
		docs       uint64
		invalid    int
		linesRead  int64
		minTime    *time.Time
		maxTime    *time.Time
		offset     = start
		idBuf      = make([]byte, 0, len(idPrefix)+20)
		flushLines = func() error {
			if len(lines) == 0 {
				return nil
			}
			entries := p.ParseLinesOrdered(ctx, lines)
			if err := ctx.Err(); err != nil {
				return err
			}
			for i, entry := range entries {
				if entry == nil {
					continue
				}
				doc := convertToLogDocument(entry, filePath, mainLogPath)
				if !isValidLogEntry(doc) {
					invalid++
					continue
				}

				ts := time.Unix(doc.Timestamp, 0)
				if minTime == nil || ts.Before(*minTime) {
					t := ts
					minTime = &t
				}
				if maxTime == nil || ts.After(*maxTime) {
					t := ts
					maxTime = &t
				}

				idBuf = append(idBuf[:0], idPrefix...)
				idBuf = strconv.AppendInt(idBuf, offsets[i], 10)
				if err := batch.Add(&Document{ID: string(idBuf), Fields: doc}); err != nil {
					return fmt.Errorf("failed to add a document of %s: %w", filePath, err)
				}
				docs++
			}
			linesRead += int64(len(lines))
			lines = lines[:0]
			offsets = offsets[:0]
			if opts.onBatch != nil {
				opts.onBatch(linesRead, offset)
			}
			return nil
		}
	)

	// consumed is the offset after the last line that was handed on, which is
	// the position to store. It is updated only after the line was queued.
	consumed := start
	for {
		line, err := reader.ReadSlice('\n')
		oversized := false
		if errors.Is(err, bufio.ErrBufferFull) {
			// A line longer than the buffer cannot be a log line. Its bytes are
			// skipped up to the end of the line.
			oversized = true
			total := len(line)
			for errors.Is(err, bufio.ErrBufferFull) {
				var more []byte
				more, err = reader.ReadSlice('\n')
				total += len(more)
			}
			line = line[:0]
			offset += int64(total)
		}

		if err != nil && !errors.Is(err, io.EOF) {
			return consumed, docs, minTime, maxTime, fmt.Errorf("failed to read %s: %w", filePath, err)
		}

		atEOF := errors.Is(err, io.EOF)
		if !oversized && len(line) > 0 {
			terminated := line[len(line)-1] == '\n'
			if terminated || compressed {
				text := string(bytes.TrimRight(line, "\r\n"))
				if text != "" {
					lines = append(lines, text)
					offsets = append(offsets, offset)
				}
				offset += int64(len(line))
				consumed = offset
			}
		} else if oversized {
			consumed = offset
		}

		if len(lines) >= batchSize {
			if err := flushLines(); err != nil {
				return consumed, docs, minTime, maxTime, err
			}
		}
		if atEOF {
			break
		}
	}

	if err := flushLines(); err != nil {
		return consumed, docs, minTime, maxTime, err
	}
	if _, err := batch.Flush(); err != nil {
		return consumed, docs, minTime, maxTime, fmt.Errorf("failed to flush the documents of %s: %w", filePath, err)
	}
	if invalid > 0 {
		logger.Warnf("File %s: filtered out %d invalid entries", filePath, invalid)
	}
	return consumed, docs, minTime, maxTime, nil
}

// isGzipFile reports whether the file has to be decompressed: it is named .gz
// and starts with the gzip magic number. A file with the name but without the
// header is read as plain text.
func isGzipFile(file *os.File, path string) bool {
	if !strings.HasSuffix(strings.ToLower(path), ".gz") {
		return false
	}
	var magic [2]byte
	if _, err := file.ReadAt(magic[:], 0); err != nil {
		return false
	}
	return magic[0] == 0x1f && magic[1] == 0x8b
}

// gzipContentSize returns the uncompressed size from the gzip trailer, modulo
// 2^32.
func gzipContentSize(file *os.File, size int64) uint32 {
	if size < 4 {
		return 0
	}
	var trailer [4]byte
	if _, err := file.ReadAt(trailer[:], size-4); err != nil {
		return 0
	}
	return binary.LittleEndian.Uint32(trailer[:])
}

// openContent returns a reader of the decompressed content that starts at the
// given offset. A plain file is read only up to the size it had when it was
// opened, so the offset that is stored never passes what was indexed.
func openContent(file *os.File, size int64, compressed bool, offset int64) (io.Reader, func(), error) {
	if !compressed {
		if offset > size {
			offset = size
		}
		return io.NewSectionReader(file, offset, size-offset), func() {}, nil
	}

	reader, err := gzip.NewReader(io.NewSectionReader(file, 0, size))
	if err != nil {
		return nil, func() {}, fmt.Errorf("failed to create gzip reader: %w", err)
	}
	closeFn := func() { _ = reader.Close() }
	if offset > 0 {
		if _, err := io.CopyN(io.Discard, reader, offset); err != nil && !errors.Is(err, io.EOF) {
			closeFn()
			return nil, func() {}, fmt.Errorf("failed to skip to offset %d: %w", offset, err)
		}
	}
	return reader, closeFn, nil
}

// fingerprintOf hashes the first line of the content. The line is what tells
// one file from another, and it does not change while the file grows. It
// returns an empty fingerprint when the first line is not complete yet.
func fingerprintOf(stream io.Reader, compressed bool) (string, error) {
	buf := make([]byte, fingerprintProbe)
	n, err := io.ReadFull(stream, buf)
	ended := false
	switch {
	case err == nil:
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		ended = true
	default:
		return "", err
	}
	buf = buf[:n]

	// Empty lines at the start say nothing about the file.
	buf = bytes.TrimLeft(buf, "\r\n")
	if len(buf) == 0 {
		return "", nil
	}

	line := buf
	if i := bytes.IndexByte(buf, '\n'); i >= 0 {
		line = buf[:i]
	} else if ended && !compressed {
		// The line is still being written.
		return "", nil
	}
	line = bytes.TrimRight(line, "\r")

	sum := sha256.Sum256(line)
	return hex.EncodeToString(sum[:16]), nil
}

// isTrackedDocID reports whether a document id has the form of this tracking,
// 16 hex characters of fingerprint, a dash and an offset.
func isTrackedDocID(id string) bool {
	if len(id) <= docIDFingerprintLen+1 || id[docIDFingerprintLen] != '-' {
		return false
	}
	for i := 0; i < docIDFingerprintLen; i++ {
		c := id[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	for _, c := range id[docIDFingerprintLen+1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
