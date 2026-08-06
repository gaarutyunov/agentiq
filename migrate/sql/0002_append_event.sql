-- `agentiq.append_event` — one event, five tables, one statement from the
-- caller's point of view (SPEC.md §8.3, D2).
--
-- This is hand-written, and it has to be: gopgql derives no writes of its own.
-- Its README is explicit — "there is no generated `createPerson` and no
-- inferred input type — it calls what the SDL names" — so the only write path
-- through a generated client is a `Mutation` field carrying `@function`, and
-- the function has to exist in the database first. `Mutation.appendEvent` in
-- schema/agentiq.graphql names this one.
--
-- That is not a workaround. It is what makes D2 achievable at all: the whole
-- append is one server-side call, so it either happens or it does not, inside
-- whatever transaction `dbos.RunAsTransaction` handed the caller. Five
-- round-trips from Go would commit the step checkpoint atomically with only the
-- last of them.
--
-- SPEC.md §21's "no hand-written SQL" is about the read model — the queries the
-- application issues — and the `rawsql` analyzer enforces it over Go string
-- literals. A PL/pgSQL function in a migration is neither: it is schema, like
-- the `CREATE TABLE` above it.
--
-- +goose Up
-- +goose StatementBegin

CREATE OR REPLACE FUNCTION agentiq.append_event(
    p_session_id       uuid,
    p_event            json,
    p_parts            json,
    p_actions          json,
    p_state_deltas     json,
    p_artifact_deltas  json
) RETURNS uuid
LANGUAGE plpgsql
AS $fn$
DECLARE
    v_event_id   uuid;
    v_actions_id uuid;
    v_sequence   integer;
BEGIN
    -- `sequence` is allocated here, inside the caller's transaction, and not by
    -- the application (SPEC.md §7.1's open question).
    --
    -- The workflow cannot allocate it: a counter derived from anything other
    -- than checkpointed state is non-deterministic, and the §17.1 analyzer
    -- cannot see that it is — it would look like arithmetic. Allocating it here
    -- makes it a function of committed rows, which is the one thing a replayed
    -- workflow and a fresh one agree about.
    --
    -- It does not perturb SPEC.md §7.2: `sequence` is not a field of
    -- `session.Event`, so it never reaches `json.Marshal`.
    SELECT COALESCE(MAX(sequence), 0) + 1
      INTO v_sequence
      FROM agentiq.event
     WHERE session_id = p_session_id;

    -- ON CONFLICT DO NOTHING over the natural key is what makes a retried
    -- append idempotent. `dbos.RunAsTransaction` commits the step checkpoint in
    -- this same transaction, so a retry after a successful commit should not
    -- reach here at all — but "should not" is a statement about DBOS, and
    -- exactly-once is the property M2 is asked to deliver rather than to
    -- assume. A second call with the same (session_id, adk_id) returns the
    -- first call's id and writes nothing.
    INSERT INTO agentiq.event (
        adk_id, session_id, sequence, invocation_id, author, branch, timestamp,
        turn_complete, interrupted, isolation_scope, error_code, error_message,
        content_role, long_running_tool_ids, requested_input, routes, node_info,
        grounding_metadata, usage_metadata, citation_metadata, custom_metadata,
        step_function_id, workflow_uuid
    )
    SELECT
        e.adk_id, p_session_id, v_sequence, e.invocation_id, e.author, e.branch,
        e.timestamp, e.turn_complete, e.interrupted, e.isolation_scope,
        e.error_code, e.error_message, e.content_role, e.long_running_tool_ids,
        e.requested_input, e.routes, e.node_info, e.grounding_metadata,
        e.usage_metadata, e.citation_metadata, e.custom_metadata,
        e.step_function_id, e.workflow_uuid
      FROM json_populate_record(NULL::agentiq.event, p_event) AS e
    ON CONFLICT ON CONSTRAINT event_key DO NOTHING
    RETURNING id INTO v_event_id;

    IF v_event_id IS NULL THEN
        -- The conflict fired: this event is already stored. Return the id it
        -- already has so the caller's own bookkeeping still resolves, and leave
        -- every child table alone — re-inserting parts would duplicate them
        -- under a key that does not forbid it.
        SELECT id INTO v_event_id
          FROM agentiq.event
         WHERE session_id = p_session_id
           AND adk_id = (json_populate_record(NULL::agentiq.event, p_event)).adk_id;
        RETURN v_event_id;
    END IF;

    -- Parts. `part_index` arrives in the document rather than being derived
    -- from array position here: SPEC.md §7.2 rule 2 makes it the slice position
    -- the application saw, and re-deriving it from the JSON array would be a
    -- second opinion about the same fact.
    INSERT INTO agentiq.part (
        event_id, part_index, text, thought, thought_signature,
        function_call_id, function_call_name, function_call_args,
        function_response_id, function_response_name, function_response_response,
        inline_data_mime_type, inline_data_bytes, inline_data_display_name,
        file_data_mime_type, file_data_uri, file_data_display_name,
        executable_code_language, executable_code_code,
        code_execution_outcome, code_execution_output,
        video_metadata_start_offset, video_metadata_end_offset, video_metadata_fps,
        audio_transcription, media_resolution, tool_call, tool_response,
        part_metadata
    )
    SELECT
        v_event_id, p.part_index, p.text, p.thought, p.thought_signature,
        p.function_call_id, p.function_call_name, p.function_call_args,
        p.function_response_id, p.function_response_name, p.function_response_response,
        p.inline_data_mime_type, p.inline_data_bytes, p.inline_data_display_name,
        p.file_data_mime_type, p.file_data_uri, p.file_data_display_name,
        p.executable_code_language, p.executable_code_code,
        p.code_execution_outcome, p.code_execution_output,
        p.video_metadata_start_offset, p.video_metadata_end_offset, p.video_metadata_fps,
        p.audio_transcription, p.media_resolution, p.tool_call, p.tool_response,
        p.part_metadata
      FROM json_populate_recordset(NULL::agentiq.part, p_parts) AS p;

    -- Actions is 0..1 and `session.Event.Actions` is a value rather than a
    -- pointer, so a row is always written. An event with no actions still has
    -- an actions object in its JSON, and a missing row would read back as a
    -- different document.
    INSERT INTO agentiq.actions (
        event_id, skip_summarization, transfer_to_agent, escalate,
        requested_auth_configs
    )
    SELECT
        v_event_id, a.skip_summarization, a.transfer_to_agent, a.escalate,
        a.requested_auth_configs
      FROM json_populate_record(NULL::agentiq.actions, p_actions) AS a
    RETURNING id INTO v_actions_id;

    -- State deltas. `temp:` keys never arrive here — they are dropped before
    -- the document is built (SPEC.md §7.2 rule 4) — so no row can carry
    -- scope = 'temp' and nothing downstream has to filter for it.
    INSERT INTO agentiq.state_delta (actions_id, event_id, scope, "key", value_json)
    SELECT v_actions_id, v_event_id, d.scope, d."key", d.value_json
      FROM json_populate_recordset(NULL::agentiq.state_delta, p_state_deltas) AS d;

    INSERT INTO agentiq.artifact_delta (actions_id, event_id, filename, version)
    SELECT v_actions_id, v_event_id, ad.filename, ad.version
      FROM json_populate_recordset(NULL::agentiq.artifact_delta, p_artifact_deltas) AS ad;

    -- Project the deltas onto the session's state (SPEC.md §6.1, §6.4). Last
    -- write wins per (session, scope, key), which is what a delta means.
    INSERT INTO agentiq.session_state (session_id, scope, "key", value_json, updated_at_ts)
    SELECT p_session_id, d.scope, d."key", d.value_json, now()
      FROM json_populate_recordset(NULL::agentiq.state_delta, p_state_deltas) AS d
    ON CONFLICT ON CONSTRAINT session_state_key
    DO UPDATE SET value_json = EXCLUDED.value_json,
                  updated_at_ts = EXCLUDED.updated_at_ts;

    UPDATE agentiq.session
       SET last_update_at = now()
     WHERE id = p_session_id;

    RETURN v_event_id;
END;
$fn$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS agentiq.append_event(uuid, json, json, json, json, json);
-- +goose StatementEnd
