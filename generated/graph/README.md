# `generated/graph/` — gopgql output, never hand-edited

Everything in this directory except this file is written by
`gopgql generate` from `schema/dbos.graphql` (SPEC.md §11). Regenerate with
`go generate ./...`; drift fails CI (SPEC.md §17.3). gopgql only ever writes
numbered `.sql` files here, so this note survives regeneration.

## The graph in `0001_dbos_graph_graph.sql` is incomplete. This is a gopgql bug, not a design choice.

**`HAS_STEP` is missing from `EDGE TABLES`.** `schema/dbos.graphql` declares
four edges — `HAS_STEP`, `PARENT`, `EMITTED`, `SPAWNED`. Only three are here.

gopgql v0.2.0's `collectEdges` deduplicates edges by *physical table name*
rather than by *relationship label* (its own doc comment says "one physical
edge table per relationship label"). `HAS_STEP` (Workflow → Step) and
`SPAWNED` (Step → Workflow) are both mapped onto `dbos.operation_outputs`, so
the second one seen is discarded — silently, with exit status 0. Which one
survives depends on type iteration order; today `SPAWNED` wins.

Consequences while the bug stands:

- `generated/client/`'s `WorkflowWithSteps` compiles
  `MATCH (v0 IS workflow) -[e0 IS "HAS_STEP"]-> (v1 IS step)` against an edge
  that does not exist. It fails at **run time**, not at generate time.
- SPEC.md §16's M1 acceptance — "a `GRAPH_TABLE` query over `dbos.*` returns a
  workflow with its steps" — cannot pass.

**Do not fix this by editing the SQL, and do not fix it by deleting one of the
two edges from the SDL.** The SDL is the contract and it is correct. Both
edges exist in `dbos.operation_outputs` and both are needed: `HAS_STEP` is the
M1 traversal, `SPAWNED` is how a workflow reaches the children it started.
Regenerate once gopgql emits both.

**`go generate ./...` is also not re-runnable yet**, for a related reason in
the same feature — gopgql cannot parse the `AS "<alias>"` edge clause it
emits here when a table is both a vertex and an edge:

```
gopgql: migrate: read migration 1: ddl: expected "SOURCE KEY", got "AS" at offset 572
```

Offset 572 is `dbos.operation_outputs AS "SPAWNED"`. The first generation into
an empty directory succeeds; every subsequent one exits 1. Deleting this
directory to get around it is not a workaround — it is a goose history, and
discarding it changes what a deployed database is migrated from. Details in
`tools/generate.go`.
