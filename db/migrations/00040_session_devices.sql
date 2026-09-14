-- Device list + instant revoke (§406): sessions learn where they
-- were issued. Legacy rows stay null — the list renders them
-- 'Unknown device (pre-3.4)' honestly rather than backfilling
-- fiction.

-- +goose Up

ALTER TABLE sessions ADD COLUMN IF NOT EXISTS user_agent text;
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS created_ip text;

-- +goose Down

ALTER TABLE sessions DROP COLUMN IF EXISTS user_agent;
ALTER TABLE sessions DROP COLUMN IF EXISTS created_ip;
