//go:build js && wasm

package main

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gaarutyunov/agentiq/wasmpg"
)

// dsn is a placeholder. Every part of it that describes *where* the server is
// — host, port, and the TCP connection itself — is ignored, because
// [wasmpg.Multiplexer.DialContext] is installed as the DialFunc and there is no
// network in a WASM sandbox (SPEC.md §12.1).
//
// It still has to parse, and two of its settings still matter:
//
//   - `sslmode=disable` stops pgx from offering TLS. The multiplexer answers
//     an SSLRequest with 'N' either way (SPEC.md §12.3), so this is a round
//     trip saved rather than a correctness fix — but a TLS handshake against a
//     backend that has no socket is a failure mode worth not having.
//   - the user and database names are what pgx puts in the StartupMessage. The
//     multiplexer answers that locally and never forwards it, because PGlite
//     runs in single-user mode with no authentication at all, so the values are
//     cosmetic and are set to PGlite's own defaults to keep logs unsurprising.
const dsn = "postgres://postgres@127.0.0.1:5432/postgres?sslmode=disable"

// newPool builds the pgxpool the whole runtime runs on — DBOS's system
// database, DBOS's notification listener, and the generated client all share it.
//
// The one piece of wiring that cannot be defaulted is MaxConns. `wasmpg` does
// not import pgxpool (it models the backend as a function so it can be tested
// off-target), so nothing inside it can set the pool limit; and a pool allowed
// to open more logical connections than the multiplexer's budget does not
// degrade gracefully. It fails at dial time with
// [wasmpg.ErrBudgetExhausted] — loudly, and by design, because the alternative
// is a pool that waits for a connection that is never coming back: one of the
// budget is DBOS's listener, parked in WaitForNotification for the lifetime of
// the page (SPEC.md §12.4, wasmpg's package doc).
//
// Hence [wasmpg.Multiplexer.MaxConns] rather than the
// [wasmpg.DefaultLogicalConns] constant: the budget is whatever the multiplexer
// was actually built with, and reading it from the multiplexer is what keeps
// the two from drifting if a caller ever passes a Config.LogicalConns.
func newPool(ctx context.Context, mux *wasmpg.Multiplexer) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse pool config: %w", err)
	}

	cfg.MaxConns = mux.MaxConns()

	// MinConns stays 0 so the pool opens nothing until something asks. Opening
	// eagerly would spend budget before `dbos migrate` has run, on connections
	// that then sit idle behind a single-threaded backend.
	cfg.MinConns = 0

	// The idle and lifetime reapers exist to recycle connections a network or a
	// server-side timeout may have killed. Neither can happen here: there is
	// one backend, in this tab, for as long as the tab lives. Recycling is
	// therefore pure churn against a budget of four, so both are pushed past
	// any plausible tab lifetime.
	//
	// They are set to a long duration and *not to zero*. Zero is not "no
	// limit" in pgxpool: MaxConnLifetime is added to time.Now() to compute the
	// connection's expiry, so zero expires every connection the instant it is
	// created, and HealthCheckPeriod is handed to time.NewTicker, which panics
	// on a non-positive interval. Both were found the only way they could be —
	// by running the demo in a browser, where the panic arrives as `exit
	// code: 2` in the console and a page that never finishes booting.
	const tabLifetime = 24 * time.Hour
	cfg.MaxConnLifetime = tabLifetime
	cfg.MaxConnIdleTime = tabLifetime

	// The health check still runs: it is also what would restore MinConns, and
	// a ticker is required to be positive. Hourly is often enough to be a
	// liveness check and rare enough not to spend round trips on a
	// single-threaded backend.
	cfg.HealthCheckPeriod = time.Hour

	// Named prepared statements cannot be used here, and this is the sharpest
	// edge in the whole browser build.
	//
	// pgx's default mode caches a prepared statement per connection under a
	// name derived from the SQL text — `stmtcache_<hash>`, the same name on
	// every connection that runs the same statement. That is correct against a
	// real server, where each connection is its own session. It is wrong here:
	// the four logical connections share one PGlite backend and therefore one
	// *session*, so prepared statements are global. The second connection to
	// run a statement the first already prepared gets
	//
	//	prepared statement "stmtcache_…" already exists
	//
	// which aborts the surrounding transaction, and the failure surfaces
	// somewhere else entirely — DBOS's launch reported it as "failed to
	// recover pending workflows", three calls away from the cause.
	//
	// Every one of pgx's five modes was tried in a browser against the real
	// bundle. None is unreservedly correct, and the reason is not pgx's fault:
	// multiplexing several pgx connections onto one *session* breaks an
	// assumption the extended query protocol is built on. What follows is the
	// least-bad of the five, and the residual defect is stated so nobody has to
	// rediscover it.
	//
	//   - QueryExecModeCacheStatement (pgx's default) names statements by SQL
	//     hash, so two connections running the same SQL collide:
	//     `prepared statement "stmtcache_…" already exists`. Loud, and fatal at
	//     DBOS launch.
	//   - QueryExecModeDescribeExec Prepares the *unnamed* statement in one
	//     round trip and executes it in a second, so another connection's Parse
	//     in between destroys it: `unnamed prepared statement does not exist`.
	//   - QueryExecModeExec is the only single-round-trip extended-protocol
	//     mode, and it is session-safe — but it forces *text* format for
	//     parameters and results, and that silently corrupts DBOS. DBOS passes
	//     `[]byte` for TEXT columns; in text format pgx encodes a `[]byte` as
	//     bytea hex, so `authenticated_roles` is written as the literal string
	//     `\x5b...` and read back as `failed to unmarshal authenticated_roles:
	//     invalid character '\'`. A mode that writes bad data is worse than one
	//     that fails.
	//   - QueryExecModeSimpleProtocol interpolates arguments client-side and
	//     needs the same OID-0 encode plans QueryExecModeExec does, with the
	//     same text-format consequences.
	//   - QueryExecModeCacheDescribe — this one. It caches the statement
	//     *description* per connection and executes through the unnamed
	//     statement, keeping binary formats, so DBOS round-trips its own data
	//     correctly.
	//
	// The residual defect: on a cache *miss* CacheDescribe has the same
	// two-round-trip Prepare-then-Execute shape as DescribeExec, so two
	// connections meeting the same SQL for the first time simultaneously can
	// still race. It is bounded — DBOS issues a fixed set of statements, so the
	// window closes once each connection has seen each one — and it fails
	// loudly rather than corrupting anything.
	//
	// The real fix belongs in wasmpg, not here: the multiplexer knows the
	// session is shared and could either give each logical connection its own
	// statement namespace or hold its one-in-flight lock across a
	// Prepare/Execute pair. Neither is expressible from the pool side, which is
	// why this comment exists instead of a fix.
	cfg.ConnConfig.DefaultQueryExecMode = pgx.QueryExecModeCacheDescribe

	// No named statements are ever created, so the statement cache would only
	// hold things that must not exist. The description cache is load-bearing in
	// this mode — pgx refuses to run with it disabled — and is left at pgx's
	// default size.
	cfg.ConnConfig.StatementCacheCapacity = 0

	// No named statements are ever created, so the statement cache would only
	// hold things that must not exist. The description cache is load-bearing in
	// this mode — pgx refuses to run with it disabled — and is left at pgx's
	// default size.

	// QueryExecModeExec's one cost, paid here.
	//
	// Because the mode never asks the server to describe a statement, pgx has
	// to infer each parameter's PostgreSQL type from the Go value alone. It
	// does that well for the standard library and for pgtype's own types, and
	// it fails for a *slice of a named string type*, which is exactly what
	// DBOS's own ListWorkflows passes:
	//
	//	unable to encode []models.WorkflowStatusType{"PENDING"} into text
	//	format for unknown type (OID 0): cannot find encode plan
	//
	// The type is reachable — `dbos.WorkflowStatusType` is an exported alias
	// for it — so the mapping is declared once per connection. Registering it
	// is not a workaround for the transport; it is the ordinary way to tell
	// pgx about a type it has no way to guess.
	//
	// If a later milestone adds an argument pgx cannot infer, it fails the same
	// way, names the Go type in the message, and is fixed by one more line here.
	//
	// The array name is pgtype's own — `_text`, PostgreSQL's internal name for
	// text[] — and not `text[]`, which registers silently and resolves to
	// nothing, leaving the identical error to be diagnosed a second time.
	cfg.AfterConnect = func(_ context.Context, conn *pgx.Conn) error {
		m := conn.TypeMap()
		m.RegisterDefaultPgType(dbos.WorkflowStatusType(""), "text")
		m.RegisterDefaultPgType([]dbos.WorkflowStatusType(nil), "_text")
		return nil
	}

	cfg.ConnConfig.DialFunc = mux.DialContext

	// pgconn resolves the host before it dials. Under js/wasm the resolver has
	// no way to answer, so the name is passed straight through: whatever comes
	// out of here is handed to DialFunc, which ignores it.
	cfg.ConnConfig.LookupFunc = func(_ context.Context, host string) ([]string, error) {
		return []string{host}, nil
	}

	// A dial that cannot make progress should fail rather than hang the page.
	// The budget check is synchronous, so this bounds the handshake, not the
	// wait for a free slot — there is no such wait (see above).
	cfg.ConnConfig.ConnectTimeout = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("open pool: %w", err)
	}
	return pool, nil
}

// assert that the multiplexer really is usable as a DialFunc without an
// adapter. pgconn.DialFunc is an unexported-in-spirit type alias; if a pgx
// upgrade changes its shape, this fails at compile time in the one file that
// cares, rather than at the first dial in a browser.
var _ func(context.Context, string, string) (net.Conn, error) = (*wasmpg.Multiplexer)(nil).DialContext
