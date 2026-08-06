-- +goose Up
CREATE TABLE agentiq.actions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id uuid NOT NULL,
    skip_summarization boolean,
    transfer_to_agent text,
    escalate boolean,
    requested_auth_configs json,
    CONSTRAINT actions_key UNIQUE (event_id)
);

CREATE TABLE agentiq.artifact_delta (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actions_id uuid NOT NULL,
    event_id uuid NOT NULL,
    filename text NOT NULL,
    version integer NOT NULL,
    CONSTRAINT artifact_delta_key UNIQUE (event_id, filename)
);

CREATE TABLE agentiq.event (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    adk_id text NOT NULL,
    session_id uuid NOT NULL,
    sequence integer NOT NULL,
    invocation_id text NOT NULL,
    author text NOT NULL,
    branch text,
    timestamp timestamptz NOT NULL,
    turn_complete boolean NOT NULL,
    interrupted boolean NOT NULL,
    isolation_scope text,
    error_code text,
    error_message text,
    content_role text,
    long_running_tool_ids text[],
    requested_input json,
    routes json,
    node_info json,
    grounding_metadata json,
    usage_metadata json,
    citation_metadata json,
    custom_metadata json,
    step_function_id integer,
    workflow_uuid text,
    CONSTRAINT event_key UNIQUE (session_id, adk_id)
);

CREATE TABLE agentiq.part (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    event_id uuid NOT NULL,
    part_index integer NOT NULL,
    text text,
    thought boolean,
    thought_signature bytea,
    function_call_id text,
    function_call_name text,
    function_call_args json,
    function_response_id text,
    function_response_name text,
    function_response_response json,
    inline_data_mime_type text,
    inline_data_bytes bytea,
    inline_data_display_name text,
    file_data_mime_type text,
    file_data_uri text,
    file_data_display_name text,
    executable_code_language text,
    executable_code_code text,
    code_execution_outcome text,
    code_execution_output text,
    video_metadata_start_offset text,
    video_metadata_end_offset text,
    video_metadata_fps double precision,
    audio_transcription json,
    media_resolution text,
    tool_call json,
    tool_response json,
    part_metadata json,
    CONSTRAINT part_key UNIQUE (event_id, part_index)
);

CREATE TABLE agentiq.session (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    adk_id text NOT NULL,
    app_name text NOT NULL,
    user_id text NOT NULL,
    workflow_uuid text,
    agent_digest text NOT NULL,
    created_at_ts timestamptz NOT NULL,
    last_update_at timestamptz NOT NULL,
    CONSTRAINT session_key UNIQUE (app_name, user_id, adk_id)
);

CREATE TABLE agentiq.session_state (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    session_id uuid NOT NULL,
    scope text NOT NULL,
    "key" text NOT NULL,
    value_json json NOT NULL,
    updated_at_ts timestamptz NOT NULL,
    CONSTRAINT session_state_key UNIQUE (session_id, scope, "key")
);

CREATE TABLE agentiq.state_delta (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    actions_id uuid NOT NULL,
    event_id uuid NOT NULL,
    scope text NOT NULL,
    "key" text NOT NULL,
    value_json json NOT NULL,
    CONSTRAINT state_delta_key UNIQUE (event_id, "key")
);

-- +goose Down
DROP TABLE IF EXISTS agentiq.actions;

DROP TABLE IF EXISTS agentiq.artifact_delta;

DROP TABLE IF EXISTS agentiq.event;

DROP TABLE IF EXISTS agentiq.part;

DROP TABLE IF EXISTS agentiq.session;

DROP TABLE IF EXISTS agentiq.session_state;

DROP TABLE IF EXISTS agentiq.state_delta;
