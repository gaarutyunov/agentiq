-- The `agentiq` schema, and the ledger that records what has been applied to
-- it.
--
-- This is the one migration AgentIQ writes by hand, and it is hand-written
-- because gopgql will not generate it: `sdl/readonly.go` puts it as "a schema
-- it does not own is not its to create". Without it `CREATE TABLE
-- agentiq.session` fails with SQLSTATE 3F000 and so does the property graph in
-- `generated/graph/0003`, which names `agentiq.*` tables.
--
-- Both statements are `IF NOT EXISTS` and that is required, not defensive:
-- this migration is applied unconditionally on every boot, because it is what
-- creates the table the other migrations are recorded in and so cannot itself
-- be recorded. See migrate/migrate.go.
--
-- `schema_migrations` is deliberately not in the SDL and must not be. It is
-- bookkeeping about migrations, not part of the SPEC.md §7.1 domain model, and
-- a table in the SDL is a vertex of `agentiq_graph`.

-- +goose Up
CREATE SCHEMA IF NOT EXISTS agentiq;

CREATE TABLE IF NOT EXISTS agentiq.schema_migrations (
    version    text        PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP SCHEMA IF EXISTS agentiq CASCADE;
