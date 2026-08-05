package wasmpg

import (
	"context"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// addr is the net.Addr both ends of a logical connection report. There is no
// network and no peer, so it names the transport instead of lying about a
// host and port.
type addr struct{}

func (addr) Network() string { return "pglite" }
func (addr) String() string  { return "pglite" }

// Conn is one logical PostgreSQL connection over the shared PGlite backend.
// It satisfies net.Conn, which is all pgx asks of it.
//
// pgx drives a connection from a single goroutine for writes and a single
// goroutine for reads, so Write serialises on its own mutex and Read assumes
// it is the only reader; the read queue itself is safe against concurrent
// delivery from the notification path.
type Conn struct {
	m      *Multiplexer
	pid    int32
	closed sync.Once
	done   chan struct{}
	closeE error

	// startup is true until pgx's StartupMessage has been answered. It
	// selects the untyped message framing — see nextFrontendMessage.
	wmu     sync.Mutex
	wbuf    []byte
	startup bool

	rmu   sync.Mutex
	queue [][]byte
	rem   []byte
	ready chan struct{} // buffered(1) wake-up for a blocked Read

	// hold is this connection's ownership of the one-in-flight lock. The lock
	// is held for the length of a transaction, so ownership outlives a single
	// round trip and has to survive a Close that arrives from another
	// goroutine.
	hold backendHold

	rd *deadline
	wd *deadline
}

// backendHold tracks whether a logical connection is sitting on the shared
// PGlite session, and whether a round trip is running inside it right now.
//
// The two are separate because they end at different times: a transaction keeps
// held true across the idle gaps between statements, while inFlight is true only
// while execProtocol has not returned. A Close arriving during those idle gaps
// must hand the session back; one arriving mid-round-trip must not, or it pulls
// the backend out from under a call that is still going.
type backendHold struct {
	mu       sync.Mutex
	held     bool
	inFlight bool
	closing  bool
}

func newConn(m *Multiplexer, pid int32) *Conn {
	return &Conn{
		m:       m,
		pid:     pid,
		done:    make(chan struct{}),
		startup: true,
		ready:   make(chan struct{}, 1),
		rd:      newDeadline(),
		wd:      newDeadline(),
	}
}

// PID is the synthetic backend PID this connection was told about in
// BackendKeyData.
func (c *Conn) PID() int32 { return c.pid }

func (c *Conn) LocalAddr() net.Addr  { return addr{} }
func (c *Conn) RemoteAddr() net.Addr { return addr{} }

func (c *Conn) SetDeadline(t time.Time) error {
	c.rd.set(t)
	c.wd.set(t)
	return nil
}

func (c *Conn) SetReadDeadline(t time.Time) error  { c.rd.set(t); return nil }
func (c *Conn) SetWriteDeadline(t time.Time) error { c.wd.set(t); return nil }

// deliver appends complete backend frames to the read queue and wakes a
// blocked reader.
//
// The queue holds whole frames, never fragments, which is what makes injecting
// a NotificationResponse safe: an injected frame can only land on a message
// boundary, where the protocol allows one to appear. Read may still hand the
// caller a partial frame — pgx reassembles — but the queue's own ordering is
// frame-aligned.
func (c *Conn) deliver(b []byte) {
	if len(b) == 0 {
		return
	}
	c.rmu.Lock()
	if isClosed(c.done) {
		c.rmu.Unlock()
		return
	}
	c.queue = append(c.queue, b)
	c.rmu.Unlock()

	select {
	case c.ready <- struct{}{}:
	default:
	}
}

// Read returns buffered backend bytes, blocking until some arrive.
//
// It never touches the one-in-flight lock. That is the property that keeps
// DBOS's listener — parked here for the lifetime of the process inside
// WaitForNotification — from stalling every other logical connection.
func (c *Conn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		c.rmu.Lock()
		if len(c.rem) == 0 && len(c.queue) > 0 {
			c.rem = c.queue[0]
			c.queue = c.queue[1:]
		}
		if len(c.rem) > 0 {
			n := copy(p, c.rem)
			c.rem = c.rem[n:]
			c.rmu.Unlock()
			return n, nil
		}
		buffered := len(c.queue) > 0
		c.rmu.Unlock()
		if buffered {
			continue
		}

		if c.rd.expired() {
			return 0, c.timeout("read")
		}

		select {
		case <-c.ready:
		case <-c.done:
			// Drain whatever landed before the close, then report it. A
			// Terminate is an orderly end of stream; anything else is a
			// closed connection.
			c.rmu.Lock()
			pending := len(c.rem) > 0 || len(c.queue) > 0
			c.rmu.Unlock()
			if pending {
				continue
			}
			return 0, c.closeErr()
		case <-c.rd.wait():
			return 0, c.timeout("read")
		}
	}
}

// Write consumes frontend wire bytes from pgx.
//
// Messages are decoded out of the stream one at a time so that the handshake
// can be answered locally (SPEC.md §12.3) and LISTEN/UNLISTEN can be observed
// (SPEC.md §12.4). Everything else is forwarded to the backend, and contiguous
// runs of forwardable messages go across in a single execProtocol call — an
// extended-query batch that arrives as Parse/Bind/Describe/Execute/Sync must
// reach the backend as one unit, or the backend produces no ReadyForQuery and
// pgx blocks waiting for it.
func (c *Conn) Write(p []byte) (int, error) {
	if isClosed(c.done) {
		return 0, c.closeErr()
	}
	if c.wd.expired() {
		return 0, c.timeout("write")
	}

	c.wmu.Lock()
	defer c.wmu.Unlock()

	ctx, cancel := c.opContext(c.wd)
	defer cancel()

	c.wbuf = append(c.wbuf, p...)

	var forward []byte
	flush := func() error {
		if len(forward) == 0 {
			return nil
		}
		msg := forward
		forward = nil
		return c.m.submit(ctx, c, msg)
	}

	for {
		msg, n, err := nextFrontendMessage(c.wbuf, c.startup)
		if err != nil {
			// Framing is lost. Report it the way the backend would, so pgx
			// surfaces a PgError rather than a torn socket, then stop.
			c.deliver(encodeErrorResponse("FATAL", "08P01", err.Error()))
			_ = c.Close()
			return 0, err
		}
		if n == 0 {
			break
		}
		c.consume(n)

		local, err := c.handleLocally(msg, flush)
		if err != nil {
			return 0, err
		}
		if !local {
			forward = append(forward, msg.raw...)
		}
	}

	if err := flush(); err != nil {
		return 0, err
	}
	return len(p), nil
}

// handleLocally answers the messages that never reach PGlite, and reports
// whether it did.
//
// flush pushes whatever forwardable messages have accumulated ahead of this
// one. Terminate has to call it: the batch before it is still owed a reply,
// and the connection is about to stop reading. The startup-phase answers do
// not, because nothing can be queued ahead of them.
func (c *Conn) handleLocally(msg frontendMessage, flush func() error) (bool, error) {
	if c.startup {
		switch msg.code {
		case sslRequestCode, gssEncRequestCode:
			// A bare 'N', with no length prefix: SSL is not negotiated, and
			// pgx then re-sends its StartupMessage in the same untyped
			// framing, so the startup phase continues.
			c.deliver([]byte{sslRefused})
			return true, nil
		case cancelRequestCode:
			// There is one backend and no second session to cancel from. A
			// real server closes the connection without replying, and so does
			// this.
			_ = c.Close()
			return true, nil
		default:
			// Any remaining untyped message is the StartupMessage. Its
			// protocol version is not enforced: PGlite speaks 3.0 and pgx
			// sends 3.0, and rejecting an unexpected value here would only
			// convert a working connection into an obscure failure.
			c.startup = false
			c.deliver(c.m.handshake(c.pid))
			return true, nil
		}
	}

	switch msg.typ {
	case feTerminate:
		// The logical connection ends; the backend stays alive for every
		// other connection sharing it (SPEC.md §12.3). Terminate is
		// deliberately not forwarded — PGlite would tear the session down.
		if err := flush(); err != nil {
			return true, err
		}
		c.terminate()
		return true, nil

	case feQuery, feParse:
		for _, op := range observeListen(msg.sql()) {
			c.m.apply(c, op)
		}
		return false, nil

	default:
		return false, nil
	}
}

// roundTripOutcome says what a finished round trip left the shared session in.
type roundTripOutcome int

const (
	// sessionIdle: the backend reported ReadyForQuery('I'). Nothing is open,
	// and the next logical connection can have the backend.
	sessionIdle roundTripOutcome = iota

	// sessionInTx: the backend reported 'T' or 'E', or reported nothing at all.
	// A transaction block is open — or a batch is half-written — so the
	// connection keeps the backend.
	sessionInTx

	// sessionUnknown: the round trip itself failed, so the backend never said
	// where it stands. The connection gives the backend up, but only behind a
	// ROLLBACK.
	sessionUnknown
)

// beginRoundTrip takes the one-in-flight lock for a round trip, unless this
// connection is already holding it across an open transaction.
//
// Re-entering an existing hold without touching the lock is the whole point:
// the second statement of a transaction must not queue behind the waiters that
// piled up while the first one ran, or a transaction could never finish.
func (c *Conn) beginRoundTrip(ctx context.Context) error {
	c.hold.mu.Lock()
	switch {
	case c.hold.closing:
		c.hold.mu.Unlock()
		return c.closeErr()
	case c.hold.held:
		c.hold.inFlight = true
		c.hold.mu.Unlock()
		return nil
	}
	c.hold.mu.Unlock()

	if err := c.m.backend.acquire(ctx, c.done); err != nil {
		return err
	}

	c.hold.mu.Lock()
	if c.hold.closing {
		// The connection shut down while this call was queued for the lock.
		// shutdown has already looked and found nothing held, so it will not
		// look again: hand the lock straight on rather than record ownership
		// nobody will ever release.
		c.hold.mu.Unlock()
		c.m.backend.release()
		return c.closeErr()
	}
	c.hold.held = true
	c.hold.inFlight = true
	c.hold.mu.Unlock()
	return nil
}

// endRoundTrip ends a round trip and decides whether this connection keeps the
// backend.
//
// It keeps it for [sessionInTx] — that is the transaction isolation the shared
// session cannot provide on its own — and gives it back otherwise. A close that
// arrived mid-round-trip is honoured here rather than by shutdown, which saw
// inFlight and left the session alone.
func (c *Conn) endRoundTrip(outcome roundTripOutcome) {
	c.hold.mu.Lock()
	c.hold.inFlight = false
	if outcome == sessionInTx && !c.hold.closing {
		c.hold.mu.Unlock()
		return
	}
	held := c.hold.held
	c.hold.held = false
	c.hold.mu.Unlock()

	if !held {
		return
	}
	if outcome != sessionIdle {
		c.rollbackSession()
	}
	c.m.backend.release()
}

// abandonHold hands the shared session back when the connection goes away.
//
// Without it, a connection that closes between BEGIN and COMMIT — a deadline, a
// Terminate, a torn frame — takes the only backend there is with it, and every
// other logical connection blocks on the lock forever.
func (c *Conn) abandonHold() {
	c.hold.mu.Lock()
	c.hold.closing = true
	if c.hold.inFlight {
		// A round trip is running. It will see closing when it ends and give
		// the session back itself; releasing here would let another connection
		// onto a backend that is still answering this one.
		c.hold.mu.Unlock()
		return
	}
	held := c.hold.held
	c.hold.held = false
	c.hold.mu.Unlock()

	if !held {
		return
	}
	c.rollbackSession()
	c.m.backend.release()
}

// rollbackSession returns the shared PGlite session to an idle state.
//
// A real backend aborts an open transaction when the session that owns it ends.
// Here the session outlives the logical connection — it outlives all of them —
// so the rollback has to be issued explicitly, or the next connection to be
// granted the backend silently finds itself inside somebody else's transaction.
// When the session was idle anyway this is a no-op the backend answers with a
// warning, which is cheaper than tracking a status that could be wrong.
//
// It runs on a background context on purpose: the context that brought us here
// is usually the cancelled or expired one that caused the abandonment in the
// first place, and skipping the cleanup because of it would wedge the session
// for everybody else. The reply is discarded — it belongs to a round trip whose
// caller has already been told the write failed.
func (c *Conn) rollbackSession() {
	_, _ = c.m.exec(context.Background(), rollbackQuery())
}

// consume drops n bytes from the front of the write buffer, resetting it once
// it is empty so that a long-lived connection does not retain a slice that
// only ever grows.
func (c *Conn) consume(n int) {
	c.wbuf = c.wbuf[n:]
	if len(c.wbuf) == 0 {
		c.wbuf = c.wbuf[:0:0]
	}
}

// Close ends the logical connection. It is safe to call more than once, and
// leaves the PGlite backend running.
func (c *Conn) Close() error {
	c.shutdown(net.ErrClosed)
	return nil
}

// terminate ends the connection in response to a Terminate message, which is
// an orderly end of stream rather than a fault.
func (c *Conn) terminate() { c.shutdown(io.EOF) }

func (c *Conn) shutdown(cause error) {
	c.closed.Do(func() {
		c.closeE = cause
		c.m.forget(c)
		close(c.done)
		// Last, and after close(c.done): a caller queued for the backend has
		// to be able to give up before the session's ownership is settled, or
		// abandonHold decides against a waiter that is still on its way in.
		c.abandonHold()
	})
}

func (c *Conn) closeErr() error {
	if c.closeE != nil {
		return c.closeE
	}
	return net.ErrClosed
}

// timeout builds the error a deadline produces. It has to satisfy net.Error
// with Timeout reporting true, because that is the only thing pgx inspects
// when deciding whether a read was cancelled or the connection broke.
func (c *Conn) timeout(op string) error {
	return &net.OpError{Op: op, Net: "pglite", Addr: addr{}, Err: os.ErrDeadlineExceeded}
}

// opContext derives a context that ends when the connection closes or the
// deadline passes, so that a write blocked behind the one-in-flight lock — or
// inside a JavaScript promise that never settles — is still interruptible.
func (c *Conn) opContext(d *deadline) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	stop := make(chan struct{})
	go func() {
		select {
		case <-stop:
		case <-c.done:
			cancel()
		case <-d.wait():
			cancel()
		}
	}()
	return ctx, func() {
		close(stop)
		cancel()
	}
}
