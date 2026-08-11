-- +goose Up
DROP PROPERTY GRAPH IF EXISTS agentiq_graph;

-- +goose Down
CREATE PROPERTY GRAPH agentiq_graph
  VERTEX TABLES (
    agentiq.actions KEY (event_id) LABEL actions PROPERTIES (id, event_id, skip_summarization, transfer_to_agent, escalate, requested_auth_configs),
    agentiq.artifact_delta KEY (event_id, filename) LABEL artifact_delta PROPERTIES (id, actions_id, event_id, filename, version),
    agentiq.event KEY (session_id, adk_id) LABEL event PROPERTIES (id, adk_id, session_id, sequence, invocation_id, author, branch, timestamp, turn_complete, interrupted, isolation_scope, error_code, error_message, content_role, long_running_tool_ids, requested_input, routes, node_info, grounding_metadata, usage_metadata, citation_metadata, custom_metadata, step_function_id, workflow_uuid),
    agentiq.part KEY (event_id, part_index) LABEL part PROPERTIES (id, event_id, part_index, text, thought, thought_signature_b64, function_call_id, function_call_name, function_call_args, function_response_id, function_response_name, function_response_response, inline_data_mime_type, inline_data_bytes_b64, inline_data_display_name, file_data_mime_type, file_data_uri, file_data_display_name, executable_code_language, executable_code_code, code_execution_outcome, code_execution_output, video_metadata_start_offset, video_metadata_end_offset, video_metadata_fps, audio_transcription, media_resolution, tool_call, tool_response, part_metadata),
    agentiq.session KEY (app_name, user_id, adk_id) LABEL session PROPERTIES (id, adk_id, app_name, user_id, workflow_uuid, agent_digest, created_at_ts, last_update_at),
    agentiq.session_state KEY (session_id, scope, "key") LABEL session_state PROPERTIES (id, session_id, scope, "key", value_json, updated_at_ts),
    agentiq.state_delta KEY (event_id, scope, "key") LABEL state_delta PROPERTIES (id, actions_id, event_id, scope, "key", value_json),
    dbos.operation_outputs KEY (workflow_uuid, function_id) LABEL step PROPERTIES (workflow_uuid, function_id, function_name, output, error),
    dbos.streams KEY (workflow_uuid, "key", "offset") LABEL stream_value PROPERTIES (workflow_uuid, "key", "offset", value, function_id),
    dbos.workflow_status KEY (workflow_uuid) LABEL workflow PROPERTIES (workflow_uuid, status, name, queue_name, created_at, updated_at, recovery_attempts, attributes, inputs, output, error)
  )
  EDGE TABLES (
    agentiq.actions AS "HAS_ACTIONS" SOURCE KEY (event_id) REFERENCES event (id)
            DESTINATION KEY (id) REFERENCES actions (id)
            LABEL "HAS_ACTIONS" PROPERTIES (event_id, id),
    agentiq.artifact_delta AS "PRODUCES" SOURCE KEY (actions_id) REFERENCES actions (id)
            DESTINATION KEY (id) REFERENCES artifact_delta (id)
            LABEL "PRODUCES" PROPERTIES (actions_id, id),
    agentiq.event AS "HAS_EVENT" SOURCE KEY (session_id) REFERENCES session (id)
            DESTINATION KEY (id) REFERENCES event (id)
            LABEL "HAS_EVENT" PROPERTIES (session_id, id),
    dbos.operation_outputs AS "HAS_STEP" SOURCE KEY (workflow_uuid) REFERENCES workflow_status (workflow_uuid)
            DESTINATION KEY (workflow_uuid, function_id) REFERENCES operation_outputs (workflow_uuid, function_id)
            LABEL "HAS_STEP" PROPERTIES (workflow_uuid, function_id),
    dbos.operation_outputs AS "SPAWNED" SOURCE KEY (workflow_uuid, function_id) REFERENCES operation_outputs (workflow_uuid, function_id)
            DESTINATION KEY (child_workflow_id) REFERENCES workflow_status (workflow_uuid)
            LABEL "SPAWNED" PROPERTIES (workflow_uuid, function_id, child_workflow_id),
    agentiq.part AS "HAS_PART" SOURCE KEY (event_id) REFERENCES event (id)
            DESTINATION KEY (id) REFERENCES part (id)
            LABEL "HAS_PART" PROPERTIES (event_id, id),
    agentiq.session AS "HAS_SESSION" SOURCE KEY (workflow_uuid) REFERENCES workflow_status (workflow_uuid)
            DESTINATION KEY (id) REFERENCES session (id)
            LABEL "HAS_SESSION" PROPERTIES (workflow_uuid, id),
    agentiq.session_state AS "HAS_STATE" SOURCE KEY (session_id) REFERENCES session (id)
            DESTINATION KEY (id) REFERENCES session_state (id)
            LABEL "HAS_STATE" PROPERTIES (session_id, id),
    agentiq.state_delta AS "SETS" SOURCE KEY (actions_id) REFERENCES actions (id)
            DESTINATION KEY (id) REFERENCES state_delta (id)
            LABEL "SETS" PROPERTIES (actions_id, id),
    dbos.streams AS "EMITTED" SOURCE KEY (workflow_uuid) REFERENCES workflow_status (workflow_uuid)
            DESTINATION KEY (workflow_uuid, "key", "offset") REFERENCES streams (workflow_uuid, "key", "offset")
            LABEL "EMITTED" PROPERTIES (workflow_uuid, "key", "offset"),
    dbos.workflow_status AS "PARENT" SOURCE KEY (workflow_uuid) REFERENCES workflow_status (workflow_uuid)
            DESTINATION KEY (parent_workflow_id) REFERENCES workflow_status (workflow_uuid)
            LABEL "PARENT" PROPERTIES (workflow_uuid, parent_workflow_id)
  );
