// Package wasmpg implements a net.Conn over PGlite's `execProtocol`, so that
// pgx — and therefore DBOS, and therefore the whole AgentIQ runtime — can talk
// to a PostgreSQL 19 engine that lives inside the browser tab (SPEC.md §8.6,
// §12).
//
// # The problem
//
// `dbos.NewDataSource` requires a `*pgxpool.Pool`; pgx requires a `net.Conn`; a
// WASM sandbox cannot open a TCP socket. PGlite is a single-connection,
// single-user-mode Postgres that skips startup and authentication entirely and
// exposes the wire protocol as one JavaScript call (SPEC.md §12.1).
//
// # The split, and why it is not what §12 literally says
//
// SPEC.md §5 and §8.6 tag the whole package `//go:build js && wasm`. That tag
// is on exactly one file here — pglite_js.go, the only file that imports
// `syscall/js`. Everything the multiplexer actually does — framing frontend
// messages, synthesising the handshake, observing LISTEN/UNLISTEN, encoding
// NotificationResponse, serialising one request in flight — is plain Go over a
// [ExecProtocol] function value, and builds on every platform.
//
// The reason is testability. There is no Node runtime and no browser on the
// machines this is developed on, so a fully js/wasm-tagged package would be a
// package no test ever executes: `go test ./wasmpg/...` would compile nothing
// and report ok. With the split, the protocol logic runs under `go test` on the
// host and in CI's unit-test job, and only the `js.Value` marshalling — which
// no test can reach without a browser anyway — is behind the tag.
//
// This costs nothing elsewhere. `syscall/js` still appears in exactly one file
// in one package, so the §17.2 `no-js-outside-wasmpg` depguard rule is
// unaffected, and the untagged files import only the standard library, so
// nothing leaks into a non-browser build.
//
// # How the deadlock is avoided
//
// One PGlite backend serves N logical connections, and DBOS parks one of them
// forever inside `pgconn.WaitForNotification`. Two rules keep that from
// wedging the runtime:
//
//  1. The one-in-flight lock is taken in [Conn.Write] and is never held across
//     a [Conn.Read]. A connection blocked in WaitForNotification is blocked in
//     Read, holds no lock, and cannot stall any other connection.
//
//  2. The logical-connection budget counts DBOS's listener. [Config.LogicalConns]
//     is what `pgxpool.Config.MaxConns` must be set to — see [Multiplexer.MaxConns]
//     — and dialling past it fails loudly with [ErrBudgetExhausted] instead of
//     blocking. A budget of 1 would hand the only connection to the listener
//     and never run a query again, so [MinLogicalConns] is 2 and [New] rejects
//     anything smaller. The usable concurrency is always budget-1.
//
// # Transactions
//
// The lock is held for the length of a transaction, not for one round trip.
// PGlite is a single backend *session*, and a transaction is session-scoped
// state: if another logical connection's traffic lands between a BEGIN and its
// COMMIT, it joins a transaction it never opened, and the connection that did
// open it finds its savepoints gone. The unit that has to be serialised is
// therefore the transaction. [Multiplexer.submit] explains why the signal is
// the backend's own ReadyForQuery transaction-status byte rather than a parser
// watching BEGIN and COMMIT go past in the write path.
//
// This narrows the concurrency the shim offers without changing the budget
// arithmetic above: a transaction was never safely concurrent with anything,
// so what used to happen in parallel was not work, it was corruption.
// [Multiplexer.DialContext] still admits budget connections and still fails
// loudly rather than blocking, because dialling does not touch the lock.
//
// What it does add is a way to stall: a logical connection that opens a
// transaction and then waits on something that itself needs a connection will
// wait forever, because the second connection cannot reach the backend. That is
// the ordinary pool-reentrancy deadlock — DBOS does not do it — and it is
// bounded rather than fatal, because the wait happens inside [Conn.Write] under
// the write deadline pgx derives from the caller's context.
//
// A connection that goes away mid-transaction rolls the session back on its way
// out ([Conn.abandonHold]). A real backend does that for free when the session
// ends; here the session outlives every connection, so it has to be said.
//
// # Notifications
//
// `LISTEN` registrations are session-global against a single backend, but pgx
// delivers a NotificationResponse only to the connection it arrives on. That
// asymmetry is the whole reason [Multiplexer] keeps a channel → connection
// routing table: it watches LISTEN/UNLISTEN go past in the write path, and
// injects a synthesised NotificationResponse frame into the read buffer of
// every logical connection registered for the channel (SPEC.md §12.4).
package wasmpg
