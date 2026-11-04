package indexer

import (
	"errors"
	"io/fs"
	"os"
	"sort"
)

// configuredGroupsLocked returns the main log paths of every log the host
// lists. The caller must hold cacheMutex.
func (lm *LogFileManager) configuredGroupsLocked() map[string]bool {
	groups := make(map[string]bool, len(lm.logCache))
	for path := range lm.logCache {
		groups[getMainLogPathFromFile(path)] = true
	}
	return groups
}

// isStaleGroup reports whether a log group recorded in the index metadata is
// gone: the host no longer lists it and its main log file does not exist. A log
// still on disk keeps its group even when the host does not list it, and so does
// a listed log whose file is temporarily missing. Any error other than "does not
// exist" keeps the group, so a permission problem never drops index data.
func isStaleGroup(mainLogPath string, configuredGroups map[string]bool) bool {
	if configuredGroups[mainLogPath] {
		return false
	}
	_, err := os.Stat(mainLogPath)
	return errors.Is(err, fs.ErrNotExist)
}

// StaleLogGroups returns the main log paths of the log groups in the index
// metadata that are gone (see isStaleGroup), in a stable order. Groups with a
// file that is indexing or queued are left out.
func (lm *LogFileManager) StaleLogGroups() ([]string, error) {
	lm.cacheMutex.RLock()
	configuredGroups := lm.configuredGroupsLocked()
	lm.cacheMutex.RUnlock()

	indexes, err := lm.persistence.GetAllLogIndexes()
	if err != nil {
		return nil, err
	}

	busy := make(map[string]bool)
	candidates := make(map[string]bool)
	lm.indexingMutex.RLock()
	for _, idx := range indexes {
		group := idx.MainLogPath
		if group == "" {
			group = getMainLogPathFromFile(idx.Path)
		}
		if lm.indexingStatus[idx.Path] || lm.indexingStatus[group] ||
			idx.IndexStatus == string(IndexStatusIndexing) || idx.IndexStatus == string(IndexStatusQueued) {
			busy[group] = true
		}
		candidates[group] = true
	}
	lm.indexingMutex.RUnlock()

	var stale []string
	for group := range candidates {
		if !busy[group] && isStaleGroup(group, configuredGroups) {
			stale = append(stale, group)
		}
	}
	sort.Strings(stale)
	return stale, nil
}

// RemoveLogGroup deletes the index documents and the index metadata of a log
// group. Log files on disk are never touched. When deleting the documents fails
// the metadata stays, because it lists the files whose documents remain. Every
// shard has to be open, see DeleteIndexByLogGroup.
func (lm *LogFileManager) RemoveLogGroup(mainLogPath string) error {
	if lm.indexer != nil {
		if err := lm.indexer.DeleteIndexByLogGroup(mainLogPath, lm); err != nil {
			return err
		}
	}
	return lm.DeleteIndexMetadataByGroup(mainLogPath)
}
