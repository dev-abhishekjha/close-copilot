-- +goose Up
-- Runtime data plane (CC-709): steps, content-addressed artifacts and LLM
-- calls, so a crashed run resumes where it stopped and any finding can be
-- rebuilt for audit from exactly what the model saw.
CREATE TABLE run_steps (
    id uuid PRIMARY KEY,
    run_id uuid NOT NULL REFERENCES close_runs(id) ON DELETE CASCADE,
    kind text NOT NULL,      -- router | check.<name> | retrieve | explain | verify | investigate | synthesize
    subject text NOT NULL,   -- check name or finding id
    status text NOT NULL,    -- pending | running | done | failed | skipped
    attempt int NOT NULL DEFAULT 0,
    input_refs text[] NOT NULL DEFAULT '{}',   -- artifact sha256s
    output_refs text[] NOT NULL DEFAULT '{}',
    feedback jsonb,          -- verifier violations handed to the next attempt
    started_at timestamptz,
    finished_at timestamptz,
    error text,
    UNIQUE (run_id, kind, subject)
);

CREATE TABLE artifacts (   -- content-addressed
    sha256 text PRIMARY KEY,
    kind text NOT NULL,      -- tool_result | retrieval | prompt | response | explanation | report
    run_id uuid REFERENCES close_runs(id),
    produced_by uuid REFERENCES run_steps(id),
    content jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE llm_calls (   -- Langfuse keeps timings only; this keeps what was said
    id uuid PRIMARY KEY,
    step_id uuid NOT NULL REFERENCES run_steps(id),
    model text NOT NULL,
    prompt_sha256 text NOT NULL REFERENCES artifacts(sha256),
    response_sha256 text NOT NULL REFERENCES artifacts(sha256),
    input_tokens int,
    output_tokens int,
    cache_read_tokens int,
    cost_usd numeric(10,6),
    latency_ms int,
    created_at timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
-- Development only: dropping these tables destroys the audit trail (every
-- step, snapshot, prompt and response a finding is rebuilt from). Never run
-- this against a database whose runs must stay auditable.
DROP TABLE IF EXISTS llm_calls;
DROP TABLE IF EXISTS artifacts;
DROP TABLE IF EXISTS run_steps;
