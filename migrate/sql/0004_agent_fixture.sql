-- The M2 agent fixture (SPEC.md §20 M2 Configuration, D24).
--
-- "Agent rows are **seeded directly** by a fixture migration. The OCI pull
-- lands in M3 and writes these same rows. Nothing built here is discarded."
--
-- The word that matters there is *these same rows*. This is not scaffolding to
-- be deleted when M3 arrives: `agentiq.agent` is the table M3's epos projection
-- writes into, with the same columns and the same key, and the only difference
-- is where the values come from — a manifest instead of this file. So the
-- fixture is written the way the projection will write: one row per resolved
-- closure, keyed by the digest of its OCI image index (§6.3).
--
-- The digest is a real, well-formed sha256 and not a placeholder string. §6.3
-- makes the digest the identity of an agent, `Session.agent_digest` carries it,
-- and `workflow_agent` pins it at enqueue; something that does not look like a
-- digest would pass here and fail the first time anything parsed one.
--
-- It is `ON CONFLICT DO NOTHING` because M3's projection may have written the
-- row already by the time a database is migrated forward, and a fixture that
-- overwrote a projected agent would silently un-deploy it.
--
-- +goose Up
-- +goose StatementBegin

INSERT INTO agentiq.agent (
    digest, name, description, model, instruction,
    skills, tools, sub_agents, created_at_ts
) VALUES (
    'sha256:0f0e4d9a7b1c2e3f4a5b6c7d8e9f0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b',
    'agentiq-demo',
    'The single agent M2 runs: one turn, no tools, no sub-agents.',
    -- SPEC.md §9.7 routes generation through OpenRouter, whose model ids are
    -- `vendor/model`. A small, cheap, widely available one, because the demo
    -- pays for its own inference through the user's PKCE-issued key (D19) and
    -- the point of the turn is that it is durable, not that it is clever.
    'openai/gpt-4o-mini',
    'You are AgentIQ''s demo agent. Answer in one short paragraph.',
    -- Empty, not absent. M2 has no tools (§20 M2) and no sub-agents; the
    -- columns exist so that M3 adds rows rather than columns, and `[]` says
    -- "resolved, and there were none" where NULL would say "not resolved".
    '[]'::json, '[]'::json, '[]'::json,
    -- A fixed timestamp, not now(): a migration whose result depends on when it
    -- ran is one whose output cannot be compared between two databases.
    TIMESTAMPTZ '2026-01-01 00:00:00+00'
)
ON CONFLICT ON CONSTRAINT agent_key DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DELETE FROM agentiq.agent
 WHERE digest = 'sha256:0f0e4d9a7b1c2e3f4a5b6c7d8e9f0a1b2c3d4e5f60718293a4b5c6d7e8f90a1b';
-- +goose StatementEnd
