package wasmpg

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRejectsABudgetThatDeadlocks(t *testing.T) {
	// A budget of 1 hands the only logical connection to DBOS's notification
	// listener, which never gives it back. Refusing at construction is the
	// difference between a clear error and a runtime that starts and then
	// silently stops making progress.
	_, err := New(Config{Exec: nullExec, LogicalConns: 1})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listener")

	_, err = New(Config{LogicalConns: 4})
	assert.ErrorContains(t, err, "Config.Exec is required")
}

func TestMaxConnsIsTheBudget(t *testing.T) {
	m, err := New(Config{Exec: nullExec})
	require.NoError(t, err)
	assert.Equal(t, int32(DefaultLogicalConns), m.MaxConns())

	m, err = New(Config{Exec: nullExec, LogicalConns: 6})
	require.NoError(t, err)
	assert.Equal(t, int32(6), m.MaxConns())
}

func TestDialFailsLoudlyPastTheBudget(t *testing.T) {
	// The pool must not be left waiting for a connection that is never coming
	// back: one of the ones in use belongs to a listener that holds it for the
	// lifetime of the process.
	m, err := New(Config{Exec: nullExec, LogicalConns: 2})
	require.NoError(t, err)

	first := dial(t, m)
	dial(t, m)

	_, err = m.DialContext(context.Background(), "tcp", "ignored")
	assert.ErrorIs(t, err, ErrBudgetExhausted)

	require.NoError(t, first.Close())
	_, err = m.DialContext(context.Background(), "tcp", "ignored")
	assert.NoError(t, err, "closing a connection returns its budget")
}

func TestListenRegistersTheConnection(t *testing.T) {
	rec := newRecorder()
	m, err := New(Config{Exec: rec.exec})
	require.NoError(t, err)
	c := dial(t, m)

	_, err = c.Write(query("LISTEN dbos_notifications_channel"))
	require.NoError(t, err)

	assert.Equal(t, 1, m.Listeners("dbos_notifications_channel"))
	// Observing does not consume: the backend still has to run the statement.
	assert.Len(t, rec.forwarded(), 1)
}

func TestListenIsObservedThroughTheExtendedProtocolToo(t *testing.T) {
	// pgx picks between the simple and the extended protocol on its own, and
	// which one DBOS's listener ends up on is not something this package gets
	// to decide. Watching only 'Q' would leave the routing table empty on
	// whichever build chose 'P'.
	m, err := New(Config{Exec: nullExec})
	require.NoError(t, err)
	c := dial(t, m)

	payload := append([]byte("\x00"), "LISTEN dbos_notifications_channel\x00"...)
	payload = append(payload, 0, 0)
	_, err = c.Write(encodeBackend('P', payload))
	require.NoError(t, err)

	assert.Equal(t, 1, m.Listeners("dbos_notifications_channel"))
}

func TestUnlistenDeregisters(t *testing.T) {
	m, err := New(Config{Exec: nullExec})
	require.NoError(t, err)
	c := dial(t, m)

	_, err = c.Write(query("LISTEN alpha; LISTEN beta"))
	require.NoError(t, err)
	require.Equal(t, 1, m.Listeners("alpha"))
	require.Equal(t, 1, m.Listeners("beta"))

	_, err = c.Write(query("UNLISTEN alpha"))
	require.NoError(t, err)
	assert.Zero(t, m.Listeners("alpha"))
	assert.Equal(t, 1, m.Listeners("beta"))

	_, err = c.Write(query("UNLISTEN *"))
	require.NoError(t, err)
	assert.Zero(t, m.Listeners("beta"))
}

func TestClosingAConnectionDropsItsRegistrations(t *testing.T) {
	m, err := New(Config{Exec: nullExec})
	require.NoError(t, err)
	c := dial(t, m)

	_, err = c.Write(query("LISTEN gone"))
	require.NoError(t, err)
	require.Equal(t, 1, m.Listeners("gone"))

	require.NoError(t, c.Close())
	assert.Zero(t, m.Listeners("gone"), "a notification must never chase a closed connection")
}

func TestNotifyReachesEveryRegisteredConnectionAndNoOthers(t *testing.T) {
	// This is the asymmetry the routing table exists for: LISTEN is
	// session-global against one backend, but pgx delivers a
	// NotificationResponse only on the connection it arrives on
	// (SPEC.md §12.4).
	m, err := New(Config{Exec: nullExec})
	require.NoError(t, err)

	a, b, bystander := dial(t, m), dial(t, m), dial(t, m)

	for _, c := range []*Conn{a, b} {
		_, err := c.Write(query("LISTEN jobs"))
		require.NoError(t, err)
		drain(t, c, 6) // the backend's ReadyForQuery for the LISTEN itself
	}
	require.Equal(t, 2, m.Listeners("jobs"))

	m.Notify("jobs", `{"workflow":"wf-1"}`)

	want := encodeNotificationResponse(DefaultBackendPID, "jobs", `{"workflow":"wf-1"}`)
	assert.Equal(t, want, readN(t, a, len(want)))
	assert.Equal(t, want, readN(t, b, len(want)))

	// The bystander's stream is untouched.
	require.NoError(t, bystander.SetReadDeadline(time.Now().Add(20*time.Millisecond)))
	_, err = bystander.Read(make([]byte, 1))
	assert.Error(t, err)
}

func TestNotifyOnAnUnregisteredChannelIsDropped(t *testing.T) {
	m, err := New(Config{Exec: nullExec})
	require.NoError(t, err)
	c := dial(t, m)

	m.Notify("nobody_is_listening", "x")

	require.NoError(t, c.SetReadDeadline(time.Now().Add(20*time.Millisecond)))
	_, err = c.Read(make([]byte, 1))
	assert.Error(t, err)
}

func TestInlineNotificationsAreStrippedFromTheStream(t *testing.T) {
	// A notification that comes back inside an execProtocol result reached
	// exactly one connection — whichever ran the statement — which is the
	// mis-routing the table corrects. It must not be delivered as-is.
	rec := newRecorder()
	rec.reply = func([]byte) []byte {
		out := encodeNotificationResponse(9, "jobs", "inline")
		return append(out, encodeReadyForQuery('I')...)
	}
	m, err := New(Config{Exec: rec.exec})
	require.NoError(t, err)

	c := dial(t, m)
	_, err = c.Write(query("NOTIFY jobs, 'inline'"))
	require.NoError(t, err)

	assert.Equal(t, encodeReadyForQuery('I'), readN(t, c, 6))
}

func TestInlineNotificationsCanBeRoutedInstead(t *testing.T) {
	rec := newRecorder()
	rec.reply = func(msg []byte) []byte {
		// Only the NOTIFY produces one; the LISTEN just succeeds.
		if string(msg) == string(query("NOTIFY jobs, 'inline'")) {
			out := encodeNotificationResponse(9, "jobs", "inline")
			return append(out, encodeReadyForQuery('I')...)
		}
		return encodeReadyForQuery('I')
	}
	m, err := New(Config{Exec: rec.exec, RouteInlineNotifications: true})
	require.NoError(t, err)

	listener := dial(t, m)
	_, err = listener.Write(query("LISTEN jobs"))
	require.NoError(t, err)
	drain(t, listener, 6)

	notifier := dial(t, m)
	_, err = notifier.Write(query("NOTIFY jobs, 'inline'"))
	require.NoError(t, err)

	want := encodeNotificationResponse(DefaultBackendPID, "jobs", "inline")
	assert.Equal(t, want, readN(t, listener, len(want)))
}

func TestBackendIsOneInFlight(t *testing.T) {
	rec := newRecorder()
	rec.gate = make(chan struct{})
	m, err := New(Config{Exec: rec.exec})
	require.NoError(t, err)

	first, second := dial(t, m), dial(t, m)

	go func() {
		_, _ = first.Write(query("SELECT pg_sleep(1)"))
	}()
	select {
	case <-rec.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the first request never reached the backend")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = second.Write(query("SELECT 2"))
	}()

	select {
	case <-done:
		t.Fatal("a second request reached the backend while the first was in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(rec.gate)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the queued request never ran")
	}
	assert.Len(t, rec.forwarded(), 2)
}

func TestBackendGrantsInFIFOOrder(t *testing.T) {
	// Ordering has to be fair, or a busy connection starves a quiet one — and
	// the quiet one is often the queue poller.
	rec := newRecorder()
	rec.gate = make(chan struct{})
	m, err := New(Config{Exec: rec.exec, LogicalConns: 8})
	require.NoError(t, err)

	holder := dial(t, m)
	go func() { _, _ = holder.Write(query("SELECT 0")) }()
	<-rec.entered

	const waiters = 5
	var wg sync.WaitGroup
	for i := 1; i <= waiters; i++ {
		c := dial(t, m)
		sql := fmt.Sprintf("SELECT %d", i)

		wg.Add(1)
		started := make(chan struct{})
		go func() {
			defer wg.Done()
			close(started)
			_, _ = c.Write(query(sql))
		}()
		<-started
		// Give the goroutine time to reach the lock, so the order they queue
		// in is the order they were launched in.
		waitForWaiters(t, &m.backend, i)
	}

	close(rec.gate)
	wg.Wait()

	forwarded := rec.forwarded()
	require.Len(t, forwarded, waiters+1)
	for i := 1; i <= waiters; i++ {
		assert.Equal(t, query(fmt.Sprintf("SELECT %d", i)), forwarded[i],
			"request %d was granted out of order", i)
	}
}

// waitForWaiters blocks until the one-in-flight lock has n queued waiters.
func waitForWaiters(t *testing.T, f *fifoLock, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		got := len(f.waiters)
		f.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("only saw fewer than %d waiters queue on the backend", n)
}

func TestAReadingConnectionNeverBlocksTheBackend(t *testing.T) {
	// This is the deadlock the whole design is arranged against: DBOS parks a
	// connection in WaitForNotification — a blocking Read — for the lifetime
	// of the process. If Read touched the one-in-flight lock, nothing else
	// would ever run a query again.
	m, err := New(Config{Exec: nullExec})
	require.NoError(t, err)

	listener := dial(t, m)
	worker := dial(t, m)

	_, err = listener.Write(query("LISTEN dbos_notifications_channel"))
	require.NoError(t, err)
	drain(t, listener, 6)

	parked := make(chan error, 1)
	go func() {
		buf := make([]byte, 64)
		_, err := listener.Read(buf)
		parked <- err
	}()
	// The listener is now blocked in Read and stays there.

	for i := 0; i < 20; i++ {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = worker.Write(query("SELECT 1"))
			drain(t, worker, 6)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("the worker stalled behind the parked listener on iteration %d", i)
		}
	}

	// And the parked read is still live: waking it delivers the notification.
	m.Notify("dbos_notifications_channel", "wake")
	select {
	case err := <-parked:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("the parked listener never woke")
	}
}

func TestCloseShutsEveryConnection(t *testing.T) {
	m, err := New(Config{Exec: nullExec})
	require.NoError(t, err)

	a, b := dial(t, m), dial(t, m)
	require.NoError(t, m.Close())

	for _, c := range []*Conn{a, b} {
		_, err := c.Read(make([]byte, 1))
		assert.Error(t, err)
	}
	_, err = m.DialContext(context.Background(), "tcp", "ignored")
	assert.ErrorIs(t, err, ErrClosed)
}
