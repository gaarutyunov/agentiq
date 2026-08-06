package wasmpg

import (
	"sync"
	"time"
)

// deadline is the net.Conn deadline machinery, modelled on the unexported
// pipeDeadline in the standard library's net.Pipe.
//
// It is not optional decoration. pgx implements context cancellation by
// setting a deadline in the past on the underlying net.Conn and expecting the
// blocked Read to come back with a timeout error; a connection that ignores
// SetDeadline hangs on every cancelled query, and — because DBOS's notification
// listener sits in a context-bounded WaitForNotification — hangs there
// permanently.
type deadline struct {
	mu     sync.Mutex
	timer  *time.Timer
	cancel chan struct{} // closed once the deadline has passed
}

func newDeadline() *deadline {
	return &deadline{cancel: make(chan struct{})}
}

// set arms the deadline for t, or disarms it when t is the zero time. A
// deadline already in the past fires immediately, which is exactly how pgx
// interrupts a blocked read.
func (d *deadline) set(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.timer != nil && !d.timer.Stop() {
		<-d.cancel // the timer already fired; wait for its close to land
	}
	d.timer = nil

	closed := isClosed(d.cancel)

	if t.IsZero() {
		if closed {
			d.cancel = make(chan struct{})
		}
		return
	}

	if dur := time.Until(t); dur > 0 {
		if closed {
			d.cancel = make(chan struct{})
		}
		d.timer = time.AfterFunc(dur, func() { close(d.cancel) })
		return
	}

	if !closed {
		close(d.cancel)
	}
}

// wait returns the channel that closes when the deadline passes.
func (d *deadline) wait() <-chan struct{} {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cancel
}

// expired reports whether the deadline has already passed, so that an
// operation can fail before it blocks at all.
func (d *deadline) expired() bool {
	return isClosed(d.wait())
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}
