// Package demo carries the parts of the browser demo that are not themselves
// browser-only, so that they compile, vet and test on every platform.
//
// Today that is one thing: the property-graph DDL the browser build has to
// execute after `dbos migrate` has created `dbos.*`, because SPEC.md §16
// requires a `GRAPH_TABLE` query to answer in the browser build as well as the
// server build.
//
// # Why the DDL is copied here
//
// The statements are gopgql's output and live in `generated/graph/`
// (SPEC.md §11). `//go:embed` cannot reference a parent directory, and neither
// `demo/wasm` nor this package sits above `generated/`, so the file cannot be
// embedded where it is written. The `go:generate` directive below copies it in,
// which makes the copy a *generated artifact*: SPEC.md §17.3's
// `go generate ./... && git diff --exit-code` gate fails the build the moment
// `generated/graph/` changes and `demo/graph/` does not. A copy nothing checks
// would be a fork; a copy the drift gate checks is a build step.
//
// Fetching the `.sql` over HTTP instead was the alternative, and it is worse:
// SPEC.md §16's `demo` target copies only `demo/web/` into `demo/dist/`, and an
// asset fetched at runtime is one more relative path to get wrong under
// `/pr-preview/pr-N/` (SPEC.md §13.3).
package demo

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

//go:generate sh -c "mkdir -p graph && rm -f graph/*.sql && cp ../generated/graph/*.sql graph/"

//go:embed graph/*.sql
var graphFS embed.FS

// Goose direction markers. `generated/graph/` is a goose history
// (see generated/graph/README.md), so every file in it is annotated even though
// the browser build has no goose: there is no `database/sql` under js/wasm to
// give it, and one CREATE plus one DROP is not a migration engine's worth of
// work.
const (
	gooseUp   = "-- +goose Up"
	gooseDown = "-- +goose Down"
)

// GraphMigration is one generated graph migration, split into the two
// directions goose annotates.
type GraphMigration struct {
	// Name is the file's base name, which carries the goose version prefix.
	Name string
	// Up creates. Down drops. Both are whole statements, ready to Exec.
	Up   string
	Down string
}

// GraphMigrations returns every migration in `generated/graph/`, in the order
// goose would apply them.
//
// The browser applies Down-then-Up on every boot rather than tracking versions.
// That is safe because the whole history describes one graph and a property
// graph holds no data — it is a view over `dbos.*`, so dropping and recreating
// it loses nothing — and it is *necessary* because the tab reloads:
// `CREATE PROPERTY GRAPH` on a graph that survived in IndexedDB would fail with
// a duplicate-object error on the second load, which is exactly the reload that
// failure-matrix row F22 requires to work.
func GraphMigrations() ([]GraphMigration, error) {
	entries, err := fs.ReadDir(graphFS, "graph")
	if err != nil {
		return nil, fmt.Errorf("demo: read graph migrations: %w", err)
	}

	out := make([]GraphMigration, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := fs.ReadFile(graphFS, path.Join("graph", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("demo: read %s: %w", e.Name(), err)
		}
		m, err := splitGoose(e.Name(), string(body))
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("demo: no graph migrations embedded; run `go generate ./...`")
	}
	return out, nil
}

// splitGoose cuts one annotated file into its two directions.
//
// It is deliberately strict. A file whose markers moved would otherwise yield
// an empty Up, and an empty Up is a demo that boots, reports success and then
// fails every graph query with "graph does not exist" — a much longer walk back
// to the cause than a startup error naming the file.
func splitGoose(name, body string) (GraphMigration, error) {
	up := strings.Index(body, gooseUp)
	if up < 0 {
		return GraphMigration{}, fmt.Errorf("demo: %s: no %q marker", name, gooseUp)
	}
	down := strings.Index(body, gooseDown)
	if down < 0 {
		return GraphMigration{}, fmt.Errorf("demo: %s: no %q marker", name, gooseDown)
	}
	if down < up {
		return GraphMigration{}, fmt.Errorf("demo: %s: %q precedes %q", name, gooseDown, gooseUp)
	}

	m := GraphMigration{
		Name: name,
		Up:   strings.TrimSpace(body[up+len(gooseUp) : down]),
		Down: strings.TrimSpace(body[down+len(gooseDown):]),
	}
	if m.Up == "" {
		return GraphMigration{}, fmt.Errorf("demo: %s: nothing between %q and %q", name, gooseUp, gooseDown)
	}
	if m.Down == "" {
		return GraphMigration{}, fmt.Errorf("demo: %s: nothing after %q", name, gooseDown)
	}
	return m, nil
}
