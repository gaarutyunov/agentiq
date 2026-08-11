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

## One directory, one purpose: tables here, the property graph next door

`generated/graph/` is generated with `--no-tables` and this one with
`--no-graph`, so neither history ever emits the other's DDL. That split is not
free and is worth knowing about before changing the SDL.

gopgql is designed around **one** directory holding both, because a change to a
column a live property graph exposes has to be sequenced `DROP PROPERTY GRAPH`
→ `ALTER TABLE` → `CREATE PROPERTY GRAPH`, and only one goose history can
order those three against each other. AgentIQ splits them because `migrate/`
embeds and applies the two as separate `//go:embed` trees, tables first.

That works for an additive change. A change that **alters or drops** an existing
column of a table the graph exposes will not sequence correctly across two
histories, and the fix at that point is to merge them into one directory rather
than to hand-order the migrations.

## Historical note: this directory was empty until gopgql v0.3.0

gopgql v0.2.2 emitted no table DDL at all for this schema and exited 0 doing it
— 0 of 7 tables — because `migrate/diff.go` collected the keys of unmanaged
*edge* tables into one drop set that was then applied to *vertex* tables too.
Every `@edge` in SPEC.md §7.1 is a foreign-key edge, so every owned table was
some edge's table and every `CREATE TABLE` disappeared with it.

That was gaarutyunov/gopgql#53 defect A, fixed in v0.3.0: unowned is a property
of a role, not of a name. The same release makes the no-op case fail loudly with
`ErrNothingWritten` rather than exiting 0 having written nothing, so this cannot
silently recur.
