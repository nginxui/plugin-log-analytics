package service

import (
	"os"
	"path/filepath"
	"testing"
	"time"

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

// Test that grouped (aggregated) log metadata with oversized LastSize values
// does not incorrectly trigger rotation detection in the fallback path.
func TestNeedsIncrementalIndexingAggregatedSizeRespectsClamp(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "access.log")
	listLog(t, logPath)

	if err := os.WriteFile(logPath, []byte("nginx-ui"), 0o644); err != nil {
		t.Fatalf("failed to create temp log file: %v", err)
	}

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("failed to stat temp log file: %v", err)
	}

	// Simulate aggregated metadata where LastSize is larger than the live file.
	logEntry := &NginxLogWithIndex{
		Path:         logPath,
		Type:         "access",
		LastModified: info.ModTime().Unix(),
		LastIndexed:  time.Now().Unix(),
		LastSize:     info.Size() * 5,
	}

	if needsIncrementalIndexing(logEntry, nil) {
		t.Fatalf("aggregated size should not trigger re-indexing when clamped")
	}
}

type stubLogIndexProvider struct {
	idx *store.NginxLogIndex
	err error
}

func (s stubLogIndexProvider) GetLogIndex(path string) (*store.NginxLogIndex, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.idx != nil {
		s.idx.Path = path
	}
	return s.idx, nil
}

func TestNeedsIncrementalIndexingSkipsWhenUnchanged(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	listLog(t, logPath)
	if err := os.WriteFile(logPath, []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("write temp log: %v", err)
	}

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat temp log: %v", err)
	}

	persisted := &store.NginxLogIndex{
		Path:         logPath,
		LastModified: info.ModTime(),
		LastSize:     info.Size(),
		LastIndexed:  time.Now(),
	}

	logData := &NginxLogWithIndex{
		Path:         logPath,
		Type:         "access",
		IndexStatus:  string(indexer.IndexStatusIndexed),
		LastModified: info.ModTime().Unix(),
		LastSize:     info.Size() * 10, // simulate grouped size inflation
		LastIndexed:  time.Now().Unix(),
	}

	if needsIncrementalIndexing(logData, stubLogIndexProvider{idx: persisted}) {
		t.Fatalf("expected no incremental indexing when file metadata is unchanged")
	}
}

func TestNeedsIncrementalIndexingDetectsGrowth(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	listLog(t, logPath)
	if err := os.WriteFile(logPath, []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("write temp log: %v", err)
	}

	initialInfo, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat temp log: %v", err)
	}

	persisted := &store.NginxLogIndex{
		Path:         logPath,
		LastModified: initialInfo.ModTime().Add(-time.Minute),
		LastSize:     initialInfo.Size(),
		LastIndexed:  time.Now().Add(-time.Minute),
	}

	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open temp log: %v", err)
	}
	if _, err := f.WriteString("more data\n"); err != nil {
		f.Close()
		t.Fatalf("append temp log: %v", err)
	}
	_ = f.Close()

	finalInfo, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("restat temp log: %v", err)
	}

	logData := &NginxLogWithIndex{
		Path:         logPath,
		Type:         "access",
		IndexStatus:  string(indexer.IndexStatusIndexed),
		LastModified: finalInfo.ModTime().Unix(),
		LastSize:     initialInfo.Size(),
		LastIndexed:  time.Now().Unix(),
	}

	if !needsIncrementalIndexing(logData, stubLogIndexProvider{idx: persisted}) {
		t.Fatalf("expected incremental indexing when file grew")
	}
}

func TestNeedsIncrementalIndexingIgnoresUnlistedFiles(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "access.log")
	if err := os.WriteFile(logPath, []byte("initial\n"), 0o644); err != nil {
		t.Fatalf("write temp log: %v", err)
	}

	// The file exists but the host does not list it.
	utils.SetHostLogs(nil)

	logData := &NginxLogWithIndex{
		Path:        logPath,
		Type:        "access",
		IndexStatus: string(indexer.IndexStatusNotIndexed),
	}
	if needsIncrementalIndexing(logData, nil) {
		t.Fatalf("a file the host does not list must never be read")
	}
}
