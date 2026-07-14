CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE accounts (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name        text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE api_keys (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id    uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    key_hash      text NOT NULL UNIQUE,
    label         text NOT NULL DEFAULT '',
    scopes        text[] NOT NULL DEFAULT '{}',
    created_at    timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz,
    revoked_at    timestamptz
);

CREATE TABLE nodes (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name               text NOT NULL,
    isp                text NOT NULL DEFAULT '',
    city               text NOT NULL DEFAULT '',
    country            text NOT NULL DEFAULT 'TM',
    agent_version      text NOT NULL DEFAULT '',
    last_heartbeat_at  timestamptz,
    secret_hash        text NOT NULL,
    tags               jsonb NOT NULL DEFAULT '[]',
    metadata           jsonb NOT NULL DEFAULT '{}',
    created_at         timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_nodes_last_heartbeat_at ON nodes(last_heartbeat_at);

CREATE TYPE check_type AS ENUM ('ping', 'tcp', 'http', 'dns', 'tls', 'traceroute');
CREATE TYPE check_status AS ENUM ('pending', 'running', 'completed', 'partial', 'failed', 'cancelled');
CREATE TYPE check_run_status AS ENUM ('queued', 'dispatched', 'running', 'done', 'error', 'timeout');

CREATE TABLE checks (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    account_id     uuid NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    batch_id       uuid,
    type           check_type NOT NULL,
    target         text NOT NULL,
    params         jsonb NOT NULL DEFAULT '{}',
    node_selector  jsonb NOT NULL DEFAULT '{}',
    callback_url   text,
    status         check_status NOT NULL DEFAULT 'pending',
    warnings       text[] NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL DEFAULT now(),
    completed_at   timestamptz
);

CREATE INDEX idx_checks_account_id ON checks(account_id);
CREATE INDEX idx_checks_batch_id ON checks(batch_id);
CREATE INDEX idx_checks_status ON checks(status);

CREATE TABLE check_runs (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    check_id       uuid NOT NULL REFERENCES checks(id) ON DELETE CASCADE,
    node_id        uuid NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
    status         check_run_status NOT NULL DEFAULT 'queued',
    dispatched_at  timestamptz,
    completed_at   timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_check_runs_check_id ON check_runs(check_id);
CREATE INDEX idx_check_runs_node_poll ON check_runs(node_id, status);

CREATE TABLE results (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    check_run_id   uuid NOT NULL REFERENCES check_runs(id) ON DELETE CASCADE,
    success        boolean NOT NULL,
    latency_ms     integer,
    status_code    text,
    error_message  text,
    raw            jsonb NOT NULL DEFAULT '{}',
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX idx_results_check_run_id ON results(check_run_id);
