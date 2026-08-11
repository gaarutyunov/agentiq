-- +goose Up
ALTER TABLE agentiq.state_delta DROP CONSTRAINT state_delta_key;

ALTER TABLE agentiq.state_delta ADD CONSTRAINT state_delta_key UNIQUE (event_id, scope, "key");

-- +goose Down
ALTER TABLE agentiq.state_delta DROP CONSTRAINT state_delta_key;

ALTER TABLE agentiq.state_delta ADD CONSTRAINT state_delta_key UNIQUE (event_id, "key");
