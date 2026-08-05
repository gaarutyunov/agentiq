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

	rd *deadline
	wd *deadline
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
