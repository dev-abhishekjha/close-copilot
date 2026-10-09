-- +goose Up
CREATE TABLE close_runs (
    id uuid PRIMARY KEY,
    company_id text NOT NULL,
    month text NOT NULL,
    status text NOT NULL,
    started_at timestamptz,
    finished_at timestamptz,
    error text,
    input_tokens bigint DEFAULT 0,
    output_tokens bigint DEFAULT 0,
    cache_read_tokens bigint DEFAULT 0,
    cost_usd numeric(10,4) DEFAULT 0,
    trace_id text
);

CREATE TABLE findings (
    id uuid PRIMARY KEY,
    run_id uuid REFERENCES close_runs(id) ON DELETE CASCADE,
    type text NOT NULL,
    severity text NOT NULL,
    title text NOT NULL,
    amount_paise bigint,
    keys jsonb NOT NULL,
    evidence jsonb NOT NULL,
    explanation text,
    action text,
    citations jsonb,
    proposal jsonb,
    verified boolean NOT NULL DEFAULT false,
    status text NOT NULL DEFAULT 'open'
);

CREATE TABLE journal_proposals (
    id uuid PRIMARY KEY,
    finding_id uuid REFERENCES findings(id),
    company_id text NOT NULL,
    payload jsonb NOT NULL,
    status text NOT NULL,
    maker text NOT NULL,
    checker text,
    reason text,
    erp_name text,
    created_at timestamptz NOT NULL DEFAULT now(),
    decided_at timestamptz,
    posted_at timestamptz
);

CREATE TABLE audit_log (
    id bigserial PRIMARY KEY,
    at timestamptz NOT NULL DEFAULT now(),
    actor text NOT NULL,
    server text,
    tool text,
    args jsonb,
    result_count int,
    latency_ms int,
    error text
);

-- +goose Down
DROP TABLE IF EXISTS audit_log;
DROP TABLE IF EXISTS journal_proposals;
DROP TABLE IF EXISTS findings;
DROP TABLE IF EXISTS close_runs;
