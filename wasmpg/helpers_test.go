package wasmpg

import (
	"context"
	"encoding/binary"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The backend is an [ExecProtocol] function, so a test supplies a closure
// rather than a generated double: there is no interface to mock, and a
// function is the whole dependency.

// nullExec is a backend that answers everything with a bare ReadyForQuery.
func nullExec(context.Context, []byte) ([]byte, error) {
	return encodeReadyForQuery(readyIdle), nil
}

// recorder is an ExecProtocol that records what reached the backend, so that
// ordering and "this never got forwarded" can both be asserted.
type recorder struct {
	mu   sync.Mutex
	sent [][]byte

	// reply, when set, produces the backend's answer for a request.
	reply func(msg []byte) []byte

	// gate, when set, blocks the backend until it is closed — which is how a
	// request is held in flight while another is queued behind it.
	gate chan struct{}

	// entered is signalled as each request reaches the backend.
	entered chan []byte
}

func newRecorder() *recorder {
	return &recorder{entered: make(chan []byte, 64)}
}

func (r *recorder) exec(_ context.Context, msg []byte) ([]byte, error) {
	cp := append([]byte(nil), msg...)

	r.mu.Lock()
	r.sent = append(r.sent, cp)
	r.mu.Unlock()

	select {
	case r.entered <- cp:
	default:
	}

	if r.gate != nil {
		<-r.gate
	}
	if r.reply != nil {
		return r.reply(cp), nil
	}
	return encodeReadyForQuery(readyIdle), nil
}

func (r *recorder) forwarded() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.sent...)
}

// startupMessage builds an untyped startup-phase message: a self-inclusive
// length, a request code, and an optional body.
func startupMessage(code int32, body []byte) []byte {
	out := binary.BigEndian.AppendUint32(nil, uint32(len(body)+8))
	out = binary.BigEndian.AppendUint32(out, uint32(code))
	return append(out, body...)
}

// query builds a simple Query ('Q') message.
func query(sql string) []byte { return encodeQuery(sql) }

// terminate builds a Terminate ('X') message.
func terminateMsg() []byte {
	return encodeBackend(feTerminate, nil)
}

// dial opens a logical connection and completes the handshake, leaving the
// connection where pgx would have it: past startup, with the handshake bytes
// already drained.
func dial(t *testing.T, m *Multiplexer) *Conn {
	t.Helper()

	nc, err := m.DialContext(context.Background(), "tcp", "pglite:5432")
	require.NoError(t, err)
	c := nc.(*Conn)

	_, err = c.Write(startupMessage(sslRequestCode, nil))
	require.NoError(t, err)
	require.Equal(t, []byte{'N'}, readN(t, c, 1))

	_, err = c.Write(startupMessage(protocolVersion3, []byte("user\x00postgres\x00\x00")))
	require.NoError(t, err)
	drain(t, c, len(m.handshake(c.PID())))

	return c
}

// readN reads exactly n bytes, failing the test rather than hanging.
func readN(t *testing.T, c *Conn, n int) []byte {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(2*time.Second)))
	defer func() { require.NoError(t, c.SetReadDeadline(time.Time{})) }()

	out := make([]byte, 0, n)
	for len(out) < n {
		buf := make([]byte, n-len(out))
		read, err := c.Read(buf)
		require.NoError(t, err)
		out = append(out, buf[:read]...)
	}
	return out
}

func drain(t *testing.T, c *Conn, n int) {
	t.Helper()
	readN(t, c, n)
}
