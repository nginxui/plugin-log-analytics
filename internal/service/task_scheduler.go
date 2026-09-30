package service

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nginxui/plugin-log-analytics/internal/cgroup"
	"github.com/nginxui/plugin-log-analytics/internal/config"
	"github.com/nginxui/plugin-log-analytics/internal/hub"
	"github.com/nginxui/plugin-log-analytics/internal/indexer"
	"github.com/nginxui/plugin-log-analytics/internal/logger"
)

// TaskScheduler manages all indexing tasks (recovery, manual rebuild, etc.)
// with unified locking to prevent concurrent execution on the same log group
type TaskScheduler struct {
	logFileManager *indexer.LogFileManager
	modernIndexer  *indexer.ParallelIndexer
	activeTasks    int32 // Counter for active tasks
	ctx            context.Context
	cancel         context.CancelFunc
	wg             sync.WaitGroup
	taskLocks      map[string]*sync.Mutex // Per-log-group locks
	locksMutex     sync.RWMutex           // Protects taskLocks map

	// runSlots bounds how many log groups may be indexed at the same time.
	//
	// The per-log-group locks only stop the same group from running twice; without
	// this semaphore a rebuild schedules one goroutine per log group and every one
	// of them runs concurrently. Each of those in turn fans out to
	// FileGroupConcurrency files, and each file holds a parse batch plus a
	// buffered index batch, so peak memory is
	// groups x files x batches - unbounded in the number of configured sites.
	// That is what makes a post-upgrade full rebuild exhaust RAM and saturate the
	// CPU on a small container (issue #1792).
	//
	// The channel holds the running groups, its capacity is a ceiling. The limit
	// itself is read from the settings on every attempt, so a changed setting
	// applies to the groups that start after it.
	runSlots chan struct{}
	slotMu   sync.Mutex
	slotWake chan struct{} // closed when a slot is released or the limit changed
}

// maxRunSlots is the capacity of the slot channel, the highest limit a setting
// can reach.
const maxRunSlots = 64

// defaultMaxConcurrentIndexTasks caps the auto-derived log group concurrency.
// Indexing is dominated by Bleve segment building and disk I/O, so more
// concurrent groups mostly buys extra resident memory.
const defaultMaxConcurrentIndexTasks = 2

// maxConcurrentIndexTasks returns how many log groups may be indexed
// concurrently. The value is derived from the CPU budget the process is
// actually allowed to use, and can be overridden through settings.
func maxConcurrentIndexTasks() int {
	if configured := config.Get().MaxConcurrentIndexTasks; configured > 0 {
		return configured
	}

	slots := cgroup.AvailableCPUs() / 2
	if slots < 1 {
		slots = 1
	}
	if slots > defaultMaxConcurrentIndexTasks {
		slots = defaultMaxConcurrentIndexTasks
	}
	return slots
}

// Global task scheduler instance
var (
	globalTaskScheduler      *TaskScheduler
	taskSchedulerInitialized bool
	taskSchedulerMutex       sync.RWMutex
)

// NotifySettingsChanged applies changed plugin settings to the running services.
func NotifySettingsChanged() {
	if scheduler := GetTaskScheduler(); scheduler != nil {
		scheduler.SlotLimitChanged()
	}
}

// GetTaskScheduler returns the global task scheduler instance
func GetTaskScheduler() *TaskScheduler {
	taskSchedulerMutex.RLock()
	defer taskSchedulerMutex.RUnlock()
	return globalTaskScheduler
}

// InitTaskScheduler initializes the global task scheduler
func InitTaskScheduler(ctx context.Context) {
	taskSchedulerMutex.RLock()
	alreadyInitialized := taskSchedulerInitialized
	taskSchedulerMutex.RUnlock()

	if alreadyInitialized {
		logger.Debug("Task scheduler already initialized")
		return
	}

	logger.Debug("Initializing task scheduler")

	// Wait a bit for services to fully initialize. The lock is deliberately not
	// held across this wait or across task recovery below: both take the
	// services lock through GetLogFileManager, and callers such as a manual
	// rebuild only need to read the scheduler pointer.
	//
	// The wait watches the context so that services stopped shortly after they
	// started - advanced indexing switched off again, or a shutdown - do not get
	// a scheduler that starts recovery against services that are already gone.
	select {
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		logger.Debug("Task scheduler initialization cancelled before services became ready")
		return
	}

	// Check if services are available
	if GetLogFileManager() == nil || GetIndexer() == nil {
		logger.Debug("Modern services not available, skipping task scheduler initialization")
		return
	}

	scheduler := NewTaskScheduler(ctx)

	taskSchedulerMutex.Lock()
	if taskSchedulerInitialized {
		taskSchedulerMutex.Unlock()
		scheduler.cancel()
		logger.Debug("Task scheduler initialized concurrently, discarding duplicate")
		return
	}
	globalTaskScheduler = scheduler
	taskSchedulerInitialized = true
	taskSchedulerMutex.Unlock()

	// Start task recovery
	if err := scheduler.RecoverUnfinishedTasks(ctx); err != nil {
		logger.Errorf("Failed to recover unfinished tasks: %v", err)
	}

	// Monitor context for shutdown. Capture the scheduler in the closure rather
	// than reading the global: StopServices clears the global, and reading it
	// here without the mutex would be a data race against that write.
	go func() {
		<-ctx.Done()
		scheduler.Shutdown()
	}()
}

// resetTaskSchedulerState clears the global scheduler registration and drains
// the previously registered scheduler, so a later InitTaskScheduler call builds
// a new scheduler against freshly created services. It must be called without
// the services lock held: draining waits for in-flight indexing tasks, and
// those reach back into the services.
func resetTaskSchedulerState() {
	taskSchedulerMutex.Lock()
	previous := globalTaskScheduler
	globalTaskScheduler = nil
	taskSchedulerInitialized = false
	taskSchedulerMutex.Unlock()

	if previous != nil {
		previous.Shutdown()
	}
}

// NewTaskScheduler creates a new task scheduler
func NewTaskScheduler(parentCtx context.Context) *TaskScheduler {
	ctx, cancel := context.WithCancel(parentCtx)
	logger.Debugf("Task scheduler limiting concurrent log group indexing to %d", maxConcurrentIndexTasks())
	return &TaskScheduler{
		logFileManager: GetLogFileManager(),
		modernIndexer:  GetIndexer(),
		ctx:            ctx,
		cancel:         cancel,
		taskLocks:      make(map[string]*sync.Mutex),
		runSlots:       make(chan struct{}, maxRunSlots),
	}
}

// slotLimit is how many log groups may run now: the setting, at most the
// capacity of the slot channel.
func (ts *TaskScheduler) slotLimit() int {
	return min(maxConcurrentIndexTasks(), cap(ts.runSlots))
}

// wakeSlotWaiters lets the tasks waiting for a slot look again. The caller
// holds slotMu.
func (ts *TaskScheduler) wakeSlotWaiters() {
	if ts.slotWake != nil {
		close(ts.slotWake)
	}
	ts.slotWake = make(chan struct{})
}

// SlotLimitChanged tells the scheduler that the setting changed, so waiting
// tasks re-read it.
func (ts *TaskScheduler) SlotLimitChanged() {
	ts.slotMu.Lock()
	defer ts.slotMu.Unlock()
	ts.wakeSlotWaiters()
}

// acquireRunSlot blocks until a global indexing slot is free. It returns false
// when the scheduler or the caller's context is cancelled while waiting.
func (ts *TaskScheduler) acquireRunSlot(ctx context.Context) bool {
	if ts.runSlots == nil {
		return true
	}

	for {
		ts.slotMu.Lock()
		if len(ts.runSlots) < ts.slotLimit() {
			ts.runSlots <- struct{}{}
			ts.slotMu.Unlock()
			return true
		}
		if ts.slotWake == nil {
			ts.slotWake = make(chan struct{})
		}
		wake := ts.slotWake
		ts.slotMu.Unlock()

		select {
		case <-wake:
		case <-ctx.Done():
			return false
		case <-ts.ctx.Done():
			return false
		}
	}
}

// releaseRunSlot returns a slot acquired by acquireRunSlot.
func (ts *TaskScheduler) releaseRunSlot() {
	if ts.runSlots == nil {
		return
	}

	ts.slotMu.Lock()
	defer ts.slotMu.Unlock()
	select {
	case <-ts.runSlots:
	default:
	}
	ts.wakeSlotWaiters()
}

// acquireTaskLock gets or creates a mutex for a specific log group
func (ts *TaskScheduler) acquireTaskLock(logPath string) *sync.Mutex {
	ts.locksMutex.Lock()
	defer ts.locksMutex.Unlock()

	if lock, exists := ts.taskLocks[logPath]; exists {
		return lock
	}

	lock := &sync.Mutex{}
	ts.taskLocks[logPath] = lock
	return lock
}

// releaseTaskLock removes the mutex for a specific log group after completion
func (ts *TaskScheduler) releaseTaskLock(logPath string) {
	ts.locksMutex.Lock()
	defer ts.locksMutex.Unlock()
	delete(ts.taskLocks, logPath)
}

// IsTaskInProgress checks if a task is currently running for a specific log group
func (ts *TaskScheduler) IsTaskInProgress(logPath string) bool {
	ts.locksMutex.RLock()
	defer ts.locksMutex.RUnlock()

	if lock, exists := ts.taskLocks[logPath]; exists {
		// Try to acquire the lock with TryLock
		// If we can't acquire it, it means task is in progress
		if lock.TryLock() {
			lock.Unlock()
			return false
		}
		return true
	}
	return false
}

// AcquireTaskLock acquires a lock for external use (e.g., manual rebuild)
// Returns the lock and a release function
func (ts *TaskScheduler) AcquireTaskLock(logPath string) (*sync.Mutex, func()) {
	lock := ts.acquireTaskLock(logPath)
	lock.Lock()

	releaseFunc := func() {
		lock.Unlock()
		ts.releaseTaskLock(logPath)
	}

	return lock, releaseFunc
}

// ScheduleIndexTask schedules an indexing task for a log group
// Returns error if task is already in progress
func (ts *TaskScheduler) ScheduleIndexTask(ctx context.Context, logPath string, progressConfig *indexer.ProgressConfig) error {
	// Check if task is already in progress
	if ts.IsTaskInProgress(logPath) {
		return fmt.Errorf("indexing task already in progress for %s", logPath)
	}

	// Queue the task asynchronously with proper context and WaitGroup
	ts.wg.Add(1)
	go ts.executeIndexTask(ctx, logPath, progressConfig)

	return nil
}

// executeIndexTask executes an indexing task with proper locking and progress tracking
func (ts *TaskScheduler) executeIndexTask(ctx context.Context, logPath string, progressConfig *indexer.ProgressConfig) {
	defer ts.wg.Done() // Always decrement WaitGroup

	// Acquire lock for this specific log group to prevent concurrent execution
	lock := ts.acquireTaskLock(logPath)
	lock.Lock()
	defer func() {
		lock.Unlock()
		ts.releaseTaskLock(logPath)
	}()

	// Check context before starting
	select {
	case <-ctx.Done():
		logger.Debugf("Context cancelled, skipping task for %s", logPath)
		return
	default:
	}

	// Wait for a global slot before touching the indexer. Indexing a log group
	// fans out over its rotated files and buffers a batch per file, so the number
	// of groups running at once has to be bounded or a full rebuild across many
	// sites allocates without limit.
	if !ts.acquireRunSlot(ctx) {
		logger.Debugf("Context cancelled while waiting for an indexing slot: %s", logPath)
		return
	}
	defer ts.releaseRunSlot()

	// The wait can be long; re-check cancellation before doing the work.
	select {
	case <-ctx.Done():
		logger.Debugf("Context cancelled, skipping task for %s", logPath)
		return
	default:
	}

	logger.Debugf("Executing indexing task: %s", logPath)

	// The round keeps the indexing indicator on, and the parser and the shards
	// alive, until the last running task ends.
	atomic.AddInt32(&ts.activeTasks, 1)
	endRound := BeginRound(true)

	// Ensure we always end the round when the task is done
	defer func() {
		atomic.AddInt32(&ts.activeTasks, -1)
		endRound()
		if r := recover(); r != nil {
			logger.Errorf("Panic during task execution: %v", r)
		}
	}()

	// Set status to indexing
	if err := ts.setTaskStatus(logPath, string(indexer.IndexStatusIndexing), 0); err != nil {
		logger.Errorf("Failed to set indexing status for %s: %v", logPath, err)
		return
	}

	// Execute the indexing with progress tracking. A group that was indexed
	// before continues from its stored state.
	startTime := time.Now()
	group, err := ts.modernIndexer.SyncLogGroup(logPath, progressConfig)

	if err != nil {
		logger.Errorf("Failed to execute indexing task %s: %v", logPath, err)
		// Set error status
		if statusErr := ts.setTaskStatus(logPath, string(indexer.IndexStatusError), 0); statusErr != nil {
			logger.Errorf("Failed to set error status for %s: %v", logPath, statusErr)
		}
		return
	}

	// Save indexing metadata using the log file manager
	var totalDocsIndexed uint64
	if group == nil {
		group = &indexer.GroupSyncResult{}
	}
	totalDocsIndexed, err = saveGroupMetadata(ts.logFileManager, logPath, group, startTime, time.Since(startTime))
	if err != nil {
		logger.Errorf("Failed to save index metadata for %s: %v", logPath, err)
	}

	// Set status to indexed (completed)
	if err := ts.setTaskStatus(logPath, string(indexer.IndexStatusIndexed), 0); err != nil {
		logger.Errorf("Failed to set completed status for %s: %v", logPath, err)
	}

	// Update searcher shards
	UpdateSearcherShards()

	logger.Debugf("Successfully completed indexing task: %s, Documents: %d", logPath, totalDocsIndexed)
}

// RecoverUnfinishedTasks recovers indexing tasks that were incomplete at last shutdown
func (ts *TaskScheduler) RecoverUnfinishedTasks(ctx context.Context) error {
	if ts.logFileManager == nil || ts.modernIndexer == nil {
		logger.Warn("Cannot recover tasks: services not available")
		return nil
	}

	logger.Debug("Starting recovery of unfinished indexing tasks")

	// Get all logs with their index status
	allLogs := GetAllLogsWithIndexGrouped(func(log *NginxLogWithIndex) bool {
		// Only process access logs
		return log.Type == "access"
	})

	var incompleteTasksCount int
	var queuePosition int = 1

	for _, log := range allLogs {
		// Stop scheduling as soon as the services are being torn down, so a
		// disable/shutdown does not keep queueing work against them.
		select {
		case <-ctx.Done():
			return nil
		case <-ts.ctx.Done():
			return nil
		default:
		}

		if ts.needsRecovery(log) {
			incompleteTasksCount++

			// Reset to queued status and assign queue position
			if err := ts.recoverTask(ctx, log.Path, queuePosition); err != nil {
				logger.Errorf("Failed to recover task for %s: %v", log.Path, err)
			} else {
				queuePosition++
			}
		}
	}

	if incompleteTasksCount > 0 {
		logger.Debugf("Recovered %d incomplete indexing tasks", incompleteTasksCount)
	} else {
		logger.Debug("No incomplete indexing tasks found")
	}

	return nil
}

// needsRecovery determines if a log file has an incomplete indexing task that needs recovery
func (ts *TaskScheduler) needsRecovery(log *NginxLogWithIndex) bool {
	// Check for incomplete states that indicate interrupted operations
	switch log.IndexStatus {
	case string(indexer.IndexStatusIndexing):
		// Task was in progress during last shutdown
		logger.Debugf("Found incomplete indexing task: %s", log.Path)
		return true

	case string(indexer.IndexStatusQueued):
		// Task was queued but may not have started
		logger.Debugf("Found queued indexing task: %s", log.Path)
		return true

	case string(indexer.IndexStatusError):
		// Check if error is recent (within last hour before restart)
		if log.LastIndexed > 0 {
			lastIndexTime := time.Unix(log.LastIndexed, 0)
			if time.Since(lastIndexTime) < time.Hour {
				logger.Debugf("Found recent error task for retry: %s", log.Path)
				return true
			}
		}

	case string(indexer.IndexStatusNotIndexed):
		// Newly enabled advanced indexing or a log path that has never been indexed.
		// Schedule an initial indexing task so users do not have to wait for the
		// incremental cron job to discover it.
		logger.Debugf("Found unindexed log for initial indexing: %s", log.Path)
		return true
	}

	return false
}

// recoverTask recovers a single indexing task
func (ts *TaskScheduler) recoverTask(ctx context.Context, logPath string, queuePosition int) error {
	// Check if task is already in progress
	if ts.IsTaskInProgress(logPath) {
		logger.Debugf("Skipping recovery for %s - task already in progress", logPath)
		return nil
	}

	logger.Debugf("Recovering indexing task for: %s (queue position: %d)", logPath, queuePosition)

	// Set status to queued with queue position
	if err := ts.setTaskStatus(logPath, string(indexer.IndexStatusQueued), queuePosition); err != nil {
		return err
	}

	// Add a small delay to stagger recovery tasks, but give up as soon as the
	// services are being torn down.
	select {
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
		logger.Debugf("Recovery cancelled while staggering task for %s", logPath)
		return nil
	case <-ts.ctx.Done():
		logger.Debugf("Recovery cancelled while staggering task for %s", logPath)
		return nil
	}

	// Create recovery progress config
	progressConfig := ts.createProgressConfig()

	// Schedule the task
	return ts.ScheduleIndexTask(ctx, logPath, progressConfig)
}

// createProgressConfig creates a standard progress configuration
func (ts *TaskScheduler) createProgressConfig() *indexer.ProgressConfig {
	return &indexer.ProgressConfig{
		NotifyInterval: 1 * time.Second,
		OnProgress: func(progress indexer.ProgressNotification) {
			// Send progress event to frontend
			events.Publish(hub.Event{
				Type: hub.TypeNginxLogIndexProgress,
				Data: hub.NginxLogIndexProgressData{
					LogPath:         progress.LogGroupPath,
					Progress:        progress.Percentage,
					Stage:           "indexing",
					Status:          "running",
					ElapsedTime:     progress.ElapsedTime.Milliseconds(),
					EstimatedRemain: progress.EstimatedRemain.Milliseconds(),
				},
			})

			logger.Debugf("Indexing progress: %s - %.1f%% (Files: %d/%d, Lines: %d/%d)",
				progress.LogGroupPath, progress.Percentage, progress.CompletedFiles,
				progress.TotalFiles, progress.ProcessedLines, progress.EstimatedLines)
		},
		OnCompletion: func(completion indexer.CompletionNotification) {
			// Send completion event to frontend
			events.Publish(hub.Event{
				Type: hub.TypeNginxLogIndexComplete,
				Data: hub.NginxLogIndexCompleteData{
					LogPath:     completion.LogGroupPath,
					Success:     completion.Success,
					Duration:    int64(completion.Duration.Milliseconds()),
					TotalLines:  completion.TotalLines,
					IndexedSize: completion.IndexedSize,
					Error:       completion.Error,
				},
			})

			logger.Debugf("Indexing completion: %s - Success: %t, Duration: %s, Lines: %d, Size: %d bytes",
				completion.LogGroupPath, completion.Success, completion.Duration,
				completion.TotalLines, completion.IndexedSize)

			// Send index ready event if indexing was successful
			if completion.Success {
				events.Publish(hub.Event{
					Type: hub.TypeNginxLogIndexReady,
					Data: hub.NginxLogIndexReadyData{
						LogPath:     completion.LogGroupPath,
						StartTime:   time.Now().Unix(),
						EndTime:     time.Now().Unix(),
						Available:   true,
						IndexStatus: "ready",
					},
				})
			}
		},
	}
}

// setTaskStatus updates the task status in the database
func (ts *TaskScheduler) setTaskStatus(logPath, status string, queuePosition int) error {
	// Get persistence manager
	persistence := ts.logFileManager.GetPersistence()
	if persistence == nil {
		return fmt.Errorf("persistence manager not available")
	}

	// Use enhanced SetIndexStatus method
	return persistence.SetIndexStatus(logPath, status, queuePosition, "")
}

// Shutdown gracefully stops all tasks
func (ts *TaskScheduler) Shutdown() {
	logger.Debug("Shutting down task scheduler...")

	// Cancel all active tasks
	ts.cancel()

	// Wait for all tasks to complete with timeout
	done := make(chan struct{})
	go func() {
		ts.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		logger.Debug("All tasks completed successfully")
	case <-time.After(30 * time.Second):
		logger.Warn("Timeout waiting for tasks to complete")
	}

	logger.Debug("Task scheduler shutdown completed")
}
