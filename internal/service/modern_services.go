package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nginxui/plugin-log-analytics/internal/analytics"
	"github.com/nginxui/plugin-log-analytics/internal/config"
	"github.com/nginxui/plugin-log-analytics/internal/hub"
	"github.com/nginxui/plugin-log-analytics/internal/indexer"
	"github.com/nginxui/plugin-log-analytics/internal/logger"
	"github.com/nginxui/plugin-log-analytics/internal/searcher"
	"github.com/nginxui/plugin-log-analytics/internal/utils"
)

// Global instances for new services
var (
	globalSearcher         *searcher.Searcher
	globalAnalytics        analytics.Service
	globalIndexer          *indexer.ParallelIndexer
	globalLogFileManager   *indexer.LogFileManager
	servicesInitialized    bool
	servicesInitializing   bool
	servicesMutex          sync.RWMutex
	shutdownCancel         context.CancelFunc
	serviceContext         context.Context
	isShuttingDown         bool
	lastShardUpdateAttempt int64
)

// shardsLoaded reports that every existing shard is open and the searcher sees
// all of them. Until a query, or a warm up, asks for it the plugin keeps no
// shard open beyond the group an indexing run is writing to, and keeps no
// searcher at all.
var shardsLoaded atomic.Bool

// events carries the progress of indexing to the /events websocket, and
// processing tracks whether an indexing run is active.
var (
	events     = hub.New()
	processing = hub.NewStatus(events, onIndexingChanged)
)

// Events returns the hub the /events websocket subscribes to.
func Events() *hub.Hub { return events }

// Processing returns the indexing status tracker.
func Processing() *hub.Status { return processing }

// activityHook mirrors the indexing state to the host indicator. It is set by
// main once the host connection is up.
var activityHook atomic.Pointer[func(indexing bool)]

// SetActivityHook installs the function called whenever indexing starts or
// stops.
func SetActivityHook(fn func(indexing bool)) {
	if fn == nil {
		activityHook.Store(nil)
		return
	}
	activityHook.Store(&fn)
}

func onIndexingChanged(indexing bool) {
	if hook := activityHook.Load(); hook != nil {
		(*hook)(indexing)
	}
}

// InitializeServices initializes the modular services. No shard is opened and
// no searcher is built here: both wait for the first query or warm up.
func InitializeServices(ctx context.Context) {
	servicesMutex.Lock()

	if servicesInitialized {
		servicesMutex.Unlock()
		logger.Info("Modern nginx log services already initialized, skipping")
		return
	}

	if servicesInitializing {
		servicesMutex.Unlock()
		logger.Info("Modern nginx log services are initializing, skipping duplicate request")
		return
	}

	servicesInitializing = true
	servicesMutex.Unlock()

	logger.Info("Initializing modern nginx log services...")

	// Create a cancellable context for services
	serviceCtx, cancel := context.WithCancel(ctx)

	indexerInstance, logFileManagerInstance, err := initializeWithDefaults(serviceCtx)
	if err != nil {
		cancel()
		servicesMutex.Lock()
		servicesInitializing = false
		servicesMutex.Unlock()
		logger.Errorf("Failed to initialize modern services: %v", err)
		markReady(ErrModernIndexerNotAvailable)
		return
	}

	servicesMutex.Lock()
	globalIndexer = indexerInstance
	globalLogFileManager = logFileManagerInstance
	shutdownCancel = cancel
	serviceContext = serviceCtx
	servicesInitialized = true
	servicesInitializing = false
	servicesMutex.Unlock()

	// Seed the brand-new manager with the log paths the host listed. The list
	// is kept apart from the manager, which is created empty on every start.
	if seeded := seedLogFileManager(logFileManagerInstance); seeded > 0 {
		logger.Infof("Seeded %d nginx log path(s) from the host into the log file manager", seeded)
	} else {
		logger.Info("The host listed no nginx log path yet; it lists them again when they change")
	}

	logger.Info("Modern nginx log services initialization completed")
	markReady(nil)

	// Initialize task scheduler after services are ready
	go InitTaskScheduler(serviceCtx)

	// Release idle shards and caches
	go runIdleWatcher(serviceCtx)

	// Monitor context for shutdown
	go func() {
		logger.Info("Started nginx_log shutdown monitor goroutine")
		<-serviceCtx.Done()
		logger.Info("Context cancelled, initiating shutdown...")

		// A manual stop cancels this context as well, and the services may
		// have been started again by now: stop only the ones it belongs to.
		stopServicesOf(serviceCtx)

		logger.Info("Nginx_log shutdown monitor goroutine completed")
	}()
}

// initializeWithDefaults creates the indexer and the log file manager with
// default configuration.
func initializeWithDefaults(ctx context.Context) (*indexer.ParallelIndexer, *indexer.LogFileManager, error) {
	logger.Info("Initializing services with default configuration")

	// Initialize parallel indexer with shard manager
	indexerConfig := indexer.DefaultIndexerConfig()
	indexerConfig.IndexPath = getIndexPath()
	indexerConfig.LazyShardLoad = true
	indexStorageNeedsReset, err := indexer.PrepareIndexStorage(indexerConfig.IndexPath)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to prepare nginx log index storage: %w", err)
	}
	shardManager := indexer.NewGroupedShardManager(indexerConfig)
	indexerInstance := indexer.NewParallelIndexer(indexerConfig, shardManager)

	// Start the indexer
	if err := indexerInstance.Start(ctx); err != nil {
		logger.Errorf("Failed to start parallel indexer: %v", err)
		return nil, nil, fmt.Errorf("failed to start parallel indexer: %w", err)
	}

	// Initialize log file manager
	logFileManagerInstance := indexer.NewLogFileManager()
	// Inject indexer for precise doc counting before persisting
	logFileManagerInstance.SetIndexer(indexerInstance)
	if indexStorageNeedsReset {
		if err := logFileManagerInstance.DeleteAllIndexMetadata(); err != nil {
			_ = indexerInstance.Stop()
			return nil, nil, fmt.Errorf("failed to reset incompatible nginx log index metadata: %w", err)
		}
		if err := indexer.CommitIndexStorageVersion(indexerConfig.IndexPath); err != nil {
			_ = indexerInstance.Stop()
			return nil, nil, fmt.Errorf("failed to commit nginx log index storage migration: %w", err)
		}
		logger.Warn("Reset incompatible nginx log indexes; a clean rebuild will be scheduled")
	}

	return indexerInstance, logFileManagerInstance, nil
}

// getIndexPath returns the directory of the index shards and makes sure it exists.
func getIndexPath() string {
	indexPath := config.IndexPath()
	if err := os.MkdirAll(indexPath, 0o755); err != nil {
		logger.Warnf("Failed to create index directory at %s: %v", indexPath, err)
	}
	return indexPath
}

// GetSearcher returns the global searcher instance. It is nil until a query or
// a warm up loaded the shards, see AcquireQuery.
func GetSearcher() *searcher.Searcher {
	servicesMutex.RLock()
	defer servicesMutex.RUnlock()

	if !servicesInitialized {
		logger.Warn("Modern services not initialized, returning nil")
		return nil
	}

	if globalSearcher == nil {
		logger.Debug("GetSearcher: no searcher yet, shards are not loaded")
		return nil
	}

	// Check searcher health status
	isHealthy := globalSearcher.IsHealthy()
	isRunning := globalSearcher.IsRunning()
	logger.Debugf("GetSearcher: returning searcher, isHealthy: %v, isRunning: %v", isHealthy, isRunning)

	// Auto-heal: if the searcher is running but unhealthy (likely zero shards),
	// and the indexer is initialized, trigger an async shard swap (throttled).
	if !isHealthy && isRunning && globalIndexer != nil && shardsLoaded.Load() {
		now := time.Now().UnixNano()
		prev := atomic.LoadInt64(&lastShardUpdateAttempt)
		if now-prev > int64(5*time.Second) {
			if atomic.CompareAndSwapInt64(&lastShardUpdateAttempt, prev, now) {
				logger.Debugf("GetSearcher: unhealthy detected, scheduling UpdateSearcherShards()")
				go UpdateSearcherShards()
			}
		}
	}

	return globalSearcher
}

// GetAnalytics returns the global analytics service instance
func GetAnalytics() analytics.Service {
	servicesMutex.RLock()
	defer servicesMutex.RUnlock()

	if !servicesInitialized {
		logger.Warn("Modern services not initialized, returning nil")
		return nil
	}

	return globalAnalytics
}

// GetIndexer returns the global indexer instance
func GetIndexer() *indexer.ParallelIndexer {
	servicesMutex.RLock()
	defer servicesMutex.RUnlock()

	if !servicesInitialized {
		return nil
	}

	return globalIndexer
}

// GetLogFileManager returns the global log file manager instance
func GetLogFileManager() *indexer.LogFileManager {
	servicesMutex.RLock()
	defer servicesMutex.RUnlock()

	if !servicesInitialized {
		// Only warn during actual operations, not during initialization
		return nil
	}

	if globalLogFileManager == nil {
		logger.Warnf("[nginx_log] GetLogFileManager: globalLogFileManager is nil even though servicesInitialized=true")
		return nil
	}

	return globalLogFileManager
}

// NginxLogCache Type aliases for backward compatibility
type NginxLogCache = indexer.NginxLogCache
type NginxLogWithIndex = indexer.NginxLogWithIndex

// Constants for backward compatibility
const (
	IndexStatusIndexed    = string(indexer.IndexStatusIndexed)
	IndexStatusIndexing   = string(indexer.IndexStatusIndexing)
	IndexStatusNotIndexed = string(indexer.IndexStatusNotIndexed)
)

// SetHostLogs records the nginx log files the host listed and hands them to
// the running LogFileManager. It replaces the previous list as a whole, so a
// log the host stopped listing disappears here too. The same list decides which
// files the plugin may read, see utils.IsValidLogPath.
func SetHostLogs(logs []utils.HostLog) {
	utils.SetHostLogs(logs)

	if manager := GetLogFileManager(); manager != nil {
		manager.SetLogPaths(hostLogEntries())
	}
}

// hostLogEntries returns the listed logs as log cache entries.
func hostLogEntries() []NginxLogCache {
	listed := utils.HostLogList()
	entries := make([]NginxLogCache, 0, len(listed))
	for _, log := range listed {
		entries = append(entries, NginxLogCache{
			Path:       log.Path,
			Type:       log.Type,
			Name:       filepath.Base(log.Path),
			ConfigFile: log.ConfigFile,
		})
	}
	return entries
}

// GetAllLogPaths returns all known log paths, optionally filtered
func GetAllLogPaths(filters ...func(*NginxLogCache) bool) []*NginxLogCache {
	if manager := GetLogFileManager(); manager != nil {
		return manager.GetAllLogPaths(filters...)
	}

	// Fallback list
	var logs []*NginxLogCache
	for _, entry := range hostLogEntries() {
		e := entry
		include := true
		for _, f := range filters {
			if !f(&e) {
				include = false
				break
			}
		}
		if include {
			logs = append(logs, &e)
		}
	}
	return logs
}

// GetAllLogsWithIndexGrouped returns logs grouped by their base name
func GetAllLogsWithIndexGrouped(filters ...func(*NginxLogWithIndex) bool) []*NginxLogWithIndex {
	if manager := GetLogFileManager(); manager != nil {
		return manager.GetAllLogsWithIndexGrouped(filters...)
	}

	// Fallback grouping by base log name (handle simple rotation patterns)
	grouped := make(map[string]*NginxLogWithIndex)
	for _, c := range hostLogEntries() {
		base := getBaseLogNameBasic(c.Path)
		if _, ok := grouped[base]; ok {
			// Nothing to aggregate without index metadata
			continue
		}
		grouped[base] = &NginxLogWithIndex{
			Path:        base,
			Type:        c.Type,
			Name:        filepath.Base(base),
			ConfigFile:  c.ConfigFile,
			IndexStatus: IndexStatusNotIndexed,
		}
	}

	// Build slice and apply filters
	keys := make([]string, 0, len(grouped))
	for k := range grouped {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	result := make([]*NginxLogWithIndex, 0, len(keys))
	for _, k := range keys {
		v := grouped[k]
		include := true
		for _, f := range filters {
			if !f(v) {
				include = false
				break
			}
		}
		if include {
			result = append(result, v)
		}
	}
	return result
}

// --- Fallback helpers ---

// getBaseLogNameBasic derives the base log file for a rotated file name.
// It delegates to the canonical implementation so fallback-mode grouping
// matches the MainLogPath persisted by the indexer.
func getBaseLogNameBasic(filePath string) string {
	return utils.MainLogPathFromFile(filePath)
}

// SetIndexingStatus sets the indexing status for a specific file path
func SetIndexingStatus(path string, isIndexing bool) {
	if manager := GetLogFileManager(); manager != nil {
		manager.SetIndexingStatus(path, isIndexing)
	}
}

// GetIndexingFiles returns a list of files currently being indexed
func GetIndexingFiles() []string {
	if manager := GetLogFileManager(); manager != nil {
		return manager.GetIndexingFiles()
	}
	return []string{}
}

// UpdateSearcherShards fetches all shards from the indexer and performs zero-downtime shard updates.
// Uses Bleve IndexAlias.Swap() for atomic shard replacement without recreating the searcher.
// This function is safe for concurrent use and maintains service availability during index rebuilds.
// While the shards are not loaded the call does nothing: the searcher would only see the
// group an indexing run just wrote, and the next query loads them all anyway.
func UpdateSearcherShards() {
	if !shardsLoaded.Load() {
		logger.Debugf("UpdateSearcherShards: shards are not loaded, nothing to update")
		return
	}

	// Schedule async update to avoid blocking indexing operations
	logger.Debugf("UpdateSearcherShards: Scheduling async shard update")
	go updateSearcherShardsAsync()
}

// updateSearcherShardsAsync performs the actual shard update asynchronously
func updateSearcherShardsAsync() {
	// Small delay to let indexing operations complete
	time.Sleep(500 * time.Millisecond)

	logger.Debugf("updateSearcherShardsAsync: Attempting to acquire write lock...")
	servicesMutex.Lock()
	logger.Debugf("updateSearcherShardsAsync: Write lock acquired")
	defer func() {
		logger.Debugf("updateSearcherShardsAsync: Releasing write lock...")
		servicesMutex.Unlock()
	}()
	if !shardsLoaded.Load() {
		// The shards were released while this update waited.
		return
	}
	updateSearcherShardsLocked()
}

// updateSearcherShardsLocked performs the actual update logic assumes the caller holds the lock.
// Uses Bleve IndexAlias.Swap() for zero-downtime shard updates following official best practices.
func updateSearcherShardsLocked() {
	if !servicesInitialized || globalIndexer == nil {
		logger.Warn("Cannot update searcher shards, services not fully initialized.")
		return
	}

	// Check if indexer is healthy before getting shards
	if !globalIndexer.IsHealthy() {
		logger.Warn("Cannot update searcher shards, indexer is not healthy")
		return
	}

	newShards := globalIndexer.GetAllShards()
	logger.Infof("Retrieved %d new shards from indexer for hot-swap update", len(newShards))

	// If no searcher exists yet, create the initial one. An empty shard set is
	// expected before the first indexing task creates a group, and the searcher
	// then answers with empty results.
	if globalSearcher == nil {
		logger.Info("Creating initial searcher with IndexAlias")
		searcherConfig := searcher.DefaultSearcherConfig()
		globalSearcher = searcher.NewSearcher(searcherConfig, newShards)

		if globalSearcher == nil {
			logger.Error("Failed to create initial searcher instance")
			return
		}

		// Create analytics service with the initial searcher
		globalAnalytics = analytics.NewService(globalSearcher)

		isHealthy := globalSearcher.IsHealthy()
		isRunning := globalSearcher.IsRunning()
		logger.Infof("Initial searcher created successfully, isHealthy: %v, isRunning: %v", isHealthy, isRunning)
		return
	}

	// An empty shard set is expected before the first indexing task creates a
	// group. Keep the current searcher state until a usable replacement exists.
	if len(newShards) == 0 {
		logger.Debugf("No index shards available yet; keeping %d current searcher shards", len(globalSearcher.GetShards()))
		return
	}

	// For subsequent updates, use hot-swap through IndexAlias
	// This follows Bleve best practices for zero-downtime index updates
	ds := globalSearcher
	oldShards := ds.GetShards()
	logger.Debugf("updateSearcherShardsLocked: About to call SwapShards...")

	// Perform atomic shard swap using IndexAlias
	if err := ds.SwapShards(newShards); err != nil {
		logger.Errorf("Failed to swap shards atomically: %v", err)
		return
	}
	logger.Debugf("updateSearcherShardsLocked: SwapShards completed successfully")

	logger.Infof("Successfully swapped %d old shards with %d new shards using IndexAlias",
		len(oldShards), len(newShards))

	// Verify searcher health after swap
	isHealthy := globalSearcher.IsHealthy()
	isRunning := globalSearcher.IsRunning()
	logger.Infof("Post-swap searcher status: isHealthy: %v, isRunning: %v", isHealthy, isRunning)

	// Note: We do NOT recreate the analytics service here since the searcher interface
	// remains the same. The analytics service rebuilds its cardinality counter lazily
	// when it detects the searcher's shard set has changed.
}

// StopServices stops all running modern services
func StopServices() {
	// The scheduler registration is cleared outside the services lock: task
	// recovery running inside the scheduler lock takes the services lock, so
	// grabbing them in the opposite order here would deadlock.
	defer resetTaskSchedulerState()

	stopServicesLocked(nil)
}

// stopServicesOf stops the services started with ctx and leaves newer ones
// running.
func stopServicesOf(ctx context.Context) {
	if stopServicesLocked(ctx) {
		resetTaskSchedulerState()
	}
}

// stopServicesLocked tears down the services while holding the services lock.
// With an owner it only stops the services started with that context. It
// reports whether it stopped anything.
func stopServicesLocked(owner context.Context) bool {
	servicesMutex.Lock()
	defer servicesMutex.Unlock()

	if !servicesInitialized {
		logger.Debug("Modern nginx log services not initialized, nothing to stop")
		return false
	}

	if owner != nil && serviceContext != owner {
		logger.Debug("Newer nginx log services are running, leaving them alone")
		return false
	}

	if isShuttingDown {
		logger.Debug("Modern nginx log services already shutting down")
		return false
	}

	logger.Debug("Stopping modern nginx log services...")
	isShuttingDown = true

	// Cancel the service context to trigger graceful shutdown
	if shutdownCancel != nil {
		shutdownCancel()
		// Wait a bit for graceful shutdown
		time.Sleep(500 * time.Millisecond)
	}

	// Stop all services
	if globalIndexer != nil {
		if err := globalIndexer.Stop(); err != nil {
			logger.Errorf("Failed to stop indexer: %v", err)
		}
		globalIndexer = nil
	}

	if globalAnalytics != nil {
		if err := globalAnalytics.Stop(); err != nil {
			logger.Errorf("Failed to stop analytics service: %v", err)
		}
		globalAnalytics = nil
	}

	if globalSearcher != nil {
		if err := globalSearcher.Stop(); err != nil {
			logger.Errorf("Failed to stop searcher: %v", err)
		}
		globalSearcher = nil
	}

	// Release the parser singleton along with the GeoIP handle and the two
	// 10,000-entry caches it owns.
	indexer.ReleaseLogParser()

	// Reset state
	globalLogFileManager = nil
	servicesInitialized = false
	resetReady()
	shutdownCancel = nil
	serviceContext = nil
	isShuttingDown = false
	shardsLoaded.Store(false)

	logger.Debug("Modern nginx log services stopped")
	return true
}

// DestroyAllIndexes completely removes all indexed data from disk.
//
// The indexer restarts on the context of the services, not on the one of the
// caller: a rebuild cancels its own context when it is done, and that would stop
// the workers of the restarted indexer with it.
func DestroyAllIndexes(_ context.Context) error {
	servicesMutex.RLock()
	defer servicesMutex.RUnlock()

	if !servicesInitialized || globalIndexer == nil {
		logger.Debug("Cannot destroy indexes, services not initialized.")
		return fmt.Errorf("services not initialized")
	}

	return globalIndexer.DestroyAllIndexes(serviceContext)
}

// seedLogFileManager copies the host's log list into the given manager. It
// takes the manager as an argument so InitializeServices can seed the instance
// it just created without going through the global getter.
func seedLogFileManager(manager *indexer.LogFileManager) int {
	entries := hostLogEntries()
	manager.SetLogPaths(entries)
	return len(entries)
}
