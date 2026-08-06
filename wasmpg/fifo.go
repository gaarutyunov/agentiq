package wasmpg

import (
	"context"
	"sync"
)

// fifoLock is the one-in-flight lock guarding the single PGlite backend
// (SPEC.md §12.4).
//
// It is a hand-rolled queue rather than a sync.Mutex because the ordering
// matters and has to be testable: waiters are granted the lock in the order
// they asked for it, so a busy connection cannot starve a quiet one, and the
// order requests reach the backend is the order they were written. A
// sync.Mutex makes no such promise until it enters starvation mode.
//
// The lock is held only across one execProtocol round trip. Nothing acquires
// it to read, which is what keeps a connection parked in WaitForNotification
// from blocking every other connection — see the package doc.
type fifoLock struct {
	mu      sync.Mutex
	held    bool
	waiters []chan struct{}
}

// acquire blocks until the lock is granted, ctx is done, or done is closed.
func (f *fifoLock) acquire(ctx context.Context, done <-chan struct{}) error {
	f.mu.Lock()
	if !f.held {
		f.held = true
		f.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	f.waiters = append(f.waiters, ch)
	f.mu.Unlock()

	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return f.abandon(ch, ctx.Err())
	case <-done:
		return f.abandon(ch, ErrClosed)
	}
}

// abandon withdraws a waiter that gave up. If it had already been granted the
// lock in the window between the select firing and this call, it owns the lock
// and has to hand it on rather than drop it.
func (f *fifoLock) abandon(ch chan struct{}, cause error) error {
	f.mu.Lock()
	for i, w := range f.waiters {
		if w == ch {
			f.waiters = append(f.waiters[:i], f.waiters[i+1:]...)
			f.mu.Unlock()
			return cause
		}
	}
	f.mu.Unlock()

	<-ch // already granted: drain the grant, then pass it along
	f.release()
	return cause
}

// release hands the lock to the longest-waiting caller, or marks it free.
func (f *fifoLock) release() {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(f.waiters) > 0 {
		next := f.waiters[0]
		f.waiters = f.waiters[1:]
		close(next)
		return
	}
	f.held = false
}
