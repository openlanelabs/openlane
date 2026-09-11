-- +goose Up
-- Budget threshold alerts (P1 §319): budget.warning (50/80%) and
-- budget.exceeded (100%) ride the notify rail. Own settings gate —
-- money alerts are noisier than task events, opt-in by default.
ALTER TABLE workspace_settings ADD COLUMN notify_budget BOOLEAN NOT NULL DEFAULT true;

-- automations may also trigger on budget events (§346 webhook family)
ALTER TABLE automations DROP CONSTRAINT automations_trigger_event_check;
ALTER TABLE automations ADD CONSTRAINT automations_trigger_event_check CHECK (trigger_event IN (
    'task.completed','approval.requested','approval.decided',
    'csat.submitted','project.created','project.created_from_template',
    'budget.warning','budget.exceeded'));

-- +goose Down
DELETE FROM automations WHERE trigger_event IN ('budget.warning', 'budget.exceeded');
ALTER TABLE automations DROP CONSTRAINT automations_trigger_event_check;
ALTER TABLE automations ADD CONSTRAINT automations_trigger_event_check CHECK (trigger_event IN (
    'task.completed','approval.requested','approval.decided',
    'csat.submitted','project.created','project.created_from_template'));
ALTER TABLE workspace_settings DROP COLUMN IF EXISTS notify_budget;
