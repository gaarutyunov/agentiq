package harness

import (
	"database/sql"
	"fmt"
	"slices"
	"strings"

	// Registers the "pgx" database/sql driver.
	//
	// This blank import is load-bearing, not decorative. See
	// [RequireSQLDriver] for what its absence costs.
	_ "github.com/jackc/pgx/v5/stdlib"
)

// SQLDriverName is the driver testcontainers is told to use, and therefore the
// driver that must be registered with database/sql.
const SQLDriverName = "pgx"

// ErrDriverNotRegistered is returned by [CheckSQLDriver] when the declared
// driver is not registered.
type ErrDriverNotRegistered struct {
	Name       string
	Registered []string
}

func (e *ErrDriverNotRegistered) Error() string {
	registered := "none"
	if len(e.Registered) > 0 {
		registered = strings.Join(e.Registered, ", ")
	}
	return fmt.Sprintf(
		"harness: SQL driver %q is not registered with database/sql (registered: %s).\n"+
			"postgres.WithSQLDriver(%q) does NOT fail when the driver is missing: testcontainers "+
			"silently falls back to running `docker exec psql`. psql does not stop on error, so a "+
			"failed DROP DATABASE is swallowed and the CREATE that follows reports "+
			"`database \"...\" already exists` — an error that names the wrong problem entirely.\n"+
			"Fix: blank-import the driver, e.g. _ \"github.com/jackc/pgx/v5/stdlib\".",
		e.Name, registered, e.Name)
}

// CheckSQLDriver reports whether name is registered with database/sql.
//
// It is separate from [RequireSQLDriver] so the guard can be asserted on in a
// unit test without a testing.TB that fails the suite.
func CheckSQLDriver(name string) error {
	drivers := sql.Drivers()
	if slices.Contains(drivers, name) {
		return nil
	}
	return &ErrDriverNotRegistered{Name: name, Registered: drivers}
}

// RequireSQLDriver fails the suite immediately if the driver testcontainers has
// been told to use is not registered.
//
// Every fixture that starts a container calls this before the container starts,
// so the failure names the missing import rather than surfacing six steps later
// as a nonsensical database error.
func RequireSQLDriver(tb testingTB, name string) {
	tb.Helper()
	if err := CheckSQLDriver(name); err != nil {
		tb.Fatalf("%v", err)
	}
}

// testingTB is the slice of testing.TB the harness uses. It is an interface of
// its own so godog step definitions, which hold a testing.TB obtained from
// godog's TestingT, can pass it through without the package importing
// "testing" into non-test code.
type testingTB interface {
	Helper()
	Cleanup(func())
	Logf(format string, args ...any)
	Fatalf(format string, args ...any)
}
