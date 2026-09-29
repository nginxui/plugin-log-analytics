package service

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/nginxui/plugin-log-analytics/internal/indexer"
	"github.com/nginxui/plugin-log-analytics/internal/logger"
)

// IdleTimeout is how long the plugin keeps shards and query caches without a
// search, a statistics request, a warm up or an indexing run.
var IdleTimeout = 10 * time.Minute

// idleCheckInterval is how often the idle watcher looks.
var idleCheckInterval = 30 * time.Second

// lifecycle tracks who needs the shards. lifecycleMu also serializes loading
// and releasing them.
var (
	lifecycleMu   sync.Mutex
	queryInflight int
	activeRounds  int
	lastActivity  = time.Now()
)

// freeOSMemory hands memory back to the system. It is a variable so tests can
// count the calls.
var freeOSMemory = debug.FreeOSMemory

// AcquireQuery makes sure every existing shard is open and the searcher and
// the analytics service exist, and keeps them from being released until the
// returned function is called. The first call after a release opens the shards
// in parallel; concurrent callers wait for that instead of opening them twice.
func AcquireQuery(ctx context.Context) (release func(), err error) {
	if err := WaitReady(ctx); err != nil {
		return nil, err
	}

	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if !shardsLoaded.Load() {
		if err := loadShardsLocked(); err != nil {
			return nil, err
		}
	}

	queryInflight++
	lastActivity = time.Now()

	var once sync.Once
	return func() {
		once.Do(func() {
			lifecycleMu.Lock()
			defer lifecycleMu.Unlock()
			queryInflight--
			lastActivity = time.Now()
		})
	}, nil
}

// Warm opens the shards in the background and restarts the idle timer. It
// returns at once.
func Warm() {
	go func() {
		release, err := AcquireQuery(context.Background())
		if err != nil {
			logger.Debugf("Warm up failed: %v", err)
			return
		}
		release()
	}()
}

// loadShardsLocked opens every existing shard and builds the searcher over
// them. The caller holds lifecycleMu.
func loadShardsLocked() error {
	servicesMutex.RLock()
	idx := globalIndexer
	initialized := servicesInitialized
	servicesMutex.RUnlock()

	if !initialized || idx == nil {
		return ErrModernIndexerNotAvailable
	}

	start := time.Now()
	if err := idx.LoadExistingShards(); err != nil {
		return fmt.Errorf("failed to load index shards: %w", err)
	}

	servicesMutex.Lock()
	defer servicesMutex.Unlock()
	if !servicesInitialized || globalIndexer == nil {
		return ErrModernIndexerNotAvailable
	}

	updateSearcherShardsLocked()
	if globalSearcher == nil {
		return ErrModernSearcherNotAvailable
	}
	shardsLoaded.Store(true)

	logger.Infof("Opened %d index shard(s) in %s", len(globalIndexer.GetAllShards()), time.Since(start).Round(time.Millisecond))
	return nil
}

// releaseShardsLocked drops the searcher, the analytics service and the query
// caches, closes every shard, and hands the memory back to the system. The
// caller holds lifecycleMu and has checked that nothing uses the shards.
func releaseShardsLocked() {
	servicesMutex.Lock()
	if globalAnalytics != nil {
		if err := globalAnalytics.Stop(); err != nil {
			logger.Warnf("Failed to stop analytics service: %v", err)
		}
		globalAnalytics = nil
	}
	if globalSearcher != nil {
		if err := globalSearcher.Stop(); err != nil {
			logger.Warnf("Failed to stop searcher: %v", err)
		}
		globalSearcher = nil
	}
	shardsLoaded.Store(false)
	idx := globalIndexer
	servicesMutex.Unlock()

	if idx != nil {
		if err := idx.ReleaseShards(); err != nil {
			logger.Warnf("Failed to release index shards: %v", err)
		}
	}

	freeOSMemory()
}

// runIdleWatcher releases the shards after IdleTimeout without activity.
func runIdleWatcher(ctx context.Context) {
	ticker := time.NewTicker(idleCheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			releaseIfIdle()
		}
	}
}

// releaseIfIdle releases the shards when they are open, nobody uses them and
// the last activity is older than IdleTimeout. It reports whether it released.
func releaseIfIdle() bool {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()

	if !shardsLoaded.Load() || queryInflight > 0 || activeRounds > 0 {
		return false
	}
	if time.Since(lastActivity) < IdleTimeout {
		return false
	}

	logger.Infof("No search or indexing for %s, releasing the index shards and caches", IdleTimeout)
	releaseShardsLocked()
	return true
}

// BeginRound marks the start of an indexing round and returns the function that
// ends it. While a round runs the parser exists and the shards stay open. When
// the last round ends the parser is released, and if no query needs the shards
// they are released too, then the memory goes back to the system. showActivity
// controls whether the round appears as the indexing indicator; the periodic
// incremental round leaves it off, like before.
func BeginRound(showActivity bool) (end func()) {
	releaseParser := indexer.AcquireLogParser()

	lifecycleMu.Lock()
	activeRounds++
	lastActivity = time.Now()
	lifecycleMu.Unlock()

	if showActivity {
		beginActivity()
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			if showActivity {
				endActivity()
			}
			releaseParser()

			lifecycleMu.Lock()
			defer lifecycleMu.Unlock()

			activeRounds--
			lastActivity = time.Now()
			if activeRounds > 0 {
				return
			}
			if !shardsLoaded.Load() && queryInflight == 0 {
				// Nobody asked for the shards, so only the groups this round
				// wrote are open. Close them now.
				releaseShardsLocked()
				return
			}
			freeOSMemory()
		})
	}
}

// activityRefs counts the rounds that show the indexing indicator, so the
// indicator stays on until the last of them ends.
var (
	activityMu   sync.Mutex
	activityRefs int
)

func beginActivity() {
	activityMu.Lock()
	defer activityMu.Unlock()

	activityRefs++
	if activityRefs == 1 {
		processing.SetIndexing(true)
	}
}

func endActivity() {
	activityMu.Lock()
	defer activityMu.Unlock()

	activityRefs--
	if activityRefs == 0 {
		processing.SetIndexing(false)
	}
}

// EnsureLoaded opens all shards for work that has to see every group, such as
// deleting the index of one log group before rebuilding it.
func EnsureLoaded() error {
	release, err := AcquireQuery(context.Background())
	if err != nil {
		return err
	}
	release()
	return nil
}
