-- +goose Up
-- Budgets (P1 §319): per-project hours + billing type. Alerts at
-- 50/80/100% fire real-time from the time-entry write that crosses
-- the threshold — no cron (§319 "real-time, not nightly").
-- budget_alert_level tracks the highest threshold already announced
-- (0 = none, 50, 80, 100) so each band fires exactly once. Reset to 0
-- when budget grows past the logged hours again.

ALTER TABLE projects ADD COLUMN budget_hours INT CHECK (budget_hours > 0),
                     ADD COLUMN billing_type TEXT NOT NULL DEFAULT 'tm'
                        CHECK (billing_type IN ('tm','fixed','retainer')),
                     ADD COLUMN budget_alert_level INT NOT NULL DEFAULT 0
                        CHECK (budget_alert_level IN (0, 50, 80, 100));

-- +goose Down
ALTER TABLE projects DROP COLUMN IF EXISTS budget_alert_level,
                    DROP COLUMN IF EXISTS billing_type,
                    DROP COLUMN IF EXISTS budget_hours;
