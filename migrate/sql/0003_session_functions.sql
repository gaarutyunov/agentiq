-- The session lifecycle functions ADK's `session.Service` needs
-- (SPEC.md §8.3): create and delete.
--
-- Reads are not here. `Query.session`, `Query.sessions`, `Query.event` and
-- `Query.part` are compiled `GRAPH_TABLE` traversals in the generated client,
-- so the read model stays generated (§21) and only the writes — which gopgql
-- derives none of — are functions.
--
-- +goose Up
-- +goose StatementBegin

-- `CREATE OR REPLACE FUNCTION` cannot change a function's return type — it
-- fails with "cannot change return type of existing function" rather than
-- replacing it. This migration's own history contains such a change
-- (`create_session` returned `uuid` before it returned `text`), so the drop is
-- what makes re-applying it over a database that has the older definition work
-- at all. It is a no-op on the fresh database every environment here starts
-- from.
DROP FUNCTION IF EXISTS agentiq.create_session(text, text, text, text, text, json);

-- `agentiq.create_session` creates, and refuses to create twice.
--
-- `sessiontestsuite`'s `Create/when_already_exists,_it_fails` requires the
-- second call with the same id to fail, so this is not a place where an upsert
-- would be a kindness — it would be a conformance failure. A caller that wants
-- "the session, creating it if absent" is asking for ADK's own
-- `AutoCreateSession`: Get first, Create when Get says not found. That is what
-- `workflow.AgentRun` does.
--
-- A replayed workflow does not need the upsert either, which is the thing worth
-- knowing here. `Create` runs inside `dbos.RunAsTransaction`, so the session row
-- and the step checkpoint commit together; a replay is handed the checkpointed
-- id and never reaches this function a second time.
--
-- It returns the *ADK* session id and not the surrogate uuid, because that is
-- the id ADK's `CreateResponse.Session.ID()` has to carry and the only one the
-- caller can ask for the session back by. It is also the one the caller may not
-- know: `sessiontestsuite`'s `Create/generated_session_id` calls Create with no
-- session id at all and requires one to come back.
--
-- Allocating it here rather than in Go is what keeps that call usable from
-- workflow code. A uuid drawn in the application would be non-deterministic and
-- a replay would invent a second session; drawn here it is a value the
-- surrounding `dbos.RunAsTransaction` checkpoints, so the replay is handed the
-- id the first attempt committed — the same argument `sequence` rests on.
CREATE OR REPLACE FUNCTION agentiq.create_session(
    p_adk_id        text,
    p_app_name      text,
    p_user_id       text,
    p_agent_digest  text,
    p_workflow_uuid text,
    p_state         json
) RETURNS text
LANGUAGE plpgsql
AS $fn$
DECLARE
    v_id uuid;
BEGIN
    IF p_adk_id IS NULL OR p_adk_id = '' THEN
        p_adk_id := gen_random_uuid()::text;
    END IF;

    INSERT INTO agentiq.session (
        adk_id, app_name, user_id, agent_digest, workflow_uuid,
        created_at_ts, last_update_at
    )
    VALUES (p_adk_id, p_app_name, p_user_id, p_agent_digest, p_workflow_uuid, now(), now())
    ON CONFLICT ON CONSTRAINT session_key DO NOTHING
    RETURNING id INTO v_id;

    IF v_id IS NULL THEN
        -- The natural key was taken. Raised rather than returned so the caller
        -- can tell "created" from "already there" — the conformance suite reads
        -- exactly that difference — and with `unique_violation` so it arrives
        -- through the generated client as an *exec.FunctionError carrying 23505.
        RAISE EXCEPTION USING
            ERRCODE = 'unique_violation',
            MESSAGE = format('session %L already exists for %L/%L', p_adk_id, p_app_name, p_user_id);
    END IF;

    -- Initial state, projected onto its §6.4 scopes by the caller before it
    -- gets here. `temp:` keys are already gone (§7.2 rule 4), so nothing here
    -- has to know about them.
    INSERT INTO agentiq.session_state (session_id, scope, "key", value_json, updated_at_ts)
    SELECT v_id, s.scope, s."key", s.value_json, now()
      FROM json_populate_recordset(NULL::agentiq.session_state, p_state) AS s
    ON CONFLICT ON CONSTRAINT session_state_key
    DO UPDATE SET value_json = EXCLUDED.value_json, updated_at_ts = EXCLUDED.updated_at_ts;

    RETURN p_adk_id;
END;
$fn$;

-- `agentiq.delete_session` removes the session and everything hanging off it.
--
-- The children are deleted explicitly rather than by `ON DELETE CASCADE`,
-- because gopgql owns these tables and emits no foreign keys — the `@edge`
-- relationships are joins in the property graph, not constraints. Leaving the
-- rows would leave an event with no session, which the graph would still
-- happily traverse from the other end.
CREATE OR REPLACE FUNCTION agentiq.delete_session(
    p_adk_id   text,
    p_app_name text,
    p_user_id  text
) RETURNS integer
LANGUAGE plpgsql
AS $fn$
DECLARE
    v_id      uuid;
    v_deleted integer;
BEGIN
    SELECT id INTO v_id
      FROM agentiq.session
     WHERE app_name = p_app_name AND user_id = p_user_id AND adk_id = p_adk_id;

    IF v_id IS NULL THEN
        -- Deleting a session that is not there is a success, not an error:
        -- ADK's Delete has no "not found" and `sessiontestsuite` calls it on
        -- sessions it never created.
        RETURN 0;
    END IF;

    DELETE FROM agentiq.state_delta    WHERE event_id IN (SELECT id FROM agentiq.event WHERE session_id = v_id);
    DELETE FROM agentiq.artifact_delta WHERE event_id IN (SELECT id FROM agentiq.event WHERE session_id = v_id);
    DELETE FROM agentiq.actions        WHERE event_id IN (SELECT id FROM agentiq.event WHERE session_id = v_id);
    DELETE FROM agentiq.part           WHERE event_id IN (SELECT id FROM agentiq.event WHERE session_id = v_id);
    DELETE FROM agentiq.event          WHERE session_id = v_id;
    DELETE FROM agentiq.session_state  WHERE session_id = v_id;
    DELETE FROM agentiq.session        WHERE id = v_id;

    GET DIAGNOSTICS v_deleted = ROW_COUNT;
    RETURN v_deleted;
END;
$fn$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS agentiq.create_session(text, text, text, text, text, json);
DROP FUNCTION IF EXISTS agentiq.delete_session(text, text, text);
-- +goose StatementEnd
