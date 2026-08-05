package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/gaarutyunov/agentiq/generated/client"
)

// Goose direction markers. `generated/graph/` is a goose history
// (generated/graph/README.md).
const (
	gooseUp   = "-- +goose Up"
	gooseDown = "-- +goose Down"
)

// GraphMigration is one generated property-graph migration.
type GraphMigration struct {
	Name string
	Up   string
	Down string
}

// GeneratedGraphMigrations reads `generated/graph/*.sql` from the checkout.
//
// It reads from disk rather than embedding because //go:embed cannot reference
// a parent directory and nothing under `test/` sits above `generated/`. The
// alternative — importing the `demo` package, which solves the same problem
// with a generated copy — would tie the server-side suite to the browser demo's
// packaging decisions for no gain.
func GeneratedGraphMigrations() ([]GraphMigration, error) {
	root, err := moduleRoot()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(root, "generated", "graph")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("harness: read %s: %w", dir, err)
	}

	var out []GraphMigration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // a path this package built from its own source location
		if err != nil {
			return nil, fmt.Errorf("harness: read %s: %w", e.Name(), err)
		}
		m, err := splitGoose(e.Name(), string(body))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("harness: no migrations in %s; run `go generate ./...`", dir)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func splitGoose(name, body string) (GraphMigration, error) {
	up := strings.Index(body, gooseUp)
	down := strings.Index(body, gooseDown)
	if up < 0 || down < 0 || down < up {
		return GraphMigration{}, fmt.Errorf("harness: %s: goose markers missing or out of order", name)
	}
	return GraphMigration{
		Name: name,
		Up:   strings.TrimSpace(body[up+len(gooseUp) : down]),
		Down: strings.TrimSpace(body[down+len(gooseDown):]),
	}, nil
}

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

// ApplyGraph drops and recreates the generated property graph.
//
// Down-then-Up rather than Up alone: a property graph is a view over `dbos.*`
// and holds no data of its own, and CREATE PROPERTY GRAPH on one that already
// exists is a duplicate-object error. Applying it twice has to be harmless,
// because the browser build applies it on every boot (SPEC.md §13, F22).
//
// It runs only after `dbos.*` exists — the graph references those tables, so
// applying it before the worker has migrated fails with "relation does not
// exist".
func ApplyGraph(ctx context.Context, pool *pgxpool.Pool) error {
	migrations, err := GeneratedGraphMigrations()
	if err != nil {
		return err
	}
	for i := len(migrations) - 1; i >= 0; i-- {
		if _, err := pool.Exec(ctx, migrations[i].Down); err != nil {
			return fmt.Errorf("harness: %s down: %w", migrations[i].Name, err)
		}
	}
	for _, m := range migrations {
		if _, err := pool.Exec(ctx, m.Up); err != nil {
			return fmt.Errorf("harness: %s up: %w", m.Name, err)
		}
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
