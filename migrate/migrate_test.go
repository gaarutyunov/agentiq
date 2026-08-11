package migrate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// graphName is the property graph the generator is configured to emit
// (tools/generate.go passes `--graph agentiq_graph`). The generated client
// bakes the same name into its GRAPH_TABLE statements, so a mismatch compiles
// and then fails at run time against a graph that does not exist.
const graphName = "agentiq_graph"

// schemaName is the schema `sql/0001` creates and every `agentiq.*` table lives
// in.
const schemaName = "agentiq"

// TestOwnStartsWithTheBootstrap pins the one ordering invariant [Apply] cannot
// recover from being wrong about: the first migration in `sql/` is what creates
// the ledger every later migration is recorded in. Apply checks this at run
// time too, but a unit test says so at `go test` time rather than against a
// live database.
func TestOwnStartsWithTheBootstrap(t *testing.T) {
	own, err := Own()
	require.NoError(t, err)
	require.NotEmpty(t, own)

	assert.Equal(t, bootstrapName, own[0].Name)

	// Applied unconditionally on every boot, so it has to be idempotent by
	// construction — there is no ledger yet to tell it that it has already run.
	assert.Contains(t, own[0].Up, "CREATE SCHEMA IF NOT EXISTS "+schemaName)
	assert.Contains(t, own[0].Up, "CREATE TABLE IF NOT EXISTS "+schemaName+".schema_migrations")
}

// TestGraphIsEmbedded is the guard on the copy: it asserts that what got
// embedded is a real graph migration and not, say, an empty directory left
// behind by a `go generate` that was never run. The *contents* are checked
// against `generated/graph/` by SPEC.md §17.3's drift gate, which is the only
// check that can see both copies.
func TestGraphIsEmbedded(t *testing.T) {
	ms, err := Graph()
	require.NoError(t, err)
	require.NotEmpty(t, ms)

	first := ms[0]
	assert.Equal(t, graphDir+"/0001_dbos_graph_graph.sql", first.Name)

	// Asserted by shape and by the graph's name, not by quoting the statement
	// back. A test that pinned the DDL text would be a second, unchecked copy
	// of `generated/graph/` — the thing SPEC.md §17.3's drift gate exists to
	// prevent.
	assert.True(t, strings.HasPrefix(first.Up, "CREATE"), "Up must create: %q", summarise(first.Up))
	assert.True(t, strings.HasPrefix(first.Down, "DROP"), "Down must drop: %q", summarise(first.Down))
	assert.Contains(t, first.Up, graphName)
	assert.Contains(t, first.Down, graphName)

	// The Down direction has to be idempotent, because every caller runs it on
	// every boot including the first, when there is no graph to drop.
	assert.Contains(t, first.Down, "IF EXISTS")
}

// TestTablesIsReadableWhileEmpty is not a formality. `tables/` holds no `.sql`
// today — gopgql v0.2.2 emits none (gaarutyunov/gopgql#53) — and the failure
// mode of getting this wrong is a compile error in every program that imports
// the package, because //go:embed rejects a pattern matching nothing.
func TestTablesIsReadableWhileEmpty(t *testing.T) {
	ms, err := Tables()
	require.NoError(t, err)

	// Deliberately not asserted non-empty: it is empty now and will not be
	// once gopgql#53 releases, and a test that pinned either state would have
	// to be edited to land the fix.
	for _, m := range ms {
		assert.NotEmpty(t, m.Up, m.Name)
		assert.True(t, strings.HasPrefix(m.Name, tablesDir+"/"), m.Name)
	}
}

// TestNamesArePrefixedByHistory covers what the ledger keys on. All three
// histories number from 0001 independently, so an unprefixed name would make
// `sql/0001` and `graph/0001` the same ledger row.
func TestNamesArePrefixedByHistory(t *testing.T) {
	own, err := Own()
	require.NoError(t, err)
	graph, err := Graph()
	require.NoError(t, err)

	for _, m := range own {
		assert.True(t, strings.HasPrefix(m.Name, ownDir+"/"), m.Name)
	}
	for _, m := range graph {
		assert.True(t, strings.HasPrefix(m.Name, graphDir+"/"), m.Name)
	}
}

// TestGooseAnnotationsAreStripped asserts the markers do not survive into what
// gets Exec'd. Nothing downstream strips them: there is no goose in a browser
// tab, and `-- +goose Up` inside a statement is a comment PostgreSQL would
// accept while the *next* file's marker would not be.
func TestGooseAnnotationsAreStripped(t *testing.T) {
	own, err := Own()
	require.NoError(t, err)
	graph, err := Graph()
	require.NoError(t, err)

	for _, m := range append(own, graph...) {
		assert.NotContains(t, m.Up, gooseUp, m.Name)
		assert.NotContains(t, m.Up, gooseDown, m.Name)
		assert.NotContains(t, m.Down, gooseDown, m.Name)
	}
}

func TestSplitGooseRejectsMalformedFiles(t *testing.T) {
	tests := map[string]struct {
		body string
		want string
	}{
		"no up marker":    {body: "-- +goose Down\ndown-body", want: "no \"-- +goose Up\""},
		"no down marker":  {body: "-- +goose Up\nup-body", want: "no \"-- +goose Down\""},
		"reversed":        {body: "-- +goose Down\ndown-body\n-- +goose Up\nup-body", want: "precedes"},
		"empty up":        {body: "-- +goose Up\n\n-- +goose Down\ndown-body", want: "nothing between"},
		"empty down":      {body: "-- +goose Up\nup-body\n-- +goose Down\n", want: "nothing after"},
		"only whitespace": {body: "   \n\t\n", want: "no \"-- +goose Up\""},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := splitGoose("sql/0001_x.sql", tc.body)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "sql/0001_x.sql", "the error must name the file")
		})
	}
}

func TestSplitGooseTrimsBothDirections(t *testing.T) {
	// The bodies are deliberately not SQL: the splitter cuts on the goose
	// markers and never looks at what is between them, and saying so with
	// placeholder text asserts that property instead of re-asserting the DDL.
	m, err := splitGoose("sql/0001_x.sql", strings.Join([]string{
		"-- +goose Up",
		"",
		"up-body",
		"",
		"-- +goose Down",
		"",
		"down-body",
		"",
	}, "\n"))
	require.NoError(t, err)

	assert.Equal(t, "up-body", m.Up)
	assert.Equal(t, "down-body", m.Down)
}

// summarise shortens a statement for a failure message.
func summarise(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 60 {
		s = s[:57] + "..."
	}
	return s
}
