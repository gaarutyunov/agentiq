-- +goose Up
CREATE PROPERTY GRAPH agentiq_graph
  VERTEX TABLES (
    dbos.operation_outputs KEY (workflow_uuid, function_id) LABEL step PROPERTIES (workflow_uuid, function_id, function_name, output, error),
    dbos.streams KEY (workflow_uuid, "key", "offset") LABEL stream_value PROPERTIES (workflow_uuid, "key", "offset", value, function_id),
    dbos.workflow_status KEY (workflow_uuid) LABEL workflow PROPERTIES (workflow_uuid, status, name, queue_name, created_at, updated_at, recovery_attempts, attributes, inputs, output, error)
  )
  EDGE TABLES (
    dbos.operation_outputs AS "SPAWNED" SOURCE KEY (workflow_uuid, function_id) REFERENCES operation_outputs (workflow_uuid, function_id)
            DESTINATION KEY (child_workflow_id) REFERENCES workflow_status (workflow_uuid)
            LABEL "SPAWNED" PROPERTIES (workflow_uuid, function_id, child_workflow_id),
    dbos.streams AS "EMITTED" SOURCE KEY (workflow_uuid) REFERENCES workflow_status (workflow_uuid)
            DESTINATION KEY (workflow_uuid, "key", "offset") REFERENCES streams (workflow_uuid, "key", "offset")
            LABEL "EMITTED" PROPERTIES (workflow_uuid, "key", "offset"),
    dbos.workflow_status AS "PARENT" SOURCE KEY (workflow_uuid) REFERENCES workflow_status (workflow_uuid)
            DESTINATION KEY (parent_workflow_id) REFERENCES workflow_status (workflow_uuid)
            LABEL "PARENT" PROPERTIES (workflow_uuid, parent_workflow_id)
  );

-- +goose Down
DROP PROPERTY GRAPH IF EXISTS agentiq_graph;
