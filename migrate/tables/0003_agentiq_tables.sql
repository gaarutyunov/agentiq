-- +goose Up
ALTER TABLE agentiq.part DROP COLUMN thought_signature;

ALTER TABLE agentiq.part DROP COLUMN inline_data_bytes;

ALTER TABLE agentiq.part ADD COLUMN thought_signature_b64 text;

ALTER TABLE agentiq.part ADD COLUMN inline_data_bytes_b64 text;

-- +goose Down
ALTER TABLE agentiq.part DROP COLUMN thought_signature_b64;

ALTER TABLE agentiq.part DROP COLUMN inline_data_bytes_b64;

ALTER TABLE agentiq.part ADD COLUMN thought_signature bytea;

ALTER TABLE agentiq.part ADD COLUMN inline_data_bytes bytea;
