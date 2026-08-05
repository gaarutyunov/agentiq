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

	// Asserted by shape and by the graph's name, not by quoting the statement
	// back. A test that pins the DDL text would be a second, unchecked copy of
	// `generated/graph/` — the thing SPEC.md §17.3's drift gate exists to
	// prevent — and it would trip the rawsql analyzer, which is right to flag a
	// hand-written CREATE anywhere outside generated/.
	assert.True(t, strings.HasPrefix(first.Up, "CREATE"), "Up must create: %q", summarise(first.Up))
	assert.True(t, strings.HasPrefix(first.Down, "DROP"), "Down must drop: %q", summarise(first.Down))
	assert.Contains(t, first.Up, graphName)
	assert.Contains(t, first.Down, graphName)

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

// graphName is the property graph the generator is configured to emit
// (tools/generate.go passes `--graph agentiq_graph`). The generated client
// bakes the same name into its GRAPH_TABLE statements, so a mismatch compiles
// and then fails at run time against a graph that does not exist.
const graphName = "agentiq_graph"

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
			_, err := splitGoose("0001_x.sql", tc.body)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "0001_x.sql", "the error must name the file")
		})
	}
}

func TestSplitGooseTrimsBothDirections(t *testing.T) {
	// The bodies are deliberately not SQL: the splitter cuts on the goose
	// markers and never looks at what is between them, and saying so with
	// placeholder text asserts that property instead of re-asserting the DDL.
	m, err := splitGoose("0001_x.sql", strings.Join([]string{
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
