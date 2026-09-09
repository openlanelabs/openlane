-- +goose Up
-- Rate-limit lookups for magic-link requests (issue #41, spec §17-2):
-- per-IP (10/hr) and per-email (5/hr) counts within the last hour.
CREATE INDEX idx_login_tokens_ip ON login_tokens(created_ip, created_at DESC);

-- Housekeeping: the app role may purge expired login tokens and long-dead
-- sessions (maintenance sweep, ~1% of requests).
GRANT DELETE ON TABLE login_tokens TO openlane_app;
GRANT DELETE ON TABLE sessions TO openlane_app;

-- +goose Down
REVOKE DELETE ON TABLE sessions FROM openlane_app;
REVOKE DELETE ON TABLE login_tokens FROM openlane_app;
DROP INDEX IF EXISTS idx_login_tokens_ip;
