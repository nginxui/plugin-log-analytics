package service

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/nginxui/plugin-log-analytics/internal/config"
	"github.com/nginxui/plugin-log-analytics/internal/indexer"
	"github.com/nginxui/plugin-log-analytics/internal/logger"
	"github.com/nginxui/plugin-log-analytics/internal/store"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// incrementalStartDelay is how long the first incremental round waits after the
// scheduler starts, so the host has listed the log files by then.
var incrementalStartDelay = 15 * time.Second

// IncrementalScheduler runs the periodic incremental indexing round. It keeps
// the rounds from overlapping and follows changes of the interval setting.
type IncrementalScheduler struct {
	cancel context.CancelFunc
	done   chan struct{}
	reset  chan struct{}
}

// StartIncrementalScheduler starts the periodic incremental indexing job. The
// interval is read from the plugin settings after every round, and Reset makes
// a changed setting take effect at once.
func StartIncrementalScheduler(ctx context.Context) *IncrementalScheduler {
	logger.Info("Setting up incremental log indexing job")

	runCtx, cancel := context.WithCancel(ctx)
	scheduler := &IncrementalScheduler{
		cancel: cancel,
		done:   make(chan struct{}),
		reset:  make(chan struct{}, 1),
	}
	go scheduler.run(runCtx)
	return scheduler
}

// Reset makes the scheduler pick up a new interval.
func (s *IncrementalScheduler) Reset() {
	select {
	case s.reset <- struct{}{}:
	default:
	}
}

// Stop stops the scheduler and waits for a running round to finish, at most
// until ctx ends. A round cannot be interrupted, so a caller that cannot wait
// stops the services under it instead.
func (s *IncrementalScheduler) Stop(ctx context.Context) {
	s.cancel()
	select {
	case <-s.done:
	case <-ctx.Done():
	}
}

func (s *IncrementalScheduler) run(ctx context.Context) {
	defer close(s.done)

	wait := incrementalStartDelay
	for {
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.reset:
			timer.Stop()
			wait = config.Get().IncrementalInterval()
			logger.Infof("Incremental log indexing job scheduled to run every %s", wait)
			continue
		case <-timer.C:
		}

		// Rounds run one after the other on this goroutine, which is what keeps
		// them from overlapping.
		performIncrementalIndexing()
		wait = config.Get().IncrementalInterval()
	}
}

// performIncrementalIndexing performs the actual incremental indexing check
func performIncrementalIndexing() {
	logger.Debug("Starting incremental log indexing scan")

	// Get log file manager
	logFileManager := GetLogFileManager()
	if logFileManager == nil {
		logger.Warn("Log file manager not available for incremental indexing")
		return
	}

	persistence := logFileManager.GetPersistence()
	if persistence == nil {
		logger.Warn("Persistence manager not available for incremental indexing")
		return
	}

	// Get modern indexer
	modernIndexer := GetIndexer()
	if modernIndexer == nil {
		logger.Warn("Modern indexer not available for incremental indexing")
		return
	}

	// Check if indexer is healthy
	if !modernIndexer.IsHealthy() {
		logger.Warn("Modern indexer is not healthy, skipping incremental indexing")
		return
	}

	// Get all log groups to check for changes
	allLogs := GetAllLogsWithIndexGrouped(func(log *NginxLogWithIndex) bool {
		// Only process access logs (skip error logs as they are not indexed)
		return log.Type == "access"
	})

	// Decide first, so a round with nothing to do opens neither the parser nor
	// a shard.
	pending := make([]*NginxLogWithIndex, 0, len(allLogs))
	for _, log := range allLogs {
		if needsIncrementalIndexing(log, persistence) {
			pending = append(pending, log)
		}
	}
	if len(pending) == 0 {
		logger.Debug("No log files need incremental indexing")
		return
	}

	endRound := BeginRound(false)
	defer endRound()

	// Process files sequentially to avoid overwhelming the system
	// This is more conservative but prevents concurrent file indexing from consuming too much CPU
	changedCount := 0
	for _, log := range pending {
		logger.Debugf("Starting incremental indexing for file: %s", log.Path)

		// Set status to indexing
		if err := setFileIndexStatus(log.Path, string(indexer.IndexStatusIndexing), logFileManager); err != nil {
			logger.Errorf("Failed to set indexing status for %s: %v", log.Path, err)
			continue
		}

		// Perform incremental indexing synchronously (one file at a time)
		if err := performGroupIncrementalIndexing(log.Path, modernIndexer, logFileManager); err != nil {
			logger.Errorf("Failed incremental indexing for %s: %v", log.Path, err)
			// Set error status
			if statusErr := setFileIndexStatus(log.Path, string(indexer.IndexStatusError), logFileManager); statusErr != nil {
				logger.Errorf("Failed to set error status for %s: %v", log.Path, statusErr)
			}
		} else {
			changedCount++
			// Set status to indexed
			if err := setFileIndexStatus(log.Path, string(indexer.IndexStatusIndexed), logFileManager); err != nil {
				logger.Errorf("Failed to set indexed status for %s: %v", log.Path, err)
			}
		}
	}

	if changedCount > 0 {
		logger.Debugf("Completed incremental indexing for %d log files", changedCount)
		// Update searcher shards once after all files are processed
		UpdateSearcherShards()
	}
}

// groupStateProvider gives the stored state of the files of a log group.
type groupStateProvider interface {
	GetLogIndexesByGroup(mainLogPath string) ([]*store.NginxLogIndex, error)
}

// needsIncrementalIndexing checks if a log group has a file that has to be read.
// The group is the main log path with its rotated files, and each of them is
// compared with its own stored state, so a rotated or compressed file that was
// never read, and a live file that was replaced, both count.
func needsIncrementalIndexing(log *NginxLogWithIndex, persistence groupStateProvider) bool {
	// Only files the host lists, and their rotated files, are read
	if !utils.IsValidLogPath(log.Path) {
		return false
	}

	// Skip if already indexing or queued
	if log.IndexStatus == string(indexer.IndexStatusIndexing) ||
		log.IndexStatus == string(indexer.IndexStatusQueued) {
		return false
	}

	files, err := indexer.GroupFiles(log.Path)
	if err != nil {
		logger.Warnf("Cannot list the files of %s: %v", log.Path, err)
		return false
	}
	if len(files) == 0 {
		// Nothing on disk, but the index still answers for what was read before.
		return false
	}

	rows := make(map[string]*store.NginxLogIndex, len(files))
	if persistence != nil {
		stored, err := persistence.GetLogIndexesByGroup(log.Path)
		if err != nil {
			logger.Debugf("Could not load persisted metadata for %s: %v", log.Path, err)
		}
		for _, row := range stored {
			rows[row.Path] = row
		}
	}

	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			logger.Warnf("Cannot stat file %s: %v", file, err)
			continue
		}
		if indexer.NeedsSync(info, rows[file]) && !indexer.Throttled(info, rows[file]) {
			logger.Debugf("File %s needs incremental indexing", file)
			return true
		}
	}
	return false
}

// performGroupIncrementalIndexing indexes what is new in a log group and stores
// the metadata of every file that was read. Each file continues from its own
// stored position. A file that is a renamed, copied or compressed copy of one
// that was read before continues from that file's position, and a file that was
// replaced is read from its start. The documents have ids that follow the
// content, so none is added twice and none is lost.
func performGroupIncrementalIndexing(basePath string, modernIndexer interface{}, logFileManager interface{}) error {
	defer func() {
		// Ensure status is always updated, even on panic
		if r := recover(); r != nil {
			logger.Errorf("Recovered from panic during incremental indexing for %s: %v", basePath, r)
			_ = setFileIndexStatus(basePath, string(indexer.IndexStatusError), logFileManager)
		}
	}()

	lfm, ok := logFileManager.(*indexer.LogFileManager)
	if !ok {
		return fmt.Errorf("invalid log file manager type")
	}
	pi, ok := modernIndexer.(*indexer.ParallelIndexer)
	if !ok {
		return fmt.Errorf("invalid indexer type")
	}
	if !utils.IsValidLogPath(basePath) {
		return fmt.Errorf("log path is not a readable log file: %s", basePath)
	}

	startTime := time.Now()
	group, err := pi.SyncLogGroup(basePath, nil)
	if err != nil {
		return fmt.Errorf("indexing failed: %w", err)
	}
	if group == nil || len(group.Files) == 0 {
		return nil
	}
	total, err := saveGroupMetadata(lfm, basePath, group, startTime, time.Since(startTime))
	if err != nil {
		return err
	}

	logger.Debugf("Incremental indexing completed: %s, files=%d, new_docs=%d", basePath, len(group.Files), total)
	return nil
}

// saveGroupMetadata stores the metadata of the files of a group that were read,
// then the record of the group, which carries the count of the whole group. It
// returns the number of documents written by the round.
func saveGroupMetadata(lfm *indexer.LogFileManager, basePath string, group *indexer.GroupSyncResult, startTime time.Time, duration time.Duration) (uint64, error) {
	var total uint64
	for path, file := range group.Files {
		total += file.Docs
		if path == basePath {
			continue
		}
		if err := lfm.SaveIndexMetadata(path, file.Docs, startTime, duration, file.MinTime, file.MaxTime); err != nil {
			logger.Errorf("Failed to save index metadata for %s: %v", path, err)
		}
	}
	if err := lfm.SaveIndexMetadata(basePath, total, startTime, duration, group.MinTime, group.MaxTime); err != nil {
		return total, fmt.Errorf("failed to save metadata: %w", err)
	}
	return total, nil
}

// setFileIndexStatus updates the index status for a file in the database using enhanced status management
func setFileIndexStatus(logPath, status string, logFileManager interface{}) error {
	if logFileManager == nil {
		return fmt.Errorf("log file manager not available")
	}

	// Get persistence manager
	lfm, ok := logFileManager.(*indexer.LogFileManager)
	if !ok {
		return fmt.Errorf("invalid log file manager type")
	}
	persistence := lfm.GetPersistence()
	if persistence == nil {
		return fmt.Errorf("persistence manager not available")
	}

	// Use enhanced SetIndexStatus method with queue position for queued status
	queuePosition := 0
	if status == string(indexer.IndexStatusQueued) {
		// For incremental indexing, we don't need specific queue positions
		// They will be processed as they come
		queuePosition = int(time.Now().Unix() % 1000) // Simple ordering by time
	}

	return persistence.SetIndexStatus(logPath, status, queuePosition, "")
}
