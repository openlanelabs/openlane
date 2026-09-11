-- +goose Up
-- Calendar import (P1 §306): source refs on time entries for
-- idempotent re-imports (UID dedupe, the §311 duplicate edge).
ALTER TABLE time_entries ADD COLUMN refs jsonb NOT NULL DEFAULT '{}';

-- one entry per calendar event per user (§311 dedupe edge): re-importing
-- the same feed inserts nothing.
CREATE UNIQUE INDEX idx_time_entries_cal_uid
    ON time_entries (workspace_id, user_id, (refs->>'calendar_uid'))
    WHERE refs->>'calendar_uid' IS NOT NULL
      AND refs->>'calendar_uid' <> '';

-- +goose Down
DROP INDEX IF EXISTS idx_time_entries_cal_uid;
ALTER TABLE time_entries DROP COLUMN IF EXISTS refs;
