package migrate

import (
	"context"
	"fmt"

	"github.com/dbos-inc/dbos-transact-golang/dbos"
	"github.com/gaarutyunov/gopgql/exec"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Runtime is the pair of handles a server needs after migrating: the DBOS
// DataSource transactions are opened on, and a gopgql handle for the reads that
// are not transactions.
//
// It lives here because this is the package that already holds the pgx
// exemption, and holding it in exactly one place is the point of the exemption.
// The alternative — `cmd/agentiq` building its own `pgxpool` — is a pgx import
// in the one package SPEC.md §21 least wants one, and .golangci.yml's
// `no-sql-outside-generated` rule says so where it lists the five exclusions.
//
// Nothing pgx-shaped crosses this boundary. A caller gets a `*dbos.DataSource`
// and an `exec.Handle`, which is exactly what `workflow.Deps` and `session.New`
// take, and never learns which driver is underneath.
type Runtime struct {
	// DataSource is what `dbos.RunAsTransaction` opens transactions on, so that
	// an application write and its step checkpoint commit together (D2).
	DataSource *dbos.DataSource

	// Handle serves the reads: sessions, events, agent rows. A read joins no
	// transaction, and DBOS hands out transactions rather than connections.
	Handle exec.Handle

	// Close releases the pool. A server holds the Runtime for its lifetime —
	// unlike [Run]'s pool, which exists only for the migration.
	Close func()
}

// Open migrates the database and returns the handles a server runs on.
//
// The order is the one migrate's package documentation sets out and it is not
// interchangeable: `dbos.NewContext` (which runs `dbos migrate`) has to have
// happened before this, because the property graph applied here projects
// `dbos.*`; and `dbos.Launch` has to happen after, because a recovered workflow
// appends into tables this creates.
//
// `dbos.NewDataSource` is called after [Apply] for the same class of reason: it
// creates its own `transaction_completion` table, and doing that against a
// half-migrated database is a state nothing needs to be able to reason about.
func Open(ctx context.Context, dctx dbos.Context, databaseURL string) (*Runtime, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("migrate: open a pool: %w", err)
	}

	if err := Apply(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}

	ds, err := dbos.NewDataSource(dctx, pool)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: data source: %w", err)
	}

	return &Runtime{
		DataSource: ds,
		Handle:     exec.Pgx(pool),
		Close:      pool.Close,
	}, nil
}
