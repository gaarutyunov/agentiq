# `generated/migrations/` — gopgql output, never hand-edited

The goose history for the tables AgentIQ **owns**: the seven `agentiq.*` types
of SPEC.md §7.1. Written by `gopgql generate --no-graph` from
`generated/schema/agentiq.graphql`; regenerate with `go generate ./...`.

The `dbos.*` types are `@readonly` and get nothing here — no `CREATE TABLE`, no
`ALTER`, no `DROP`, no index. DBOS owns those tables and migrates them with
`dbos migrate`. That much works as intended and is worth re-checking whenever
this directory changes.

`generated/graph/0003` creates a property graph over these tables, so this
history is applied **before** it.

## This directory is empty, and that is a known gopgql defect — not "nothing to do"

gopgql v0.2.2 emits **no table DDL at all** for this schema, and exits 0 while
doing it:

```
$ gopgql generate --sdl generated/schema/agentiq.graphql \
                  --dir generated/migrations --name agentiq --no-graph
gopgql: generated/migrations is already up to date with generated/schema/agentiq.graphql
```

A table gopgql owns is skipped whenever some `@relationship` maps an edge onto
it. `generator.collectEdges` records such an edge as `EdgeTable{Unmanaged: true}`
and `generator.TablesDDL` skips every unmanaged entry — but the same physical
table is also a *vertex* table gopgql owns, and its `CREATE TABLE` disappears
with it. SPEC.md §7.1's edges are all foreign-key edges (`@edge(from: "id",
to: "session_id")`), so every `agentiq.*` table is some edge's table:

- `schema/agentiq.graphql` on its own emits 1 of 7 tables — only `session`, the
  one table no edge names.
- Merged with `schema/dbos.graphql` it emits 0, because `Workflow.sessions`
  claims `agentiq.session` as the `HAS_SESSION` edge table too.

It is not something the SDL can be written around. An edge touching a
`@readonly` type **must** be mapped onto an existing table —
`sdl.validateRelationshipMapping` refuses the alternative outright ("gopgql
would have to create an edge table referencing a table it does not own") — so
`agentiq.session` cannot both carry `HAS_SESSION` and be created by gopgql.
Rewriting the intra-`agentiq` edges as gopgql-generated junction tables would
recover six of the seven, at the cost of contradicting SPEC.md §7.1's physical
model, and would still leave `session` uncreated.

The fix belongs in gopgql: `TablesDDL` should emit the vertex table of a
non-`@readonly` node even when that table is also claimed as an unmanaged edge
table. Nothing in this repository needs to change when it lands — the SDL
already describes the tables correctly, and `go generate ./...` will fill this
directory.

## `CREATE SCHEMA agentiq` is nobody's job yet

gopgql deliberately emits no `CREATE SCHEMA` (`sdl/readonly.go`: "a schema it
does not own is not its to create"). `CREATE TABLE agentiq.session` fails
against a database where the schema does not exist, so something outside this
directory has to create it before the first migration runs.
