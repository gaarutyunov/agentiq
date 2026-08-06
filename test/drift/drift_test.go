//go:build integration

package drift

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/gaarutyunov/agentiq/test/harness"
)

// updateFixture rewrites testdata/dbos_schema.json from the live container.
//
//	go test ./test/drift -tags=integration -args -update-dbos-fixture
//
// It is a flag rather than an environment variable so that running it is a
// deliberate act recorded in shell history. Regenerating the fixture is how a
// DBOS upgrade is accepted, and it must be a decision, not a reflex: the diff
// it produces is the list of columns `schema/dbos.graphql` now has to account
// for.
var updateFixture = flag.Bool("update-dbos-fixture", false,
	"rewrite testdata/dbos_schema.json from the running container")

const (
	dbosModule  = "github.com/dbos-inc/dbos-transact-golang"
	fixturePath = "testdata/dbos_schema.json"
)

// column is one column of one `dbos.*` table.
//
// The data type is compared as well as the name. A retype — `text` to `jsonb`,
// `integer` to `bigint` — changes how the generated read model decodes the
// value, and a check that only counted names would pass while the projection
// silently started returning the wrong thing.
type column struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// schemaFixture is the checked-in shape of `dbos.*`.
type schemaFixture struct {
	// DBOSVersion is the version the fixture was cut against. It is what the
	// failure message names (SPEC.md §17.4).
	DBOSVersion string `json:"dbosVersion"`

	// Tables maps table name to its columns, sorted by name.
	Tables map[string][]column `json:"tables"`
}

// TestDBOSSchemaDrift is SPEC.md §17.4.
func TestDBOSSchemaDrift(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)

	pinned := pinnedDBOSVersion(t)

	pg := harness.StartPostgres(ctx, t)

	// Opening a DBOS client runs the system-database migrations, which is what
	// creates `dbos.*`. Nothing else in this test needs DBOS: the subject is
	// the schema those migrations produced, not anything DBOS does with it.
	client := harness.NewDBOSClient(ctx, t, pg.DSN)
	require.NoError(t, client.Shutdown(client, 30*time.Second))

	pool := harness.OpenPool(ctx, t, pg.DSN)
	live, err := introspect(ctx, pool)
	require.NoError(t, err, "introspect dbos.*")
	require.NotEmpty(t, live, "dbos.* has no tables; the migrations did not run")

	if *updateFixture {
		require.NoError(t, writeFixture(schemaFixture{DBOSVersion: pinned, Tables: live}))
		t.Logf("wrote %s for DBOS %s", fixturePath, pinned)
		return
	}

	want, err := readFixture()
	require.NoError(t, err,
		"read %s — cut it with `go test ./test/drift -tags=integration -args -update-dbos-fixture`",
		fixturePath)

	assert.Equal(t, want.DBOSVersion, pinned,
		"the fixture was cut against DBOS %s but this build pins %s; re-cut it and review what changed in schema/dbos.graphql",
		want.DBOSVersion, pinned)

	assertNoDrift(t, pinned, want.Tables, live)
}

// assertNoDrift reports every difference, not the first.
//
// A DBOS upgrade typically adds several columns at once; reporting one at a
// time turns a single review into several CI rounds.
func assertNoDrift(t *testing.T, pinned string, want, live map[string][]column) {
	t.Helper()

	const remedy = "\nDBOS %s changed `dbos.*`. Update schema/dbos.graphql (SPEC.md §7.3) to account for it, " +
		"then re-cut the fixture with `go test ./test/drift -tags=integration -args -update-dbos-fixture`."

	for name := range want {
		if _, ok := live[name]; !ok {
			t.Errorf("table dbos.%s is in the fixture and not in the database."+remedy, name, pinned)
		}
	}
	for name := range live {
		if _, ok := want[name]; !ok {
			t.Errorf("table dbos.%s exists in the database and not in the fixture."+remedy, name, pinned)
		}
	}

	for name, wantCols := range want {
		liveCols, ok := live[name]
		if !ok {
			continue
		}
		added, removed, retyped := diff(wantCols, liveCols)
		if len(added) > 0 {
			t.Errorf("dbos.%s gained columns %s."+remedy, name, strings.Join(added, ", "), pinned)
		}
		if len(removed) > 0 {
			t.Errorf("dbos.%s lost columns %s."+remedy, name, strings.Join(removed, ", "), pinned)
		}
		if len(retyped) > 0 {
			t.Errorf("dbos.%s retyped columns %s."+remedy, name, strings.Join(retyped, ", "), pinned)
		}
	}
}

// diff compares two sorted column lists.
func diff(want, live []column) (added, removed, retyped []string) {
	wantByName := make(map[string]string, len(want))
	for _, c := range want {
		wantByName[c.Name] = c.Type
	}
	liveByName := make(map[string]string, len(live))
	for _, c := range live {
		liveByName[c.Name] = c.Type
	}

	for _, c := range live {
		wantType, ok := wantByName[c.Name]
		switch {
		case !ok:
			added = append(added, c.Name+" "+c.Type)
		case wantType != c.Type:
			retyped = append(retyped, c.Name+" "+wantType+" -> "+c.Type)
		}
	}
	for _, c := range want {
		if _, ok := liveByName[c.Name]; !ok {
			removed = append(removed, c.Name+" "+c.Type)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(retyped)
	return added, removed, retyped
}

// introspect reads the live column set of the `dbos` schema.
//
// This is the one query in the repository that cannot come from the generated
// client (SPEC.md §21, and the `test/drift` exemption in .golangci.yml): the
// point is to see what the database actually has, independently of what was
// generated from the SDL. A generated read model asked the same question would
// answer from the SDL and always agree with itself.
func introspect(ctx context.Context, pool *pgxpool.Pool) (map[string][]column, error) {
	rows, err := pool.Query(ctx, introspectSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]column{}
	for rows.Next() {
		var table, name, typ string
		if err := rows.Scan(&table, &name, &typ); err != nil {
			return nil, err
		}
		out[table] = append(out[table], column{Name: name, Type: typ})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, cols := range out {
		sort.Slice(cols, func(i, j int) bool { return cols[i].Name < cols[j].Name })
	}
	return out, nil
}

// introspectSQL reads base tables only: views and sequences are not part of the
// projection and change for reasons the read model does not care about.
const introspectSQL = `
SELECT c.table_name, c.column_name, c.data_type
FROM information_schema.columns c
JOIN information_schema.tables t
  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
WHERE c.table_schema = 'dbos' AND t.table_type = 'BASE TABLE'
ORDER BY c.table_name, c.column_name`

// pinnedDBOSVersion is the DBOS version this build is linked against — the
// version whose migrations produced the schema being introspected, and the one
// SPEC.md §17.4 requires the failure message to name.
//
// Build info is tried first because it is what the binary actually links.
// Test binaries do not always carry a populated Deps list, though, so go.mod is
// the fallback rather than the primary: it is the same number, read from the
// file that decides it.
func pinnedDBOSVersion(t *testing.T) string {
	t.Helper()

	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == dbosModule {
				return dep.Version
			}
		}
	}

	root, err := harness.ModuleRoot()
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(root, "go.mod"))
	require.NoError(t, err, "read go.mod to determine the pinned DBOS version")

	m := dbosRequire.FindStringSubmatch(string(body))
	require.Len(t, m, 2, "%s is not required by go.mod", dbosModule)
	return m[1]
}

// dbosRequire matches the `require` line for the DBOS module in go.mod.
var dbosRequire = regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(dbosModule) + `\s+(v\S+)`)

func readFixture() (schemaFixture, error) {
	body, err := os.ReadFile(fixturePath)
	if err != nil {
		return schemaFixture{}, err
	}
	var f schemaFixture
	if err := json.Unmarshal(body, &f); err != nil {
		return schemaFixture{}, err
	}
	return f, nil
}

func writeFixture(f schemaFixture) error {
	if err := os.MkdirAll(filepath.Dir(fixturePath), 0o750); err != nil {
		return err
	}
	body, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(fixturePath, append(body, '\n'), 0o600)
}
