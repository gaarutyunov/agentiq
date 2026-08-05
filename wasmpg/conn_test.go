package wasmpg

import (
	"context"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSSLRequestIsRefusedLocally(t *testing.T) {
	rec := newRecorder()
	m, err := New(Config{Exec: rec.exec})
	require.NoError(t, err)

	nc, err := m.DialContext(context.Background(), "tcp", "ignored")
	require.NoError(t, err)
	c := nc.(*Conn)

	n, err := c.Write(startupMessage(sslRequestCode, nil))
	require.NoError(t, err)
	assert.Equal(t, 8, n)

	// A bare 'N', with no length prefix (SPEC.md §12.3).
	assert.Equal(t, []byte{'N'}, readN(t, c, 1))
	assert.Empty(t, rec.forwarded(), "the handshake must never reach PGlite")
}

func TestStartupMessageIsAnsweredLocally(t *testing.T) {
	rec := newRecorder()
	m, err := New(Config{Exec: rec.exec})
	require.NoError(t, err)

	nc, err := m.DialContext(context.Background(), "tcp", "ignored")
	require.NoError(t, err)
	c := nc.(*Conn)

	_, err = c.Write(startupMessage(sslRequestCode, nil))
	require.NoError(t, err)
	readN(t, c, 1)

	_, err = c.Write(startupMessage(protocolVersion3, []byte("user\x00postgres\x00\x00")))
	require.NoError(t, err)

	want := m.handshake(c.PID())
	assert.Equal(t, want, readN(t, c, len(want)))
	assert.Empty(t, rec.forwarded(), "the handshake must never reach PGlite")
}

func TestHandshakeSurvivesAByteAtATime(t *testing.T) {
	// pgx writes whole messages, but nothing in net.Conn promises that, and a
	// shim that only works on whole-message writes is a shim that breaks the
	// first time something buffers differently.
	rec := newRecorder()
	m, err := New(Config{Exec: rec.exec})
	require.NoError(t, err)

	nc, err := m.DialContext(context.Background(), "tcp", "ignored")
	require.NoError(t, err)
	c := nc.(*Conn)

	stream := append(startupMessage(sslRequestCode, nil), startupMessage(protocolVersion3, nil)...)
	for _, b := range stream {
		_, err := c.Write([]byte{b})
		require.NoError(t, err)
	}

	want := append([]byte{'N'}, m.handshake(c.PID())...)
	assert.Equal(t, want, readN(t, c, len(want)))
}

func TestTerminateClosesTheConnectionAndLeavesTheBackendAlive(t *testing.T) {
	rec := newRecorder()
	m, err := New(Config{Exec: rec.exec})
	require.NoError(t, err)

	c := dial(t, m)
	other := dial(t, m)

	_, err = c.Write(terminateMsg())
	require.NoError(t, err)

	// Terminate is answered by ending the stream, not by a message, and it is
	// never forwarded: PGlite would tear the shared session down.
	_, err = c.Read(make([]byte, 8))
	assert.ErrorIs(t, err, io.EOF)
	assert.Empty(t, rec.forwarded())
	assert.Equal(t, 1, m.Conns(), "the terminated connection gives its budget back")

	// The backend is still there for everyone else.
	_, err = other.Write(query("SELECT 1"))
	require.NoError(t, err)
	assert.Len(t, rec.forwarded(), 1)
}

func TestWriteForwardsARunAsOneCall(t *testing.T) {
	// Parse/Bind/Describe/Execute/Sync must reach the backend together, or it
	// produces no ReadyForQuery and pgx blocks waiting for one.
	rec := newRecorder()
	m, err := New(Config{Exec: rec.exec})
	require.NoError(t, err)
	c := dial(t, m)

	batch := encodeBackend('P', append([]byte("\x00"), "SELECT 1\x00\x00\x00"...))
	batch = append(batch, encodeBackend('B', []byte("\x00\x00\x00\x00\x00\x00\x00"))...)
	batch = append(batch, encodeBackend('E', []byte("\x00\x00\x00\x00\x00"))...)
	batch = append(batch, encodeBackend('S', nil)...)

	_, err = c.Write(batch)
	require.NoError(t, err)

	forwarded := rec.forwarded()
	require.Len(t, forwarded, 1, "one Write of one batch is one execProtocol call")
	assert.Equal(t, batch, forwarded[0])
}

func TestReadDeadlineInterruptsABlockedRead(t *testing.T) {
	// pgx cancels a query by putting the deadline in the past and expecting
	// the blocked Read to return. DBOS's listener sits in exactly such a read
	// for the lifetime of the process.
	m, err := New(Config{Exec: nullExec})
	require.NoError(t, err)
	c := dial(t, m)

	require.NoError(t, c.SetReadDeadline(time.Now().Add(20*time.Millisecond)))

	start := time.Now()
	_, err = c.Read(make([]byte, 8))
	assert.ErrorIs(t, err, os.ErrDeadlineExceeded)
	assert.Less(t, time.Since(start), time.Second)

	var netErr net.Error
	require.ErrorAs(t, err, &netErr)
	assert.True(t, netErr.Timeout(), "pgx only inspects Timeout()")

	// Clearing the deadline makes the connection usable again — pgx reuses a
	// pooled connection after a cancelled query.
	require.NoError(t, c.SetReadDeadline(time.Time{}))
	c.deliver(encodeReadyForQuery('I'))
	assert.Equal(t, encodeReadyForQuery('I'), readN(t, c, 6))
}

func TestReadDrainsBufferedBytesAfterClose(t *testing.T) {
	m, err := New(Config{Exec: nullExec})
	require.NoError(t, err)
	c := dial(t, m)

	c.deliver(encodeReadyForQuery('I'))
	require.NoError(t, c.Close())

	assert.Equal(t, encodeReadyForQuery('I'), readN(t, c, 6))
	_, err = c.Read(make([]byte, 8))
	assert.ErrorIs(t, err, net.ErrClosed)
}

func TestFramingErrorSurfacesAsAPostgresError(t *testing.T) {
	// Failure-matrix row F20 wants a pgx error, not a torn socket, when
	// something goes wrong beneath the pool.
	m, err := New(Config{Exec: nullExec})
	require.NoError(t, err)
	c := dial(t, m)

	_, err = c.Write([]byte{'Q', 0, 0, 0, 1})
	require.ErrorIs(t, err, errMessageTooSmall)

	got := readN(t, c, 5)
	assert.Equal(t, byte(beErrorResponse), got[0])
}

func TestCancelRequestClosesWithoutReply(t *testing.T) {
	rec := newRecorder()
	m, err := New(Config{Exec: rec.exec})
	require.NoError(t, err)

	nc, err := m.DialContext(context.Background(), "tcp", "ignored")
	require.NoError(t, err)
	c := nc.(*Conn)

	_, err = c.Write(startupMessage(cancelRequestCode, []byte{0, 0, 0, 1, 0, 0, 0, 2}))
	require.NoError(t, err)

	_, err = c.Read(make([]byte, 1))
	assert.ErrorIs(t, err, net.ErrClosed)
	assert.Empty(t, rec.forwarded())
}

func TestAddrIsNotANetwork(t *testing.T) {
	m, err := New(Config{Exec: nullExec})
	require.NoError(t, err)
	c := dial(t, m)

	assert.Equal(t, "pglite", c.LocalAddr().Network())
	assert.Equal(t, "pglite", c.RemoteAddr().String())
}
