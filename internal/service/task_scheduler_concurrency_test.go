package service

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nginxui/plugin-log-analytics/internal/config"
)

func withMaxConcurrentIndexTasks(t *testing.T, value int) {
	t.Helper()

	previous := config.Get()
	config.Set(config.Settings{MaxConcurrentIndexTasks: value})
	t.Cleanup(func() { config.Set(previous) })
}

func TestMaxConcurrentIndexTasksHonoursSetting(t *testing.T) {
	withMaxConcurrentIndexTasks(t, 7)

	assert.Equal(t, 7, maxConcurrentIndexTasks())
}

func TestMaxConcurrentIndexTasksAutoDerivedIsBounded(t *testing.T) {
	withMaxConcurrentIndexTasks(t, 0)

	slots := maxConcurrentIndexTasks()

	assert.GreaterOrEqual(t, slots, 1, "at least one group must be indexable")
	assert.LessOrEqual(t, slots, defaultMaxConcurrentIndexTasks,
		"auto-derived concurrency must stay capped regardless of the host CPU count")
}

// TestRunSlotsBoundConcurrency is the regression guard for issue #1792: a
// post-upgrade rebuild schedules one task per log group, and before the fix all
// of them ran at once, each fanning out over its rotated files. Peak memory then
// scaled with the number of configured sites and the container ran out of RAM.
func TestRunSlotsBoundConcurrency(t *testing.T) {
	withMaxConcurrentIndexTasks(t, 2)

	scheduler := &TaskScheduler{
		taskLocks: make(map[string]*sync.Mutex),
		runSlots:  make(chan struct{}, maxConcurrentIndexTasks()),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheduler.ctx = ctx
	scheduler.cancel = cancel

	const groups = 32

	var inFlight int32
	var peak int32
	var waitGroup sync.WaitGroup

	release := make(chan struct{})

	for i := 0; i < groups; i++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()

			if !scheduler.acquireRunSlot(ctx) {
				return
			}
			defer scheduler.releaseRunSlot()

			current := atomic.AddInt32(&inFlight, 1)
			for {
				observed := atomic.LoadInt32(&peak)
				if current <= observed || atomic.CompareAndSwapInt32(&peak, observed, current) {
					break
				}
			}

			<-release
			atomic.AddInt32(&inFlight, -1)
		}()
	}

	// Give every goroutine a chance to pile up on the semaphore.
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&inFlight) == 2
	}, 2*time.Second, 5*time.Millisecond, "the first two tasks should acquire slots immediately")

	assert.LessOrEqual(t, atomic.LoadInt32(&peak), int32(2),
		"no more than the configured number of log groups may index concurrently")

	close(release)
	waitGroup.Wait()

	assert.Equal(t, int32(2), atomic.LoadInt32(&peak),
		"all %d groups must have run through exactly 2 slots", groups)
	assert.Equal(t, int32(0), atomic.LoadInt32(&inFlight))
	assert.Len(t, scheduler.runSlots, 0, "every acquired slot must be released")
}

// TestSlotLimitFollowsTheSetting shows that a raised limit lets the waiting
// tasks through without a restart.
func TestSlotLimitFollowsTheSetting(t *testing.T) {
	withMaxConcurrentIndexTasks(t, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheduler := &TaskScheduler{
		ctx:       ctx,
		cancel:    cancel,
		taskLocks: make(map[string]*sync.Mutex),
		runSlots:  make(chan struct{}, maxRunSlots),
	}

	require.True(t, scheduler.acquireRunSlot(ctx))

	acquired := make(chan bool, 1)
	go func() { acquired <- scheduler.acquireRunSlot(ctx) }()

	select {
	case <-acquired:
		t.Fatal("the second task must wait while the limit is 1")
	case <-time.After(100 * time.Millisecond):
	}

	config.Set(config.Settings{MaxConcurrentIndexTasks: 2})
	scheduler.SlotLimitChanged()

	select {
	case ok := <-acquired:
		assert.True(t, ok)
	case <-time.After(2 * time.Second):
		t.Fatal("a raised limit must let the waiting task run")
	}
}

// TestAcquireRunSlotUnblocksOnCancellation makes sure queued tasks cannot pin
// goroutines for the life of the process when the scheduler shuts down.
func TestAcquireRunSlotUnblocksOnCancellation(t *testing.T) {
	withMaxConcurrentIndexTasks(t, 1)

	scheduler := &TaskScheduler{
		taskLocks: make(map[string]*sync.Mutex),
		runSlots:  make(chan struct{}, 1),
	}
	ctx, cancel := context.WithCancel(context.Background())
	scheduler.ctx = ctx
	scheduler.cancel = cancel

	require.True(t, scheduler.acquireRunSlot(ctx), "the first task takes the only slot")

	// Queue many waiters so the goroutine signal dominates unrelated background
	// activity in this package's other tests.
	const waiters = 50

	baseline := runtime.NumGoroutine()

	blocked := make(chan bool, waiters)
	for i := 0; i < waiters; i++ {
		go func() {
			blocked <- scheduler.acquireRunSlot(ctx)
		}()
	}

	require.Eventually(t, func() bool {
		return runtime.NumGoroutine() >= baseline+waiters
	}, 2*time.Second, 10*time.Millisecond, "every waiter should be parked on the semaphore")

	select {
	case <-blocked:
		t.Fatal("no waiter may proceed while the only slot is taken")
	case <-time.After(100 * time.Millisecond):
	}

	cancel()

	for i := 0; i < waiters; i++ {
		select {
		case acquired := <-blocked:
			assert.False(t, acquired, "a cancelled waiter must not report a slot")
		case <-time.After(2 * time.Second):
			t.Fatal("cancelling the scheduler must release blocked waiters")
		}
	}

	require.Eventually(t, func() bool {
		return runtime.NumGoroutine() < baseline+waiters/2
	}, 2*time.Second, 10*time.Millisecond, "blocked waiters must not leak goroutines")
}

// TestRunSlotsSurviveRepeatedScheduling checks that repeatedly acquiring and
// releasing slots neither leaks capacity nor accumulates goroutines, which is
// what a long-lived instance does across many incremental indexing cycles.
func TestRunSlotsSurviveRepeatedScheduling(t *testing.T) {
	withMaxConcurrentIndexTasks(t, 2)

	scheduler := &TaskScheduler{
		taskLocks: make(map[string]*sync.Mutex),
		runSlots:  make(chan struct{}, maxConcurrentIndexTasks()),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scheduler.ctx = ctx
	scheduler.cancel = cancel

	// Settle the runtime before sampling, so background goroutines started by
	// earlier tests do not count against the baseline.
	runtime.GC()
	before := runtime.NumGoroutine()

	for cycle := 0; cycle < 200; cycle++ {
		require.True(t, scheduler.acquireRunSlot(ctx))
		scheduler.releaseRunSlot()
	}

	assert.Len(t, scheduler.runSlots, 0, "slots must be fully returned after each cycle")

	require.Eventually(t, func() bool {
		return runtime.NumGoroutine() <= before+2
	}, 2*time.Second, 10*time.Millisecond, "repeated scheduling must not grow the goroutine count")
}
