//go:build js && wasm

// Command demo is the AgentIQ browser entry point (SPEC.md §4, §12).
//
// It runs the production runtime — real DBOS, real pgx, real generated client —
// inside a browser tab against a real PostgreSQL 19 engine. Exactly one thing
// differs from the server build in cmd/agentiq: the transport beneath pgx is
// `wasmpg` over PGlite's `execProtocol` instead of a TCP socket
// (SPEC.md §12.2). Everything above that line is the same code.
//
// What the page demonstrates, in the order it happens:
//
//  1. PGlite boots from IndexedDB — the same database as the last page load.
//  2. `dbos.NewContext` runs the `dbos.*` migrations *through the shim*, which
//     is SPEC.md §16's "`dbos migrate` succeeds against PGlite" acceptance.
//     There is no `dbos` CLI in a WASM page and there is no pre-seeded data
//     directory; the migrations are library calls over the pgxpool, which is
//     the reading SPEC.md §16 left open and this file settles.
//  3. `migrate.Apply` creates the `agentiq` schema, applies the generated
//     `agentiq.*` table history and runs the generated `CREATE PROPERTY GRAPH`.
//     It is the same call, in the same order, that `cmd/agentiq` and the
//     integration harness make.
//  4. A notification probe answers the one question SPEC.md §12.4 left open
//     (see probe.go) — the highest-value thing a real browser can report.
//  5. `dbos.Launch` recovers whatever the previous tab left in flight, which is
//     failure-matrix row F22, and starts the queue worker.
//  6. Clicking Start enqueues the two-step workflow; the table is filled by the
//     generated `GRAPH_TABLE` traversal, which is SPEC.md §16's other M1
//     acceptance.
//
// # Transaction isolation between logical connections
//
// Driving the deployed page used to show every step of 1 to 6 working except
// the last part of 6: an enqueued workflow reached PENDING, its first step
// failed with
//
//	failed to release savepoint: RELEASE SAVEPOINT can only be used in
//	transaction blocks
//
// and the workflow then reported SUCCESS having recorded no steps at all.
//
// It was never a bug in this file, and no setting in pool.go could fix it.
// PGlite is one backend *session*, and SPEC.md §12.4's multiplexer gives N
// logical connections a share of it. Session-scoped state is therefore shared,
// and a transaction is session-scoped state: connection A opens one, connection
// B commits or rolls back, and A's savepoint is gone.
//
// The fix is in `wasmpg`, where it belonged: the multiplexer now holds its
// one-in-flight lock for the length of a transaction rather than for one round
// trip, keyed off the backend's own ReadyForQuery transaction-status byte
// ('I' idle, 'T' in a transaction, 'E' in a failed one) rather than off a
// parser watching BEGIN and COMMIT go past. See [wasmpg.Multiplexer]'s submit
// for why the status byte and not the statement text.
//
// Two things about that are worth knowing here rather than rediscovering:
//
//   - The serialisation is asserted off-target, in `wasmpg`'s own suite, which
//     runs without a browser because the backend is an `ExecProtocol` function
//     value. What that proves is that the shim never hands the session on while
//     the backend reports a transaction open, and rolls one back that a
//     connection abandoned. Whether the workflow now records its steps is a
//     question only a real PGlite can answer, and test/browser is what asks it.
//   - It does *not* close the prepared-statement race pool.go describes. That
//     race is between two Parse/Execute pairs, each of which leaves the session
//     idle, so a transaction-scoped lock does not cover it. pool.go's
//     QueryExecModeCacheDescribe is still the mitigation.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"syscall/js"
	"time"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/gaarutyunov/gopgql/exec"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gaarutyunov/agentiq/generated/client"
	"github.com/gaarutyunov/agentiq/migrate"
	"github.com/gaarutyunov/agentiq/wasmpg"
	"github.com/gaarutyunov/agentiq/workflow"
)

const (
	appName = "agentiq"

	// defaultDataDir is IndexedDB-backed, which is not a preference: GitHub
	// Pages cannot set COOP/COEP, so there is no SharedArrayBuffer and PGlite's
	// OPFS-with-SAB mode is unavailable (SPEC.md §3.3, §12.6, §13.3).
	defaultDataDir = "idb://agentiq-m1"

	// executorID and appVersion are pinned rather than derived, and this is
	// what makes the reload story work. DBOS recovers the workflows left
	// PENDING by *this executor* at *this application version*; a value that
	// changed per page load would leave every interrupted workflow orphaned,
	// and failure-matrix row F22 — reload the tab, the workflow resumes —
	// would quietly assert nothing. One tab is one executor.
	executorID = "browser"
	appVersion = "m1"

	// logicalConns overrides [wasmpg.DefaultLogicalConns], which is 4.
	//
	// Four is not enough for this page, and the way it runs out is nasty:
	// pgxpool *blocks* on Acquire when it is at MaxConns rather than failing,
	// so the symptom is a page that reaches "ready" and then does nothing at
	// all — no error, no log line, no further round trip. DBOS holds its
	// notification listener forever and takes more for its queue runner and
	// scheduler, and the demo then wants one for the refresh loop and one for
	// a click. That is more than three.
	//
	// Raising it is close to free. The multiplexer serialises every round trip
	// against one backend either way, so a logical connection is a small struct
	// and a channel, not a socket or a server-side process; wasmpg's own
	// documentation says as much. Eight leaves headroom for the milestones that
	// add work to the same page rather than sizing it exactly to today's
	// callers, which is the kind of number that silently becomes wrong.
	logicalConns = 8

	// demoDigest stands in for a real agent digest. M1 has no agent
	// (SPEC.md §20 M1); the workflow only asserts the digest is non-empty.
	demoDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	// refreshInterval is how often the property-graph traversal re-runs. It is
	// slower than the workflow's own 2s checkpoint delay so a person can watch
	// a run cross from ENQUEUED to PENDING to SUCCESS.
	refreshInterval = 1500 * time.Millisecond

	// listLimit bounds the workflow table.
	listLimit = 20
)

type app struct {
	ui      *ui
	dbosCtx dbos.Context
	pool    *pgxpool.Pool
	client  *client.Client
	backend *backend
	facts   runtimeFacts
}

func main() {
	u := newUI()
	a := &app{ui: u, client: client.New()}

	go func() {
		if err := a.boot(context.Background()); err != nil {
			u.fail(err)
		}
	}()

	// The page owns the lifetime, not this function. Blocking on a channel
	// nothing ever sends on keeps the Go runtime scheduling the goroutines that
	// are parked on JavaScript callbacks — the DBOS worker, the refresh loop,
	// and every click handler.
	select {}
}

func (a *app) boot(ctx context.Context) error {
	u := a.ui

	dataDir := queryParam("dataDir", defaultDataDir)

	// RouteInlineNotifications is settable from the URL so the browser test can
	// run the page both ways and compare. See probe.go for why the answer is
	// not known ahead of time.
	routeInline := queryParam("routeInline", "") != ""

	u.stage("loading PostgreSQL 19 (PGlite, PG19 fork)")
	pglite, err := createPGlite(ctx, dataDir)
	if err != nil {
		return fmt.Errorf("instantiate PGlite: %w", err)
	}

	be, err := newBackend(pglite)
	if err != nil {
		return err
	}
	a.backend = be

	u.stage("wiring the pgx transport over execProtocol")
	mux, err := wasmpg.New(wasmpg.Config{
		Exec:                     be.exec,
		LogicalConns:             logicalConns,
		RouteInlineNotifications: routeInline,
	})
	if err != nil {
		return fmt.Errorf("build the multiplexer: %w", err)
	}
	// The subscription lives as long as the page does; there is nothing to
	// release it on, because the tab closing releases everything.
	_ = be.subscribe(mux)

	pool, err := newPool(ctx, mux)
	if err != nil {
		return err
	}
	a.pool = pool

	a.facts = runtimeFacts{
		PGliteVersion: bridgeString("pgliteVersion"),
		ServerVersion: wasmpg.DefaultServerVersion,
		DataDir:       dataDir,
		LogicalConns:  mux.MaxConns(),
		PoolMaxConns:  pool.Config().MaxConns,
		UsableConns:   mux.MaxConns() - 1,
	}
	u.setRuntime(a.facts)

	u.stage("running dbos migrate through the shim")
	dbosCtx, err := dbos.NewContext(ctx, dbos.Config{
		AppName: appName,
		// SPEC.md §10.1: the application database and the DBOS system database
		// are the same database. In the browser they could not be anything
		// else — there is one PGlite backend — but the pool is passed
		// explicitly rather than through a URL because there is no URL: the
		// connection is a JavaScript function call.
		SystemDBPool:       pool,
		DatabaseSchema:     "dbos",
		ApplicationVersion: appVersion,
		ExecutorID:         executorID,
		// The admin server would want to listen on a TCP port. There are none.
		AdminServer: false,
		Logger:      slog.New(newSlogHandler(u, slog.LevelInfo)),
	})
	if err != nil {
		return fmt.Errorf("dbos migrate: %w", err)
	}
	a.dbosCtx = dbosCtx
	a.facts.Migrated = true

	u.stage("creating the agentiq schema and the property graph")
	name, err := a.migrate(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	a.facts.GraphMigration = name
	u.setRuntime(a.facts)

	// Before Launch, on purpose: DBOS's notification listener takes one of the
	// four logical connections and never gives it back (SPEC.md §12.4), and the
	// probe wants two.
	u.stage("probing notification delivery")
	u.setProbe(runNotifyProbe(ctx, pool, be, routeInline))

	u.stage("registering workflows")
	if err := workflow.Register(dbosCtx, workflow.Deps{}); err != nil {
		return fmt.Errorf("register workflows: %w", err)
	}

	u.stage("launching the DBOS queue worker")
	if err := dbos.Launch(dbosCtx); err != nil {
		return fmt.Errorf("launch: %w", err)
	}
	a.facts.Launched = true

	rows, err := a.list(ctx)
	if err != nil {
		return fmt.Errorf("list workflows: %w", err)
	}
	a.facts.Recovered = len(rows)
	u.setRuntime(a.facts)
	u.renderWorkflows(rows)
	if n := pending(rows); n > 0 {
		u.logf("%d workflow(s) left in flight by a previous tab; DBOS is recovering them", n)
	}

	go a.watchdog(ctx, mux)

	u.onClick("start", func() { a.startRun(ctx) })
	u.onClick("bad-sql", func() { a.runInvalidSQL(ctx) })
	u.onClick("reload", func() { js.Global().Get("location").Call("reload") })

	u.ready()
	go a.refresh(ctx)
	return nil
}

// migrate applies everything AgentIQ owns: the `agentiq` schema, the generated
// table history, and the generated property graph, in that order.
//
// The order and the re-application rules are `migrate`'s, not this file's, and
// deliberately so — the same sequence has to run in the integration harness and
// in `cmd/agentiq`, and a browser-only copy of it is how the page ends up being
// the only consumer that works. What is specific here is only the handle: the
// pool built over `wasmpg.Dialer`, because there is no connection string in a
// WASM page.
//
// It runs on every boot, including the reload failure-matrix row F22 is about.
// `migrate` is written for that: the schema and the tables are recorded in a
// ledger and skipped once applied, and the property graph — which holds no data
// and has no `CREATE ... IF NOT EXISTS` in SQL/PGQ — is dropped and recreated.
func (a *app) migrate(ctx context.Context) (string, error) {
	if err := migrate.Apply(ctx, a.pool); err != nil {
		return "", err
	}

	graph, err := migrate.Graph()
	if err != nil {
		return "", err
	}
	applied := make([]string, 0, len(graph))
	for _, m := range graph {
		applied = append(applied, m.Name)
	}
	return strings.Join(applied, ", "), nil
}

// startRun enqueues one workflow. Every externally triggered run goes through a
// DBOS queue — there is no unqueued execution path (SPEC.md §7.4) — and
// workflow.Enqueue is the in-process half of that, the SQL half being
// `Mutation.startAgentRun` (§7.4) which needs an API server M1 does not have.
func (a *app) startRun(ctx context.Context) {
	a.ui.enable("start", false)
	defer a.ui.enable("start", true)

	in := workflow.AgentRunInput{
		AgentDigest: demoDigest,
		AppName:     appName,
		UserID:      "browser",
		Message:     json.RawMessage(`{"text":"hello from the tab"}`),
	}

	h, err := workflow.Enqueue(a.dbosCtx, in)
	if err != nil {
		a.ui.logf("enqueue failed: %v", err)
		return
	}
	a.ui.logf("enqueued %s", h.GetWorkflowID())

	// Read the status straight back. It is the shortest path from "the enqueue
	// returned an ID" to "the row is committed and visible", and the two are
	// not the same claim on a backend where every logical connection shares one
	// session.
	if st, err := h.GetStatus(); err != nil {
		a.ui.logf("status of %s is unreadable: %v", shortID(h.GetWorkflowID()), err)
	} else {
		a.ui.logf("status of %s is %s on queue %q", shortID(h.GetWorkflowID()), st.Status, st.QueueName)
	}

	// The table is not refreshed here. The refresh loop picks the new run up
	// within one interval, and a query issued from a click handler is one more
	// connection competing with the queue runner for the same backend at the
	// exact moment the queue runner has work to do.
}

// list is the M1 acceptance read (SPEC.md §16): every workflow, each with the
// steps the property graph says it has.
//
// The two halves come from two places on purpose. The workflow *list* is
// DBOS's own client API — the durable-execution runtime's view of its own
// state, and the only thing that can report a workflow that has not run a step
// yet. The *steps* come from the generated `GRAPH_TABLE` traversal over
// `dbos.*`, which is the read model SPEC.md §7.3 defines and the thing M1 has
// to prove works in the browser. Neither is hand-written SQL.
func (a *app) list(ctx context.Context) ([]workflowRow, error) {
	statuses, err := dbos.ListWorkflows(a.dbosCtx,
		dbos.WithFilterLimit(listLimit),
		dbos.WithFilterSortDesc(),
	)
	if err != nil {
		return nil, err
	}

	rows := make([]workflowRow, 0, len(statuses))
	for _, s := range statuses {
		row := workflowRow{
			ID:      s.ID,
			Status:  string(s.Status),
			Name:    shortName(s.Name),
			Queue:   s.QueueName,
			Created: s.CreatedAt.Format(time.RFC3339),
		}

		// The traversal is an inner join over HAS_STEP, so a workflow that has
		// not checkpointed a step yet is simply absent from it. That is not an
		// error: it is what "no steps yet" looks like in a graph.
		//
		// A traversal that *fails* is recorded on the row rather than failing
		// the whole list. The two reads answer different questions — DBOS's
		// list is "what runs exist", the traversal is "what does the property
		// graph say about this one" — and losing the second is no reason to
		// stop showing the first. It also stops a failing traversal from
		// turning the refresh loop into a hot loop that starves the queue
		// worker of connections, which is exactly what it did.
		found, err := a.client.WorkflowWithSteps(ctx, exec.Pgx(a.pool), client.WorkflowWithStepsInput{WorkflowUuid: s.ID})
		if err != nil {
			row.StepsError = err.Error()
			rows = append(rows, row)
			continue
		}
		for _, w := range found {
			for _, st := range w.Steps {
				row.Steps = append(row.Steps, stepRow{
					FunctionID:   int32(st.FunctionId),
					FunctionName: deref(st.FunctionName),
					Error:        deref(st.Error),
				})
			}
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// refresh re-reads the graph on a timer. Polling rather than LISTEN is
// deliberate: this is the page's own view, and making it depend on notification
// delivery would hide exactly the failure the probe exists to detect.
func (a *app) refresh(ctx context.Context) {
	ticker := time.NewTicker(refreshInterval)
	defer ticker.Stop()

	var last string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rows, err := a.list(ctx)
			if err != nil {
				a.ui.logf("refresh failed: %v", err)
				continue
			}
			// Log only transitions. A line per poll would bury everything else
			// in a page that polls for as long as the tab is open.
			if s := summarise(rows); s != last {
				a.ui.logf("graph: %s", s)
				last = s
			}
			a.ui.renderWorkflows(rows)
			a.ui.setPGlite(a.backend.observe())
		}
	}
}

// invalidStatement is deliberately invalid SQL, and it is the whole of
// failure-matrix row F20: "invalid SQL in the browser test surfaces as a pgx
// error through the shim". The row is about the *transport* — that an
// ErrorResponse frame produced by PGlite is framed, read back through
// wasmpg.Conn and decoded by pgx into a *pgconn.PgError with its SQLSTATE
// intact, rather than being lost or presenting as a torn connection.
//
// It cannot come from the generated client: gopgql compiles statements from the
// SDL and will not emit one that references a table that does not exist. That
// is why .golangci.yml exempts this package from rawsql, and why the exemption
// names this statement.
const invalidStatement = "SELECT * FROM a_relation_that_does_not_exist"

// runInvalidSQL drives F20 from the page. The error it produces is reported,
// not raised: a demonstration that errors survive the shim is only a
// demonstration if the page stays up afterwards.
func (a *app) runInvalidSQL(ctx context.Context) {
	_, err := a.pool.Exec(ctx, invalidStatement)
	if err == nil {
		// A statement that cannot succeed just did. That is a finding.
		a.ui.setShimError(shimError{Statement: invalidStatement, Message: "the invalid statement unexpectedly succeeded"})
		return
	}

	out := shimError{Statement: invalidStatement, Message: err.Error()}

	// The SQLSTATE is the point. An error that arrives as a plain string has
	// lost the frame; one that arrives as a *pgconn.PgError carrying 42P01 was
	// decoded from a real ErrorResponse.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		out.SQLState = pgErr.Code
		out.Message = pgErr.Message
		out.IsPgError = true
	}
	a.ui.setShimError(out)
}

// watchdog reports the transport's own counters on a timer.
//
// It reads nothing from the database — only in-memory counters on the pool, the
// multiplexer and the execProtocol tap — so it is the one loop that cannot
// itself be blocked by whatever it is reporting on. That is the whole point:
// when the page stops making progress, the question is *where*, and a loop that
// needs a connection to answer it cannot answer it.
//
// It logs on change only. Idle, it says nothing.
func (a *app) watchdog(ctx context.Context, mux *wasmpg.Multiplexer) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var last string
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s := a.pool.Stat()
			obs := a.backend.observe()
			line := fmt.Sprintf(
				"pool acquired=%d idle=%d total=%d max=%d waits=%d | mux conns=%d/%d | execProtocol calls=%d",
				s.AcquiredConns(), s.IdleConns(), s.TotalConns(), s.MaxConns(),
				s.EmptyAcquireCount(), mux.Conns(), mux.MaxConns(), obs.RoundTrips,
			)
			if line != last {
				a.ui.logf("%s", line)
				last = line
			}
		}
	}
}

// summarise renders the table as one line, so the log can record a transition
// without repeating the whole state on every poll.
func summarise(rows []workflowRow) string {
	if len(rows) == 0 {
		return "no workflows"
	}
	parts := make([]string, 0, len(rows))
	for _, r := range rows {
		parts = append(parts, fmt.Sprintf("%s=%s(%d steps)", shortID(r.ID), r.Status, len(r.Steps)))
	}
	return strings.Join(parts, " ")
}

func pending(rows []workflowRow) int {
	n := 0
	for _, r := range rows {
		switch r.Status {
		case string(dbos.WorkflowStatusPending), string(dbos.WorkflowStatusEnqueued):
			n++
		}
	}
	return n
}

// shortName trims the fully-qualified workflow name DBOS records
// (`github.com/gaarutyunov/agentiq/workflow.AgentRun`) down to the part that
// identifies it. The full value stays in the JSON state.
func shortName(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// queryParam reads a URL search parameter, so the page can be pointed at a
// different IndexedDB database or run with inline notification routing on
// without a rebuild. Both are things the browser test needs to vary.
func queryParam(name, fallback string) string {
	params := js.Global().Get("URLSearchParams").New(js.Global().Get("location").Get("search"))
	v := params.Call("get", name)
	if v.Type() != js.TypeString || v.String() == "" {
		return fallback
	}
	return v.String()
}

// bridgeString reads a string the boot shim published, or "" if it did not.
func bridgeString(name string) string {
	b, err := bridge()
	if err != nil {
		return ""
	}
	if v := b.Get(name); v.Type() == js.TypeString {
		return v.String()
	}
	return ""
}
