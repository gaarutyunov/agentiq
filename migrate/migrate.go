// Package migrate applies every migration AgentIQ owns, in the one order they
// can run in, and is the only place that order is written down.
//
// # Why this package exists at all
//
// gopgql deliberately emits no `CREATE SCHEMA` — `sdl/readonly.go` puts it as
// "a schema it does not own is not its to create" — so `CREATE TABLE
// agentiq.session` fails against a fresh database with SQLSTATE 3F000, and so
// does `generated/graph/0003`'s property graph, which names `agentiq.*` tables.
// DBOS does not create it either: `dbos.*` is DBOS's and nothing else
// (SPEC.md §2.2). The schema is AgentIQ's, and creating it is therefore
// AgentIQ's job.
//
// # Why one package rather than a line in each caller
//
// Three programs migrate the same database, and each of them is the only one
// that exercises some part of the sequence:
//
//   - `test/harness` — the integration suite, against a real `postgres:19beta2`
//     container.
//   - `cmd/agentiq` — the server.
//   - `demo/wasm` — the browser demo, which runs `dbos migrate` through the
//     wasmpg shim against PGlite. It is the one a person actually looks at, and
//     the one most easily forgotten, because it is the only consumer that
//     cannot run a migration CLI: there is no goose and no `database/sql`
//     driver in a WASM page, so the statements have to be library calls.
//
// Before this package the ordering lived in two of those three and disagreed:
// `cmd/agentiq` applied no property graph at all, and only the browser hid it.
// A fix written into one caller would have left the other two broken in exactly
// the same way, which is why the ordering is stated once, here, and the callers
// only supply a handle to run it against.
//
// # The order, and why it is that order
//
//  1. `sql/` — AgentIQ's own hand-written history. `0001` creates the schema
//     and the ledger below. This is the only hand-written SQL in the
//     repository outside the test fixtures, and it is hand-written because
//     gopgql refuses to generate it by design (above), not because it was
//     easier.
//  2. `tables/` — a copy of `generated/migrations/`, gopgql's `CREATE TABLE`
//     history for the seven `agentiq.*` types of SPEC.md §7.1. It needs the
//     schema from step 1.
//  3. `graph/` — a copy of `generated/graph/`, the `CREATE PROPERTY GRAPH`
//     spanning `dbos.*` and `agentiq.*`. It needs both the `dbos.*` tables
//     (created by `dbos migrate`, which every caller runs before this package)
//     and the `agentiq.*` tables from step 2. Applying it before them fails
//     with "schema \"agentiq\" does not exist", and then, once the schema is
//     there but the tables are not, with "relation \"agentiq.actions\" does not
//     exist" — both verified on a live postgres:19beta2.
//
// # Re-application
//
// Two of the three callers re-run the whole sequence routinely: the browser
// re-migrates on every tab reload (which is what failure-matrix row F22 is
// about) and the server on every restart. Neither has goose, so nothing else
// records what has already run.
//
// Steps 1 and 2 are therefore recorded in `agentiq.schema_migrations` and
// skipped when already applied. `sql/0001` is the exception: it is what creates
// that ledger, so it cannot consult it, and it is written to be idempotent by
// construction (`IF NOT EXISTS` on both statements) instead.
//
// Step 3 is applied Down-then-Up unconditionally and is not recorded. A
// property graph is a declaration over tables and holds no data of its own, so
// recreating it costs nothing — and SQL/PGQ has no `CREATE ... IF NOT EXISTS`
// for one, so a bare `CREATE` against a graph that survived in IndexedDB would
// fail on the second page load.
//
// # Concurrency
//
// Nothing here takes a lock. Every caller today migrates from a single process
// against a database it is the only writer of — one container per suite, one
// tab, one worker. Two servers racing to create the same table would surface as
// a duplicate-object error from one of them rather than as corruption, and
// giving them an advisory lock is a change to make when there is a second
// server, not before.
package migrate

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// The three histories. `sql/` is checked in by hand; `tables/` and `graph/` are
// copies written by `go generate ./...` (see tools/generate.go), because
// //go:embed cannot reference a parent directory and this package does not sit
// above `generated/`. The copies are generated artifacts, so SPEC.md §17.3's
// `go generate ./... && git diff --exit-code` gate fails the moment they drift
// from what gopgql wrote.
//
// Whole directories rather than `*.sql` patterns: `tables/` legitimately holds
// no `.sql` at all right now (see its README), and //go:embed fails to compile
// on a pattern that matches nothing.
//
//go:embed sql tables graph
var migrationsFS embed.FS

const (
	ownDir    = "sql"
	tablesDir = "tables"
	graphDir  = "graph"

	// bootstrapName is the migration that creates the schema and the ledger. It
	// is applied unconditionally, so it is the one file in `sql/` that must
	// stay idempotent on its own.
	bootstrapName = ownDir + "/0001_agentiq_schema.sql"
)

// Goose direction markers. All three histories are goose-annotated —
// `generated/graph/` and `generated/migrations/` because gopgql writes them
// that way, and `sql/` to match — even though no caller here runs goose.
const (
	gooseUp   = "-- +goose Up"
	gooseDown = "-- +goose Down"
)

// Migration is one migration, split into the two directions goose annotates.
type Migration struct {
	// Name is the history it came from and its file name, e.g.
	// "sql/0001_agentiq_schema.sql". The prefix is load-bearing: it is what
	// goes into the ledger, and the three histories number from 0001
	// independently.
	Name string

	// Up and Down are whole statements, ready to Exec. The goose annotations
	// are stripped: there is nothing here to strip them later.
	Up, Down string
}

// Own returns AgentIQ's own hand-written history.
func Own() ([]Migration, error) { return read(ownDir) }

// Tables returns gopgql's `CREATE TABLE` history for the `agentiq.*` types.
//
// It is legitimately empty today. gopgql v0.2.2 emits no table DDL for this
// schema at all and exits 0 while doing it — gaarutyunov/gopgql#53 — so
// `generated/migrations/` has nothing to copy. See
// generated/migrations/README.md for the mechanism. Nothing here changes when
// that is fixed: `go generate ./...` fills the directory and [Apply] picks the
// files up.
func Tables() ([]Migration, error) { return read(tablesDir) }

// Graph returns the generated property-graph history.
func Graph() ([]Migration, error) { return read(graphDir) }

// read loads one history, in the order goose would apply it.
func read(dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(migrationsFS, dir)
	if err != nil {
		return nil, fmt.Errorf("migrate: read %s: %w", dir, err)
	}

	out := make([]Migration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := fs.ReadFile(migrationsFS, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s/%s: %w", dir, e.Name(), err)
		}
		m, err := splitGoose(dir+"/"+e.Name(), string(body))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// splitGoose cuts one annotated file into its two directions.
//
// It is deliberately strict. A file whose markers moved would otherwise yield
// an empty Up, and an empty Up is a program that boots, reports success and
// then fails every query against a schema that was never created — a much
// longer walk back to the cause than a startup error naming the file.
func splitGoose(name, body string) (Migration, error) {
	up := strings.Index(body, gooseUp)
	if up < 0 {
		return Migration{}, fmt.Errorf("migrate: %s: no %q marker", name, gooseUp)
	}
	down := strings.Index(body, gooseDown)
	if down < 0 {
		return Migration{}, fmt.Errorf("migrate: %s: no %q marker", name, gooseDown)
	}
	if down < up {
		return Migration{}, fmt.Errorf("migrate: %s: %q precedes %q", name, gooseDown, gooseUp)
	}

	m := Migration{
		Name: name,
		Up:   strings.TrimSpace(body[up+len(gooseUp) : down]),
		Down: strings.TrimSpace(body[down+len(gooseDown):]),
	}
	if m.Up == "" {
		return Migration{}, fmt.Errorf("migrate: %s: nothing between %q and %q", name, gooseUp, gooseDown)
	}
	if m.Down == "" {
		return Migration{}, fmt.Errorf("migrate: %s: nothing after %q", name, gooseDown)
	}
	return m, nil
}

// DB is what [Apply] needs from a database handle.
//
// It is an interface so the browser can pass the `pgxpool` it built over
// `wasmpg.Dialer` and the integration suite the one it built over a TCP socket,
// without this package caring which. `*pgxpool.Pool` satisfies it as written;
// so does `*pgx.Conn`.
//
// gopgql's own constructor is not an option: `exec.OpenReadOnly` sets
// `default_transaction_read_only=on` by design, and DDL through it fails with
// SQLSTATE 25006.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// The ledger statements. These are the only SQL string literals in the package
// — everything else is an embedded file — and they cannot come from the
// generated client, because the ledger is not in the SDL and must not be: it is
// bookkeeping about migrations, not part of the domain model the property graph
// projects.
const (
	ledgerRead   = `SELECT version FROM agentiq.schema_migrations`
	ledgerRecord = `INSERT INTO agentiq.schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`
)

// Apply runs every migration AgentIQ owns against db, in the order the package
// documentation sets out.
//
// It must run after `dbos migrate` — the property graph in step 3 projects
// `dbos.*`, and applying it first fails with "relation does not exist".
func Apply(ctx context.Context, db DB) error {
	own, err := Own()
	if err != nil {
		return err
	}
	if len(own) == 0 || own[0].Name != bootstrapName {
		return fmt.Errorf("migrate: %s must be the first migration in %s/; got %v",
			bootstrapName, ownDir, names(own))
	}

	// The bootstrap first and unconditionally: it is what creates the ledger
	// the rest are recorded in.
	if _, err := db.Exec(ctx, own[0].Up); err != nil {
		return fmt.Errorf("migrate: %s: %w", own[0].Name, err)
	}

	tables, err := Tables()
	if err != nil {
		return err
	}
	recorded := append(append([]Migration{}, own[1:]...), tables...)

	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}
	for _, m := range recorded {
		if applied[m.Name] {
			continue
		}
		if _, err := db.Exec(ctx, m.Up); err != nil {
			return fmt.Errorf("migrate: %s: %w", m.Name, err)
		}
		if _, err := db.Exec(ctx, ledgerRecord, m.Name); err != nil {
			return fmt.Errorf("migrate: record %s: %w", m.Name, err)
		}
	}

	return ApplyGraph(ctx, db)
}

// ApplyGraph drops and recreates the generated property graph, and nothing
// else.
//
// Down-then-Up, every time, for the reasons the package documentation gives:
// the graph holds no data, and SQL/PGQ has no `CREATE ... IF NOT EXISTS` for
// one. It is exported separately because the graph is the only part of the
// sequence that is safe to reapply on its own.
func ApplyGraph(ctx context.Context, db DB) error {
	graph, err := Graph()
	if err != nil {
		return err
	}
	if len(graph) == 0 {
		return fmt.Errorf("migrate: no migrations in %s/; run `go generate ./...`", graphDir)
	}
	for i := len(graph) - 1; i >= 0; i-- {
		if _, err := db.Exec(ctx, graph[i].Down); err != nil {
			return fmt.Errorf("migrate: %s down: %w", graph[i].Name, err)
		}
	}
	for _, m := range graph {
		if _, err := db.Exec(ctx, m.Up); err != nil {
			return fmt.Errorf("migrate: %s up: %w", m.Name, err)
		}
	}
	return nil
}

// appliedVersions reads the ledger.
func appliedVersions(ctx context.Context, db DB) (map[string]bool, error) {
	rows, err := db.Query(ctx, ledgerRead)
	if err != nil {
		return nil, fmt.Errorf("migrate: read the migration ledger: %w", err)
	}
	defer rows.Close()

	applied := map[string]bool{}
	for rows.Next() {
		var version string
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("migrate: read the migration ledger: %w", err)
		}
		applied[version] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("migrate: read the migration ledger: %w", err)
	}
	return applied, nil
}

// names renders a history for an error message.
func names(ms []Migration) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Name)
	}
	return out
}
