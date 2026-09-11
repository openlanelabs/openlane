-- +goose Up
-- Timesheet approvals (P1 §307): draft→submitted→approved/rejected.
-- reject_reason is manager-authored; note stays user-authored.

ALTER TABLE time_entries ADD COLUMN reject_reason text;

-- +goose Down
ALTER TABLE time_entries DROP COLUMN IF EXISTS reject_reason;
