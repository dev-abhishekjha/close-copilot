-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'copilot_app') THEN
        CREATE ROLE copilot_app NOLOGIN;
    END IF;
END
$$;
-- +goose StatementEnd

GRANT SELECT, INSERT ON audit_log TO copilot_app;
GRANT USAGE ON SEQUENCE audit_log_id_seq TO copilot_app;
REVOKE UPDATE, DELETE, TRUNCATE ON audit_log FROM copilot_app;

-- +goose Down
REVOKE USAGE ON SEQUENCE audit_log_id_seq FROM copilot_app;
REVOKE SELECT, INSERT ON audit_log FROM copilot_app;
