package wasmpg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
)

// ExecProtocol hands raw frontend wire bytes to the single PGlite backend and
// returns the raw backend bytes it produced.
//
// It is PGlite's `execProtocol`, and deliberately not `execProtocolRaw`
// (D7, SPEC.md §12.2): the Raw variants bypass the wrappers that manage
// notification listeners, which is exactly the machinery DBOS depends on for
// queue dispatch and stream reads. Choosing Raw looks simpler and reappears
// later as failure-matrix row F21.
//
// Modelling the backend as a function rather than a js.Value is what lets the
// multiplexer be tested off-target; pglite_js.go supplies the real one.
type ExecProtocol func(ctx context.Context, msg []byte) ([]byte, error)

// Parameter is one ParameterStatus the synthesised handshake reports.
type Parameter struct {
	Name  string
	Value string
}

const (
	// DefaultLogicalConns is the logical-connection budget, and therefore the
	// value `pgxpool.Config.MaxConns` must carry (SPEC.md §12.4).
	//
	// One of these is DBOS's notification listener, which parks inside
	// WaitForNotification for the lifetime of the process and never gives its
	// connection back. The remaining three serve queue polling, workflow
	// status writes and application queries. Raising it costs nothing but
	// memory — the backend is single-threaded either way — and lowering it to
	// 1 deadlocks the runtime outright.
	DefaultLogicalConns = 4

	// MinLogicalConns is the smallest budget that can make progress: one for
	// the listener, one for everything else.
	MinLogicalConns = 2

	// DefaultBackendPID is the synthetic PID reported in BackendKeyData and
	// stamped on injected NotificationResponse frames. PGlite runs one
	// backend and reports no PID of its own, so the value is arbitrary; it is
	// only ever compared for equality.
	DefaultBackendPID int32 = 40000

	// DefaultServerVersion is what the synthesised handshake reports for
	// `server_version`. It is deliberately a plain dotted version rather than
	// the engine's own `19beta2`: callers that parse the first component with
	// strconv choke on the beta suffix, and the shim has no reason to inflict
	// that on them.
	DefaultServerVersion = "19.0"
)

// Errors the multiplexer returns to its callers.
var (
	// ErrBudgetExhausted means more logical connections were dialled than
	// [Config.LogicalConns] allows. It is returned rather than blocked on
	// deliberately: a pool that waits here waits forever, because the
	// connections in use include one that is never coming back.
	ErrBudgetExhausted = errors.New("wasmpg: logical connection budget exhausted")

	// ErrClosed is returned by operations on a closed connection or
	// multiplexer.
	ErrClosed = errors.New("wasmpg: connection closed")
)

// Config configures a [Multiplexer].
type Config struct {
	// Exec is the PGlite backend. Required.
	Exec ExecProtocol

	// LogicalConns is the connection budget, counting DBOS's listener.
	// Zero means [DefaultLogicalConns]; anything below [MinLogicalConns] is
	// rejected.
	LogicalConns int

	// ServerVersion overrides the reported `server_version`.
	ServerVersion string

	// Params replaces the whole ParameterStatus set the handshake reports.
	// Empty means the SPEC.md §12.3 set.
	Params []Parameter

	// BackendPID overrides [DefaultBackendPID].
	BackendPID int32

	// RouteInlineNotifications re-routes NotificationResponse frames found
	// inside an execProtocol result through the channel table, instead of
	// dropping them.
	//
	// It defaults to false because PGlite's execProtocol is documented to
	// dispatch notifications to onNotification as well, making the inline
	// copy a duplicate that reaches only the connection that happened to run
	// the statement. That has not been verified against the real bundle here
	// — no browser was available — so this exists as the one-line correction
	// if notifications turn out to arrive inline only. Its cost when wrong in
	// the other direction is a duplicate notification, which DBOS tolerates:
	// it re-reads the database on every wake-up.
	RouteInlineNotifications bool
}

// defaultParams is the ParameterStatus set of SPEC.md §12.3, in the order the
// handshake sends it.
func defaultParams(serverVersion string) []Parameter {
	return []Parameter{
		{"server_version", serverVersion},
		{"client_encoding", "UTF8"},
		{"DateStyle", "ISO, MDY"},
		{"TimeZone", "UTC"},
		{"integer_datetimes", "on"},
	}
}

// Multiplexer serialises N logical connections onto PGlite's single backend
// session and routes asynchronous NotificationResponse frames (SPEC.md §8.6,
// §12.4).
type Multiplexer struct {
	exec   ExecProtocol
	budget int
	params []Parameter
	pid    int32

	routeInline bool

	// backend is held only across a single execProtocol round trip.
	backend fifoLock

	mu      sync.Mutex
	conns   map[*Conn]struct{}
	routes  map[string]map[*Conn]struct{}
	nextPID int32
	cleanup []func()
	closed  bool
}

// New builds a multiplexer over an existing backend function. The browser
// entry point is [Dialer]; this is what it is built on, and what tests use.
func New(cfg Config) (*Multiplexer, error) {
	if cfg.Exec == nil {
		return nil, errors.New("wasmpg: Config.Exec is required")
	}

	budget := cfg.LogicalConns
	if budget == 0 {
		budget = DefaultLogicalConns
	}
	if budget < MinLogicalConns {
		return nil, fmt.Errorf(
			"wasmpg: LogicalConns %d is below the minimum of %d: DBOS's notification listener holds one connection for the lifetime of the process, so a smaller budget cannot run a query",
			budget, MinLogicalConns)
	}

	version := cfg.ServerVersion
	if version == "" {
		version = DefaultServerVersion
	}
	params := cfg.Params
	if len(params) == 0 {
		params = defaultParams(version)
	}
	pid := cfg.BackendPID
	if pid == 0 {
		pid = DefaultBackendPID
	}

	return &Multiplexer{
		exec:        cfg.Exec,
		budget:      budget,
		params:      params,
		pid:         pid,
		routeInline: cfg.RouteInlineNotifications,
		conns:       make(map[*Conn]struct{}),
		routes:      make(map[string]map[*Conn]struct{}),
		nextPID:     pid,
	}, nil
}

// MaxConns is the value to assign to `pgxpool.Config.MaxConns`.
//
// The types line up on purpose: the budget and the pool limit are the same
// number, and setting the pool higher than the budget turns a deadlock into a
// loud [ErrBudgetExhausted] rather than fixing anything.
func (m *Multiplexer) MaxConns() int32 { return int32(m.budget) }

// DialContext opens a logical connection. Its signature is pgconn's DialFunc,
// so it can be assigned straight to `pgxpool.Config.ConnConfig.DialFunc`; the
// network and address are ignored, because there is no network.
func (m *Multiplexer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.closed {
		return nil, ErrClosed
	}
	if len(m.conns) >= m.budget {
		return nil, fmt.Errorf("%w: %d of %d in use", ErrBudgetExhausted, len(m.conns), m.budget)
	}

	m.nextPID++
	c := newConn(m, m.nextPID)
	m.conns[c] = struct{}{}
	return c, nil
}

// Notify injects a notification into every logical connection registered for
// the channel (SPEC.md §12.4). It is what PGlite's onNotification callback
// calls, and it is the multiplexer's only notification ingress.
//
// A notification for a channel nobody registered is dropped, which is the same
// thing a real backend does for a session that never issued LISTEN.
func (m *Multiplexer) Notify(channel, payload string) {
	m.mu.Lock()
	targets := make([]*Conn, 0, len(m.routes[channel]))
	for c := range m.routes[channel] {
		targets = append(targets, c)
	}
	pid := m.pid
	m.mu.Unlock()

	if len(targets) == 0 {
		return
	}

	frame := encodeNotificationResponse(pid, channel, payload)
	for _, c := range targets {
		c.deliver(frame)
	}
}

// Listeners reports how many logical connections are registered for a channel.
// It exists so the routing table can be asserted on directly.
func (m *Multiplexer) Listeners(channel string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.routes[channel])
}

// Conns reports how many logical connections are open, against the budget.
func (m *Multiplexer) Conns() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.conns)
}

// Close shuts every logical connection and releases whatever the JavaScript
// side registered. The PGlite backend itself is not touched: it outlives the
// multiplexer, the same way it outlives a Terminate (SPEC.md §12.3).
func (m *Multiplexer) Close() error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil
	}
	m.closed = true
	conns := make([]*Conn, 0, len(m.conns))
	for c := range m.conns {
		conns = append(conns, c)
	}
	cleanup := m.cleanup
	m.cleanup = nil
	m.mu.Unlock()

	for _, c := range conns {
		_ = c.Close()
	}
	for _, fn := range cleanup {
		fn()
	}
	return nil
}

// addCleanup registers a function to run at Close. pglite_js.go uses it to
// release the js.Func values it handed to JavaScript.
func (m *Multiplexer) addCleanup(fn func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cleanup = append(m.cleanup, fn)
}

// submit runs one execProtocol round trip on behalf of c, holding the
// one-in-flight lock for its duration and no longer.
func (m *Multiplexer) submit(ctx context.Context, c *Conn, msg []byte) error {
	if err := m.backend.acquire(ctx, c.done); err != nil {
		return err
	}
	defer m.backend.release()

	out, err := m.exec(ctx, msg)
	if err != nil {
		return err
	}

	clean, inline := splitNotifications(out)
	if len(clean) > 0 {
		c.deliver(clean)
	}
	if m.routeInline {
		for _, n := range inline {
			m.Notify(n.channel, n.payload)
		}
	}
	return nil
}

// apply updates the routing table from one observed LISTEN or UNLISTEN.
//
// It is applied when the statement is written rather than when the backend
// confirms it. Registering early can only deliver a notification to a
// connection that is about to be listening; waiting would drop one that
// arrives while the LISTEN is in flight, and a dropped queue notification is
// how a stalled worker starts.
func (m *Multiplexer) apply(c *Conn, op listenOp) {
	m.mu.Lock()
	defer m.mu.Unlock()

	switch op.kind {
	case listenRegister:
		reg, ok := m.routes[op.channel]
		if !ok {
			reg = make(map[*Conn]struct{})
			m.routes[op.channel] = reg
		}
		reg[c] = struct{}{}
	case listenUnregister:
		m.unregisterLocked(c, op.channel)
	case listenUnregisterAll:
		for channel := range m.routes {
			m.unregisterLocked(c, channel)
		}
	}
}

// forget drops every registration a connection held. It runs on close, so a
// notification never chases a connection that has gone away.
func (m *Multiplexer) forget(c *Conn) {
	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.conns, c)
	for channel := range m.routes {
		m.unregisterLocked(c, channel)
	}
}

func (m *Multiplexer) unregisterLocked(c *Conn, channel string) {
	reg, ok := m.routes[channel]
	if !ok {
		return
	}
	delete(reg, c)
	if len(reg) == 0 {
		delete(m.routes, channel)
	}
}

// handshake is the byte sequence a StartupMessage is answered with, in the
// order SPEC.md §12.3 lists: AuthenticationOk, the ParameterStatus set,
// BackendKeyData, and ReadyForQuery reporting an idle transaction status.
func (m *Multiplexer) handshake(pid int32) []byte {
	out := encodeAuthenticationOk()
	for _, p := range m.params {
		out = append(out, encodeParameterStatus(p.Name, p.Value)...)
	}
	// The cancellation secret is derived from the PID rather than randomised:
	// there is no second connection to cancel over, and a random value here
	// would be a `crypto/rand` call in a package the determinism analyser has
	// no reason to trust.
	out = append(out, encodeBackendKeyData(pid, pid^0x5eed)...)
	return append(out, encodeReadyForQuery(readyIdle)...)
}
