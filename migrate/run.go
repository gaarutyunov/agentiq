package migrate

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Run opens a pool against databaseURL, applies everything, and closes it.
//
// It is the entry point for a caller that has a connection string and no pool
// of its own — `cmd/agentiq`, which hands DBOS a URL rather than a handle. The
// pool lives only for the migration: a server that kept it would be holding a
// second pool for the lifetime of the process to run three statements at
// startup.
//
// The browser does not use it and cannot: there is no connection string in a
// WASM page — the connection is a JavaScript function call — so `demo/wasm`
// passes the pool it already built over `wasmpg.Dialer` to [Apply] directly.
// It carries no `!js` build constraint even so, because `cmd/agentiq` has no
// build constraint either and `make lint-wasm` typechecks every package in the
// module under `GOOS=js GOARCH=wasm`; a constraint here would make the server
// fail to typecheck there for a function no browser calls.
func Run(ctx context.Context, databaseURL string) error {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("migrate: open a pool: %w", err)
	}
	defer pool.Close()

	return Apply(ctx, pool)
}
