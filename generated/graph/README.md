# `generated/graph/` — gopgql output, never hand-edited

Everything in this directory except this file is written by
`gopgql generate` from `schema/dbos.graphql` (SPEC.md §11). Regenerate with
`go generate ./...`; drift fails CI (SPEC.md §17.3). gopgql only ever writes
numbered `.sql` files here, so this note survives regeneration.

`0001_dbos_graph_graph.sql` creates `agentiq_graph` with three vertex elements
and four edge elements. `HAS_STEP` (Workflow → Step) and `SPAWNED`
(Step → Workflow) both map onto `dbos.operation_outputs`; two edge elements over
one physical table is intentional and legal, and both are needed — `HAS_STEP` is
SPEC.md §16's M1 traversal, `SPAWNED` is how a workflow reaches the children it
started.

## This directory is a goose history, and it is append-only from here on

SQL/PGQ has no `ALTER PROPERTY GRAPH`, so gopgql expresses any later change to
the graph as a new drop migration plus a new create migration rather than by
editing `0001` in place. That is correct once a migration has been applied
anywhere, and it is why **you must not delete this directory to "start clean"** —
that changes what a deployed database is migrated from, and goose would see a
version it has already applied change underneath it.

The one exception has already been spent. `0001` was regenerated from empty
once, on this branch, while no database had ever applied it and the branch was
unmerged. What that discarded was a create-wrong / drop / create-right sequence:
gopgql v0.2.0 emitted the first graph without `HAS_STEP` (gopgql#49), and
regenerating under v0.2.1 appended the fix forward instead of correcting it.
Keeping that would have shipped a known-broken step for every future environment
to execute for no reason, and left a misleading `0001` for anyone reading the DDL
to learn the schema. Once this branch merges, the reasoning no longer applies.
