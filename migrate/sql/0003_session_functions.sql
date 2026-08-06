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

-- `agentiq.create_session` is an upsert, deliberately.
--
-- ADK's `Service.Create` is called with a client-supplied `SessionID` on every
-- turn of a conversation, not once per conversation, and `sessiontestsuite`
-- calls it repeatedly with the same id. Returning the existing row is what
-- makes that a no-op rather than a duplicate-key error, and it is also what a
-- replayed workflow needs: the second attempt must find the session the first
-- one made.
CREATE OR REPLACE FUNCTION agentiq.create_session(
    p_adk_id        text,
    p_app_name      text,
    p_user_id       text,
    p_agent_digest  text,
    p_workflow_uuid text,
    p_state         json
) RETURNS uuid
LANGUAGE plpgsql
AS $fn$
DECLARE
    v_id uuid;
BEGIN
    INSERT INTO agentiq.session (
        adk_id, app_name, user_id, agent_digest, workflow_uuid,
        created_at_ts, last_update_at
    )
    VALUES (p_adk_id, p_app_name, p_user_id, p_agent_digest, p_workflow_uuid, now(), now())
    ON CONFLICT ON CONSTRAINT session_key DO NOTHING
    RETURNING id INTO v_id;

    IF v_id IS NULL THEN
        SELECT id INTO v_id
          FROM agentiq.session
         WHERE app_name = p_app_name AND user_id = p_user_id AND adk_id = p_adk_id;
    END IF;

    -- Initial state, projected onto its §6.4 scopes by the caller before it
    -- gets here. `temp:` keys are already gone (§7.2 rule 4), so nothing here
    -- has to know about them.
    INSERT INTO agentiq.session_state (session_id, scope, "key", value_json, updated_at_ts)
    SELECT v_id, s.scope, s."key", s.value_json, now()
      FROM json_populate_recordset(NULL::agentiq.session_state, p_state) AS s
    ON CONFLICT ON CONSTRAINT session_state_key
    DO UPDATE SET value_json = EXCLUDED.value_json, updated_at_ts = EXCLUDED.updated_at_ts;

    RETURN v_id;
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
