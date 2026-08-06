package wasmpg

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// backendScript is an [ExecProtocol] whose answer to each round trip is given
// in advance: one ReadyForQuery transaction-status byte per call, in order.
//
// Scripting the status byte is how a test says "the session is now inside a
// transaction block" without a Postgres to say it. That is the right shape,
// because the status byte is exactly what the multiplexer keys off — a double
// that inferred the status from the statement text would be testing the
// inference the production code deliberately does not make.
type backendScript struct {
	mu    sync.Mutex
	sent  [][]byte
	steps []scriptStep
	n     int
}

// scriptStep is one round trip's scripted behaviour. A zero status means the
// reply carries no ReadyForQuery at all, which is what an extended-query batch
// looks like before its Sync arrives.
type scriptStep struct {
	status byte
	err    error
	block  chan struct{} // when set, the round trip waits on it
}

// newScript builds a backend that answers with the given statuses in order and
// with a plain idle ReadyForQuery once the script runs out.
func newScript(statuses ...byte) *backendScript {
	steps := make([]scriptStep, len(statuses))
	for i, s := range statuses {
		steps[i] = scriptStep{status: s}
	}
	return &backendScript{steps: steps}
}

func (b *backendScript) exec(_ context.Context, msg []byte) ([]byte, error) {
	b.mu.Lock()
	b.sent = append(b.sent, append([]byte(nil), msg...))
	step := scriptStep{status: readyIdle}
	if b.n < len(b.steps) {
		step = b.steps[b.n]
	}
	b.n++
	b.mu.Unlock()

	if step.block != nil {
		<-step.block
	}
	if step.err != nil {
		return nil, step.err
	}
	if step.status == 0 {
		// ParseComplete: a real reply that says nothing about the transaction.
		return encodeBackend('1', nil), nil
	}
	return encodeReadyForQuery(step.status), nil
}

// statements returns the SQL text each round trip carried, in the order the
// backend saw it. A round trip carrying no statement contributes an empty
// string, so positions line up with round trips.
func (b *backendScript) statements() []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]string, 0, len(b.sent))
	for _, buf := range b.sent {
		var texts []string
		for len(buf) > 0 {
			msg, n, err := nextFrontendMessage(buf, false)
			if err != nil || n == 0 {
				break
			}
			if sql := msg.sql(); sql != "" {
				texts = append(texts, sql)
			}
			buf = buf[n:]
		}
		out = append(out, strings.Join(texts, " | "))
	}
	return out
}

// backendHeld reports whether the one-in-flight lock is currently taken.
func backendHeld(f *fifoLock) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held
}

// writeQuery sends a simple query and fails the test if the write does.
func writeQuery(t *testing.T, c *Conn, sql string) {
	t.Helper()
	_, err := c.Write(query(sql))
	require.NoError(t, err)
}

// requireBlocked asserts that fn does not finish within a short grace period,
// and returns a channel that closes when it eventually does.
func requireBlocked(t *testing.T, fn func()) chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
		t.Fatal("the call reached the backend while a transaction was open")
	case <-time.After(100 * time.Millisecond):
	}
	return done
}

func requireDone(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("%s never completed", what)
	}
}

func TestBackendIsHeldForTheLengthOfATransaction(t *testing.T) {
	// The defect this whole change exists for: a step inside a DBOS workflow
	// fails with `RELEASE SAVEPOINT can only be used in transaction blocks`
	// because another logical connection's traffic landed inside the
	// transaction, and the workflow then reports SUCCESS having recorded no
	// steps at all.
	script := newScript(readyInTx, readyInTx, readyIdle)
	m, err := New(Config{Exec: script.exec})
	require.NoError(t, err)

	worker, other := dial(t, m), dial(t, m)

	writeQuery(t, worker, "BEGIN")
	assert.True(t, backendHeld(&m.backend), "BEGIN must not give the session up")

	writeQuery(t, worker, "INSERT INTO operation_outputs VALUES (1)")
	assert.True(t, backendHeld(&m.backend), "the session stays held between statements")

	blocked := requireBlocked(t, func() { _, _ = other.Write(query("SELECT 1")) })

	writeQuery(t, worker, "COMMIT")
	requireDone(t, blocked, "the queued connection")

	assert.Equal(t,
		[]string{"BEGIN", "INSERT INTO operation_outputs VALUES (1)", "COMMIT", "SELECT 1"},
		script.statements(),
		"another connection's statement must never land inside the transaction")
}

func TestRollbackReleasesTheBackend(t *testing.T) {
	script := newScript(readyInTx, readyIdle)
	m, err := New(Config{Exec: script.exec})
	require.NoError(t, err)
	c := dial(t, m)

	writeQuery(t, c, "BEGIN")
	require.True(t, backendHeld(&m.backend))

	writeQuery(t, c, "ROLLBACK")
	assert.False(t, backendHeld(&m.backend))
}

func TestAFailedTransactionKeepsTheBackendUntilItIsEnded(t *testing.T) {
	// An error inside a transaction aborts the block without the client having
	// sent anything: the backend reports 'E', and every statement fails until
	// the block is left. 'E' is still an open block, so the session is still
	// not free — this is the case a frontend-only BEGIN/COMMIT watcher gets
	// wrong in both directions, by seeing no COMMIT and holding forever.
	script := newScript(readyInTx, readyFailedTx, readyIdle)
	m, err := New(Config{Exec: script.exec})
	require.NoError(t, err)
	c := dial(t, m)

	writeQuery(t, c, "BEGIN")
	writeQuery(t, c, "SELECT nonexistent_column")
	assert.True(t, backendHeld(&m.backend), "a failed transaction block is still a block")

	writeQuery(t, c, "ROLLBACK")
	assert.False(t, backendHeld(&m.backend))
}

func TestAnAbortedTransactionIsReleasedWhenTheConnectionGoesAway(t *testing.T) {
	// The transaction that sends neither COMMIT nor ROLLBACK: the block failed,
	// the client gave up, and the connection closed. A real backend aborts the
	// transaction when the session ends; here the session outlives the
	// connection, so the shim has to roll it back itself — otherwise the next
	// logical connection is granted a backend that is still inside somebody
	// else's aborted block, and every statement it runs fails with `current
	// transaction is aborted`.
	script := newScript(readyInTx, readyFailedTx)
	m, err := New(Config{Exec: script.exec})
	require.NoError(t, err)

	abandoner, next := dial(t, m), dial(t, m)

	writeQuery(t, abandoner, "BEGIN")
	writeQuery(t, abandoner, "SELECT nonexistent_column")
	require.True(t, backendHeld(&m.backend))

	require.NoError(t, abandoner.Close())
	assert.False(t, backendHeld(&m.backend), "a closed connection must not keep the session")

	writeQuery(t, next, "SELECT 1")
	assert.Equal(t,
		[]string{"BEGIN", "SELECT nonexistent_column", "ROLLBACK", "SELECT 1"},
		script.statements(),
		"the abandoned block must be rolled back before anyone else runs")
}

func TestTerminateMidTransactionRollsBackAndReleases(t *testing.T) {
	// Terminate is the orderly version of the same thing: pgx ends the logical
	// connection while a transaction is open on the shared session.
	script := newScript(readyInTx)
	m, err := New(Config{Exec: script.exec})
	require.NoError(t, err)
	c := dial(t, m)

	writeQuery(t, c, "BEGIN")
	require.True(t, backendHeld(&m.backend))

	_, err = c.Write(terminateMsg())
	require.NoError(t, err)

	assert.False(t, backendHeld(&m.backend))
	assert.Equal(t, []string{"BEGIN", "ROLLBACK"}, script.statements())
}

func TestSavepointsDoNotReleaseTheBackendEarly(t *testing.T) {
	// This is DBOS's own shape: each step runs inside a savepoint, so RELEASE
	// SAVEPOINT arrives repeatedly while the outer transaction stays open. A
	// watcher counting BEGIN and COMMIT in the write path would have to track
	// nesting depth and tell `ROLLBACK TO x` from `ROLLBACK`; the backend's
	// status byte reports 'T' throughout and needs neither.
	script := newScript(readyInTx, readyInTx, readyInTx, readyInTx, readyInTx, readyIdle)
	m, err := New(Config{Exec: script.exec})
	require.NoError(t, err)
	c := dial(t, m)

	for _, sql := range []string{
		"BEGIN",
		"SAVEPOINT dbos_step_1",
		"INSERT INTO operation_outputs VALUES (1)",
		"RELEASE SAVEPOINT dbos_step_1",
		"SAVEPOINT dbos_step_2",
	} {
		writeQuery(t, c, sql)
		assert.True(t, backendHeld(&m.backend), "released the session at %q", sql)
	}

	writeQuery(t, c, "COMMIT")
	assert.False(t, backendHeld(&m.backend))
}

func TestTheStatementTextNeverDecidesWhetherToHold(t *testing.T) {
	// listen_test.go has the negative cases for observeListen because that
	// parser reads statement text and can be fooled by it. This is the same
	// guarantee for transactions, asserted where it now lives: the shim reads
	// the backend's status byte, so a BEGIN inside a string literal, a
	// dollar-quoted body or a comment cannot make it hold the session — and
	// equally, a statement that merely says COMMIT cannot make it let go.
	for _, sql := range []string{
		`SELECT 'BEGIN'`,
		`SELECT $tag$ BEGIN; COMMIT; $tag$`,
		`SELECT 1 -- BEGIN`,
		`/* BEGIN */ SELECT 1`,
		`INSERT INTO notes (body) VALUES ('BEGIN TRANSACTION')`,
	} {
		t.Run(sql, func(t *testing.T) {
			m, err := New(Config{Exec: newScript(readyIdle).exec})
			require.NoError(t, err)
			c := dial(t, m)

			writeQuery(t, c, sql)
			assert.False(t, backendHeld(&m.backend),
				"an idle backend means an idle session, whatever the text said")
		})
	}

	// And the converse: text that looks like the end of a transaction does not
	// release a session the backend still reports as open.
	m, err := New(Config{Exec: newScript(readyInTx, readyInTx).exec})
	require.NoError(t, err)
	c := dial(t, m)

	writeQuery(t, c, "BEGIN")
	writeQuery(t, c, `SELECT 'COMMIT'`)
	assert.True(t, backendHeld(&m.backend))
}

func TestBeginThroughTheExtendedProtocolHoldsTheBackend(t *testing.T) {
	// pgx chooses between the simple and the extended protocol on its own, and
	// may send BEGIN as a parameterised statement. Nothing about the decision
	// depends on which it chose, because the status byte comes back either way.
	script := newScript(readyInTx)
	m, err := New(Config{Exec: script.exec})
	require.NoError(t, err)
	c := dial(t, m)

	payload := append([]byte("\x00"), "BEGIN\x00"...)
	payload = append(payload, 0, 0)
	_, err = c.Write(encodeBackend(feParse, payload))
	require.NoError(t, err)

	assert.True(t, backendHeld(&m.backend))
}

func TestAReplyWithoutReadyForQueryKeepsTheBackend(t *testing.T) {
	// An extended-query batch whose Sync has not arrived produces no
	// ReadyForQuery. Handing the backend on here would let another connection
	// land between a Parse/Bind/Execute and its Sync, which is the same
	// interleaving in a different costume.
	script := newScript(0, readyIdle)
	m, err := New(Config{Exec: script.exec})
	require.NoError(t, err)
	c := dial(t, m)

	writeQuery(t, c, "SELECT 1")
	assert.True(t, backendHeld(&m.backend), "no ReadyForQuery means the session is still busy")

	writeQuery(t, c, "SELECT 2")
	assert.False(t, backendHeld(&m.backend))
}

func TestAFailedRoundTripRollsBackAndReleases(t *testing.T) {
	// execProtocol itself failed, so the backend never reported where it
	// stands. Holding on would wedge the multiplexer over a connection that has
	// already been told its write failed; releasing without a rollback would
	// hand on a session that may still be inside a block.
	script := &backendScript{steps: []scriptStep{
		{status: readyInTx},
		{err: errors.New("pglite: execProtocol rejected the message")},
	}}
	m, err := New(Config{Exec: script.exec})
	require.NoError(t, err)
	c, next := dial(t, m), dial(t, m)

	writeQuery(t, c, "BEGIN")
	require.True(t, backendHeld(&m.backend))

	_, err = c.Write(query("INSERT INTO operation_outputs VALUES (1)"))
	require.Error(t, err)
	assert.False(t, backendHeld(&m.backend))

	writeQuery(t, next, "SELECT 1")
	assert.Equal(t, []string{
		"BEGIN",
		"INSERT INTO operation_outputs VALUES (1)",
		"ROLLBACK",
		"SELECT 1",
	}, script.statements())
}

func TestATransactionIsGrantedItsNextStatementAheadOfWaiters(t *testing.T) {
	// The second statement of a transaction must not queue behind the
	// connections that piled up while the first one ran — the FIFO lock is
	// fair, so a transaction that re-queued for every statement would be
	// overtaken and could never finish.
	script := newScript(readyInTx, readyInTx, readyIdle)
	m, err := New(Config{Exec: script.exec, LogicalConns: 4})
	require.NoError(t, err)

	worker := dial(t, m)
	writeQuery(t, worker, "BEGIN")

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		c := dial(t, m)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Write(query("SELECT waiting"))
		}()
	}
	waitForWaiters(t, &m.backend, 2)

	writeQuery(t, worker, "INSERT INTO operation_outputs VALUES (1)")
	writeQuery(t, worker, "COMMIT")
	wg.Wait()

	got := script.statements()
	require.Len(t, got, 5)
	assert.Equal(t,
		[]string{"BEGIN", "INSERT INTO operation_outputs VALUES (1)", "COMMIT"},
		got[:3],
		"a waiter overtook the open transaction")
}

func TestClosingTheMultiplexerReleasesAnOpenTransaction(t *testing.T) {
	script := newScript(readyInTx)
	m, err := New(Config{Exec: script.exec})
	require.NoError(t, err)

	c := dial(t, m)
	writeQuery(t, c, "BEGIN")
	require.True(t, backendHeld(&m.backend))

	require.NoError(t, m.Close())
	assert.False(t, backendHeld(&m.backend))
	assert.Equal(t, []string{"BEGIN", "ROLLBACK"}, script.statements())
}

func TestAConnectionClosedMidRoundTripDoesNotReleaseTwice(t *testing.T) {
	// Close can arrive from another goroutine while execProtocol is still
	// running. Releasing the lock from both the closer and the round trip would
	// hand the backend to two connections at once; releasing from neither would
	// strand it. Exactly one of them has to do it.
	gate := make(chan struct{})
	script := &backendScript{steps: []scriptStep{{status: readyInTx, block: gate}}}
	m, err := New(Config{Exec: script.exec})
	require.NoError(t, err)

	c, next := dial(t, m), dial(t, m)

	writing := make(chan struct{})
	go func() {
		defer close(writing)
		_, _ = c.Write(query("BEGIN"))
	}()
	// The round trip is now inside the backend, holding the lock.
	waitForInFlight(t, c)

	require.NoError(t, c.Close())
	close(gate)
	requireDone(t, writing, "the interrupted write")

	assert.False(t, backendHeld(&m.backend), "the session was stranded by the close")

	// A second acquisition proves the lock is genuinely free rather than
	// double-released into a state where two connections believe they hold it.
	writeQuery(t, next, "SELECT 1")
	assert.False(t, backendHeld(&m.backend))
}

// waitForInFlight blocks until c has a round trip running inside the backend.
func waitForInFlight(t *testing.T, c *Conn) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c.hold.mu.Lock()
		inFlight := c.hold.inFlight
		c.hold.mu.Unlock()
		if inFlight {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the round trip never reached the backend")
}

func TestLastReadyForQueryReadsTheEndOfABatch(t *testing.T) {
	// One round trip can carry `COMMIT; BEGIN;`, which answers with an 'I' and
	// then a 'T'. Only the last one describes the state the session was left in.
	batch := append(encodeReadyForQuery(readyIdle), encodeReadyForQuery(readyInTx)...)
	status, ok := lastReadyForQuery(batch)
	require.True(t, ok)
	assert.Equal(t, byte(readyInTx), status)
	assert.Equal(t, sessionInTx, sessionOutcome(batch))

	reversed := append(encodeReadyForQuery(readyInTx), encodeReadyForQuery(readyIdle)...)
	status, ok = lastReadyForQuery(reversed)
	require.True(t, ok)
	assert.Equal(t, byte(readyIdle), status)
	assert.Equal(t, sessionIdle, sessionOutcome(reversed))

	// A reply with no ReadyForQuery, and one whose framing is lost, both mean
	// "assume the session is still busy".
	_, ok = lastReadyForQuery(encodeBackend('1', nil))
	assert.False(t, ok)
	assert.Equal(t, sessionInTx, sessionOutcome(encodeBackend('1', nil)))
	assert.Equal(t, sessionInTx, sessionOutcome([]byte{'Z', 0, 0}))
	assert.Equal(t, sessionInTx, sessionOutcome(nil))

	// A ReadyForQuery is one payload byte. Anything else claiming to be one is
	// malformed, and is ignored rather than indexed into.
	assert.Equal(t, sessionInTx, sessionOutcome(encodeBackend(beReadyForQuery, nil)))
}
