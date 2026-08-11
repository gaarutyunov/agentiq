# `generated/graph/` — gopgql output, never hand-edited

Everything in this directory except this file is written by
`gopgql generate` from `generated/schema/agentiq.graphql` — the concatenation of
`schema/dbos.graphql` and `schema/agentiq.graphql` that `tools/sdlmerge` writes,
because gopgql's `--sdl` takes one file and one property graph can only span two
PostgreSQL schemas if one document describes both (SPEC.md §11). Regenerate with
`go generate ./...`; drift fails CI (SPEC.md §17.3). gopgql only ever writes
numbered `.sql` files here, so this note survives regeneration.

`0001_dbos_graph_graph.sql` created `agentiq_graph` over `dbos.*` alone: three
vertex elements and four edge elements. `HAS_STEP` (Workflow → Step) and
`SPAWNED` (Step → Workflow) both map onto `dbos.operation_outputs`; two edge
elements over one physical table is intentional and legal, and both are needed —
`HAS_STEP` is SPEC.md §16's M1 traversal, `SPAWNED` is how a workflow reaches the
children it started.

`0002` drops that graph and `0003` recreates it spanning **both** schemas, which
is what M2 needs: ten vertex elements and eleven edge elements, adding the seven
`agentiq.*` types of SPEC.md §7.1 and the six edges between them, plus
`HAS_SESSION` from `dbos.workflow_status` to `agentiq.session` (SPEC.md §6.2,
§7.3). The pair is one generation — `0002` first because PostgreSQL refuses to
alter a column a live property graph exposes.

`0003` references `agentiq.*` tables, so **the table migrations have to be
applied before it**. gopgql v0.2.2 does not currently emit them; see
`generated/migrations/README.md`.

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
