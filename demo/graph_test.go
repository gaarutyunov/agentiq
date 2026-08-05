package demo

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGraphMigrationsMatchGenerated is the guard on the copy: it asserts that
// what got embedded is a real graph migration and not, say, an empty directory
// left behind by a `go generate` that was never run. The *contents* are checked
// against `generated/graph/` by SPEC.md §17.3's drift gate, which is the only
// check that can see both copies.
func TestGraphMigrationsMatchGenerated(t *testing.T) {
	ms, err := GraphMigrations()
	require.NoError(t, err)
	require.NotEmpty(t, ms)

	first := ms[0]
	assert.Equal(t, "0001_dbos_graph_graph.sql", first.Name)
	assert.Contains(t, first.Up, "CREATE PROPERTY GRAPH agentiq_graph")
	assert.Contains(t, first.Down, "DROP PROPERTY GRAPH IF EXISTS agentiq_graph")

	// The goose annotations must not survive into what gets Exec'd: the
	// browser has no goose to strip them, and `-- +goose Up` inside a
	// statement is a comment PostgreSQL would accept while the *next* file's
	// marker would not be.
	for _, m := range ms {
		assert.NotContains(t, m.Up, gooseUp, m.Name)
		assert.NotContains(t, m.Up, gooseDown, m.Name)
		assert.NotContains(t, m.Down, gooseDown, m.Name)
	}

	// The Down direction has to be idempotent, because the browser runs it on
	// every boot including the first, when there is no graph to drop.
	assert.Contains(t, first.Down, "IF EXISTS")
}

func TestSplitGooseRejectsMalformedFiles(t *testing.T) {
	tests := map[string]struct {
		body string
		want string
	}{
		"no up marker":    {body: "-- +goose Down\nDROP;", want: "no \"-- +goose Up\""},
		"no down marker":  {body: "-- +goose Up\nCREATE;", want: "no \"-- +goose Down\""},
		"reversed":        {body: "-- +goose Down\nDROP;\n-- +goose Up\nCREATE;", want: "precedes"},
		"empty up":        {body: "-- +goose Up\n\n-- +goose Down\nDROP;", want: "nothing between"},
		"empty down":      {body: "-- +goose Up\nCREATE;\n-- +goose Down\n", want: "nothing after"},
		"only whitespace": {body: "   \n\t\n", want: "no \"-- +goose Up\""},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := splitGoose("0001_x.sql", tc.body)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "0001_x.sql", "the error must name the file")
		})
	}
}

func TestSplitGooseTrimsBothDirections(t *testing.T) {
	m, err := splitGoose("0001_x.sql", strings.Join([]string{
		"-- +goose Up",
		"",
		"CREATE PROPERTY GRAPH g;",
		"",
		"-- +goose Down",
		"",
		"DROP PROPERTY GRAPH IF EXISTS g;",
		"",
	}, "\n"))
	require.NoError(t, err)

	assert.Equal(t, "CREATE PROPERTY GRAPH g;", m.Up)
	assert.Equal(t, "DROP PROPERTY GRAPH IF EXISTS g;", m.Down)
}
