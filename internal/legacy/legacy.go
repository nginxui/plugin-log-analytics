// Package legacy takes over the log analytics data of a host that kept it
// itself before this plugin existed. The host leaves a handoff in the import
// directory of the plugin data directory:
//
//	import/nginx_log_indices.json  the rows of the old table, one object each
//	import/legacy.json             {"index_path": "...", "geolite_path": "..."}
//
// Consume moves the index and the city database into the data directory,
// imports the rows into the state database and removes the import directory.
// The host treats the removal as the sign that the import worked, so nothing
// is removed until everything else succeeded, and every step can run again.
package legacy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"gorm.io/gorm"

	"github.com/nginxui/plugin-log-analytics/internal/config"
	"github.com/nginxui/plugin-log-analytics/internal/logger"
	"github.com/nginxui/plugin-log-analytics/internal/store"
)

const (
	// ImportDir is the directory of the handoff inside the data directory.
	ImportDir = "import"
	// IndicesFile holds the exported rows of the old nginx_log_indices table.
	IndicesFile = "nginx_log_indices.json"
	// LegacyFile names the old index directory and the old city database.
	LegacyFile = "legacy.json"

	// KVIndexPath is the key that keeps the path of an index that could not be
	// moved into the data directory and is used where it is.
	KVIndexPath = "index_path"

	// importBatchSize is how many rows go into one insert.
	importBatchSize = 200
)

// KV is the part of the host key value store the handoff needs.
type KV interface {
	KVGet(ctx context.Context, key string, out any) (bool, error)
	KVSet(ctx context.Context, key string, value any) error
	KVDelete(ctx context.Context, key string) error
}

// Legacy is the content of legacy.json. Either field may be empty.
type Legacy struct {
	IndexPath   string `json:"index_path"`
	GeoLitePath string `json:"geolite_path"`
}

// Consume imports the handoff in dataDir, if there is one. It returns whether a
// handoff was found and completed. When it fails, the import directory stays in
// place and the next start tries again.
func Consume(ctx context.Context, dataDir string, kv KV) (bool, error) {
	importDir := filepath.Join(dataDir, ImportDir)
	if info, err := os.Stat(importDir); err != nil || !info.IsDir() {
		return false, nil
	}

	logger.Infof("Found a log analytics handoff from the host in %s, importing it", importDir)

	legacy, err := readLegacy(filepath.Join(importDir, LegacyFile))
	if err != nil {
		logger.Warnf("Ignoring the unreadable legacy description: %v", err)
	}

	// The rows first: they are what the group directories of the index are named
	// after, and importing them again is harmless.
	imported, err := importRows(filepath.Join(importDir, IndicesFile))
	if err != nil {
		return false, err
	}
	logger.Infof("Imported %d index record(s)", imported)

	if err := adoptIndex(ctx, legacy.IndexPath, dataDir, kv); err != nil {
		return false, err
	}
	if err := adoptGeoLite(legacy.GeoLitePath); err != nil {
		// The database can be downloaded again, so this does not hold the import back.
		logger.Warnf("Could not take over the city database %s: %v", legacy.GeoLitePath, err)
	}

	if err := os.RemoveAll(importDir); err != nil {
		return false, fmt.Errorf("remove the import directory: %w", err)
	}
	logger.Info("Log analytics handoff completed")
	return true, nil
}

func readLegacy(path string) (Legacy, error) {
	var legacy Legacy
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return legacy, nil
		}
		return legacy, err
	}
	if err := json.Unmarshal(raw, &legacy); err != nil {
		return Legacy{}, err
	}
	return legacy, nil
}

// importRows upserts the exported rows by path. A row keeps its id and creation
// time, because the shard directories of the index are named after them.
func importRows(path string) (int, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read the exported index records: %w", err)
	}

	var rows []store.NginxLogIndex
	if err := json.Unmarshal(raw, &rows); err != nil {
		return 0, fmt.Errorf("decode the exported index records: %w", err)
	}
	if len(rows) == 0 {
		return 0, nil
	}

	db, err := store.DB()
	if err != nil {
		return 0, err
	}

	err = db.Transaction(func(tx *gorm.DB) error {
		paths := make([]string, 0, len(rows))
		for i := range rows {
			paths = append(paths, rows[i].Path)
		}
		for start := 0; start < len(paths); start += importBatchSize {
			end := min(start+importBatchSize, len(paths))
			if err := tx.Where("path IN ?", paths[start:end]).Delete(&store.NginxLogIndex{}).Error; err != nil {
				return err
			}
		}
		return tx.CreateInBatches(rows, importBatchSize).Error
	})
	if err != nil {
		return 0, fmt.Errorf("import the index records: %w", err)
	}
	return len(rows), nil
}

// adoptIndex brings the old index directory under the data directory. It moves
// it with a rename, which needs the same filesystem. Otherwise the index is used
// where it is and its path is kept in the key value store.
func adoptIndex(ctx context.Context, indexPath, dataDir string, kv KV) error {
	if indexPath == "" {
		return nil
	}

	info, err := os.Stat(indexPath)
	if err != nil || !info.IsDir() {
		logger.Infof("The old index directory %s is gone, nothing to move", indexPath)
		return nil
	}

	target := filepath.Join(dataDir, "index")
	if same, _ := sameDirectory(indexPath, target); same {
		return nil
	}

	if empty, err := isEmptyDir(target); err != nil {
		return err
	} else if !empty {
		logger.Warnf("The index directory %s already holds data, the old index in %s stays where it is", target, indexPath)
		return nil
	}
	// A rename needs the target to be absent.
	if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("prepare the index directory: %w", err)
	}

	if err := os.Rename(indexPath, target); err == nil {
		logger.Infof("Moved the index from %s to %s", indexPath, target)
		if kv != nil {
			_ = kv.KVDelete(ctx, KVIndexPath)
		}
		config.SetIndexPath("")
		return nil
	} else {
		logger.Infof("Cannot move the index from %s (%v), using it in place", indexPath, err)
	}

	// The target was removed above, so the plugin can create it again on demand.
	if kv != nil {
		if err := kv.KVSet(ctx, KVIndexPath, indexPath); err != nil {
			return fmt.Errorf("remember the index location: %w", err)
		}
	}
	config.SetIndexPath(indexPath)
	return nil
}

// adoptGeoLite brings the old city database into the geolite directory, by
// rename when both sit on the same filesystem and by copy otherwise.
func adoptGeoLite(path string) error {
	if path == "" {
		return nil
	}

	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}

	dir := config.GeoLiteDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	target := filepath.Join(dir, filepath.Base(path))
	if same, _ := sameFile(path, target); same {
		return nil
	}
	if _, err := os.Stat(target); err == nil {
		// A database is already installed, keep it.
		return nil
	}

	if err := os.Rename(path, target); err == nil {
		return nil
	}
	return copyFile(path, target)
}

func copyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := target + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, target)
}

func isEmptyDir(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	return len(entries) == 0, nil
}

func sameDirectory(a, b string) (bool, error) {
	infoA, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	infoB, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	return os.SameFile(infoA, infoB), nil
}

func sameFile(a, b string) (bool, error) { return sameDirectory(a, b) }

// ResolveKV reads the index location a previous handoff stored, and applies it
// when the directory is still there. It runs on every start.
func ResolveKV(ctx context.Context, kv KV) {
	if kv == nil {
		return
	}

	var stored string
	found, err := kv.KVGet(ctx, KVIndexPath, &stored)
	if err != nil || !found || stored == "" {
		return
	}
	if info, err := os.Stat(stored); err != nil || !info.IsDir() {
		logger.Warnf("The stored index location %s is not available", stored)
		return
	}
	config.SetIndexPath(stored)
}
