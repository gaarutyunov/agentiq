package harness

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gaarutyunov/agentiq/generated/client"
	"github.com/gaarutyunov/agentiq/migrate"
)

// OpenPool opens a writable pool and registers its close.
//
// gopgql's own constructor, exec.OpenReadOnly, sets
// `default_transaction_read_only=on` by design (a `@function` mutation
// attempted through it fails with SQLSTATE 25006). The property-graph DDL has
// to be executed by something, and this is it.
func OpenPool(ctx context.Context, tb testingTB, dsn string) *pgxpool.Pool {
	tb.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		tb.Fatalf("harness: open pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		tb.Fatalf("harness: ping: %v", err)
	}
	tb.Cleanup(pool.Close)
	return pool
}

// ApplyMigrations applies everything AgentIQ owns: the `agentiq` schema, the
// generated `agentiq.*` table history, and the generated property graph.
//
// The sequence itself belongs to `migrate`, which is also what `cmd/agentiq`
// and the browser demo run. A suite that applied its own version of it would be
// asserting on an ordering the shipped programs do not use — and the previous
// version of this function, which applied the property graph and nothing else,
// is exactly how the missing `CREATE SCHEMA agentiq` stayed invisible until the
// graph grew its first `agentiq.*` vertex.
//
// It runs only after `dbos.*` exists — the property graph projects those
// tables, so applying it before the worker has migrated fails with "relation
// does not exist".
func ApplyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	if err := migrate.Apply(ctx, pool); err != nil {
		return fmt.Errorf("harness: %w", err)
	}
	return nil
}

// GraphWorkflow runs the generated GRAPH_TABLE traversal for one workflow.
//
// The statement is gopgql's, compiled from
// schema/operations/workflow_with_steps.graphql. SPEC.md §21 forbids
// hand-written SQL, and a suite that hand-wrote this query would be asserting
// on a query M1 does not ship.
func GraphWorkflow(ctx context.Context, pool *pgxpool.Pool, id string) ([]client.WorkflowWithStepsWorkflow, error) {
	rows, err := client.New().WorkflowWithSteps(ctx, pool, client.WorkflowWithStepsInput{WorkflowUuid: id})
	if err != nil {
		return nil, fmt.Errorf("harness: graph query: %w", err)
	}
	return rows, nil
}

// SeedRecoveryAttempts sets a workflow's `recovery_attempts` counter.
//
// This is the compression failure-matrix row F5 needs. DBOS dead-letters a
// workflow when `recovery_attempts` exceeds the registered maximum plus one
// (dbos/internal/sysdb/system_database.go), and the registered maximum is the
// default 100. Driving that with 100 real worker restarts would cost more than
// the whole 20-minute integration budget and would exercise the same single
// comparison the 101st restart exercises. §19 F13 compresses a 168-hour
// timeout for exactly this reason.
//
// It writes to a DBOS-owned table, which nothing else in this repository does
// and nothing else should: `dbos.*` is DBOS's (SPEC.md §2.2). It is confined to
// this one function so the rule is broken in a place that says why.
func SeedRecoveryAttempts(ctx context.Context, pool *pgxpool.Pool, id string, attempts int) error {
	tag, err := pool.Exec(ctx,
		`UPDATE dbos.workflow_status SET recovery_attempts = $1 WHERE workflow_uuid = $2`,
		attempts, id)
	if err != nil {
		return fmt.Errorf("harness: seed recovery_attempts: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("harness: seed recovery_attempts: %d rows affected, want 1", tag.RowsAffected())
	}
	return nil
}

// DefaultMaxRecoveryAttempts mirrors DBOS's `_DEFAULT_MAX_RECOVERY_ATTEMPTS`.
//
// It is not exported by DBOS, so it is restated rather than referenced. If a
// DBOS upgrade changes it, F5 fails as a timeout waiting for
// MAX_RECOVERY_ATTEMPTS_EXCEEDED — loudly, and pointing here.
const DefaultMaxRecoveryAttempts = 100
