-- +goose Up
CREATE TABLE agentiq.agent (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    digest text NOT NULL,
    name text NOT NULL,
    description text,
    model text NOT NULL,
    instruction text NOT NULL,
    skills json NOT NULL,
    tools json NOT NULL,
    sub_agents json NOT NULL,
    created_at_ts timestamptz NOT NULL,
    CONSTRAINT agent_key UNIQUE (digest)
);

CREATE TABLE agentiq.workflow_agent (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workflow_uuid text NOT NULL,
    agent_digest text NOT NULL,
    pinned_at_ts timestamptz NOT NULL,
    CONSTRAINT workflow_agent_key UNIQUE (workflow_uuid)
);

-- +goose Down
DROP TABLE IF EXISTS agentiq.agent;

DROP TABLE IF EXISTS agentiq.workflow_agent;
