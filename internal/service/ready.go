package service

import (
	"context"
	"sync"
	"time"
)

// StartWait bounds how long a request waits for the services to start. The
// host may send requests as soon as the plugin listens, a moment before the
// services are up.
var StartWait = 30 * time.Second

var (
	readyMu  sync.Mutex
	readyCh  = make(chan struct{})
	readyErr error
)

// markReady wakes the requests waiting for the services. A non nil err is
// what they answer with, because the services did not start. A later call
// replaces the outcome, so a start that succeeds after a failed one counts.
func markReady(err error) {
	readyMu.Lock()
	defer readyMu.Unlock()
	readyErr = err
	select {
	case <-readyCh:
	default:
		close(readyCh)
	}
}

// resetReady makes requests wait again once the services stopped.
func resetReady() {
	readyMu.Lock()
	defer readyMu.Unlock()
	select {
	case <-readyCh:
		readyCh = make(chan struct{})
		readyErr = nil
	default:
	}
}

// MarkStartFailed releases the waiting requests when the plugin could not get
// as far as starting the services.
func MarkStartFailed(err error) {
	if err == nil {
		err = ErrModernIndexerNotAvailable
	}
	markReady(err)
}

// WaitReady blocks until the services started, their start failed, ctx ends
// or StartWait passed.
func WaitReady(ctx context.Context) error {
	return WaitReadyWithin(ctx, StartWait)
}

// WaitReadyWithin is WaitReady with its own time limit.
func WaitReadyWithin(ctx context.Context, limit time.Duration) error {
	readyMu.Lock()
	ch := readyCh
	readyMu.Unlock()

	timer := time.NewTimer(limit)
	defer timer.Stop()

	select {
	case <-ch:
		readyMu.Lock()
		defer readyMu.Unlock()
		return readyErr
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ErrModernIndexerNotAvailable
	}
}
