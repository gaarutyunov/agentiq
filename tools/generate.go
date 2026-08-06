// Package tools also carries the `go:generate` directives, so that
// `go generate ./...` regenerates everything in `generated/` from `schema/`
// (SPEC.md §11).
//
// The directives live here rather than beside the schema because `schema/`
// holds no Go files and `go generate ./...` only visits packages. They run
// `go run github.com/gaarutyunov/gopgql/cmd/gopgql` without an `@version`
// suffix on purpose: that resolves the generator from the module's build list,
// which is the version tools.go pins in go.mod. Naming a version here instead
// would let the two drift apart silently.
//
// Generation is hermetic: gopgql compiles the SDL and every operation document
// without a live database, so `go generate ./... && git diff --exit-code` is a
// usable CI gate (SPEC.md §17.3).
//
// The property-graph name is `agentiq_graph` in both directives. It has to be:
// the client generator bakes the graph name into the compiled `GRAPH_TABLE`
// statements, so a client generated against a different name compiles fine and
// then fails at run time against a graph that does not exist.
//
// `generated/graph/` is a goose history, so the graph directive is append-only:
// a run that finds the directory already matching the SDL writes nothing and
// exits 0, and a run that finds it stale appends a drop plus a recreate rather
// than rewriting what is there. Never delete the directory to "start clean" once
// any database has applied a migration from it — that changes what a deployed
// database is migrated from. See `generated/graph/README.md`.
package tools

//go:generate go run ./sdlmerge -out ../generated/schema/agentiq.graphql ../schema/dbos.graphql ../schema/agentiq.graphql
//go:generate go run github.com/gaarutyunov/gopgql/cmd/gopgql generate --sdl ../generated/schema/agentiq.graphql --dir ../generated/migrations --name agentiq --no-graph
//go:generate go run github.com/gaarutyunov/gopgql/cmd/gopgql generate --sdl ../generated/schema/agentiq.graphql --dir ../generated/graph --name dbos_graph --graph agentiq_graph
//go:generate go run github.com/gaarutyunov/gopgql/cmd/gopgql generate client --sdl ../generated/schema/agentiq.graphql --operations ../schema/operations --out ../generated/client --package client --graph agentiq_graph
