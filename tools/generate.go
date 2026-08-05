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
// KNOWN GAP — `go generate ./...` is not yet re-runnable, and the SPEC.md §17.3
// CI gate (`go generate ./... && git diff --exit-code`) therefore cannot pass.
// gopgql v0.2.0 cannot re-read the migration it just wrote:
//
//	gopgql: migrate: read migration 1: ddl: expected "SOURCE KEY", got "AS" at offset 572
//	exit status 1
//
// Offset 572 is `dbos.operation_outputs AS "SPAWNED"`. gopgql's generator emits
// an `AS "<alias>"` clause on any edge element whose table is also a vertex
// element — which is the whole shape of an externally-owned schema like
// `dbos.*` — and gopgql's own migration DDL reader does not accept that clause.
// So the first generation succeeds and every subsequent one exits 1.
//
// This needs a fix in gopgql, not here. Deleting `generated/graph/` before
// regenerating is not a workaround: the directory is a goose history, and
// throwing it away changes what a deployed database is migrated from.
package tools

//go:generate go run github.com/gaarutyunov/gopgql/cmd/gopgql generate --sdl ../schema/dbos.graphql --dir ../generated/graph --name dbos_graph --graph agentiq_graph
//go:generate go run github.com/gaarutyunov/gopgql/cmd/gopgql generate client --sdl ../schema/dbos.graphql --operations ../schema/operations --out ../generated/client --package client --graph agentiq_graph
