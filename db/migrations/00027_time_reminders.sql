-- +goose Up
-- Auto-reminders for missing timesheets (P1 §307). Toggle only —
-- the reminder query + delivery live in the worker.
ALTER TABLE workspace_settings ADD COLUMN notify_reminders boolean NOT NULL DEFAULT true;

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION time_reminder_digest()
RETURNS TABLE (workspace_id uuid, slack_url text, names text, n int)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT ws.id, COALESCE(st.slack_webhook_url, ''),
           COALESCE(string_agg(u.display_name, ', ' ORDER BY u.display_name), ''),
           count(*)::int
    FROM workspace_settings st
    JOIN workspaces ws ON ws.id = st.workspace_id
    JOIN memberships m ON m.workspace_id = ws.id
    JOIN users u ON u.id = m.user_id
    WHERE st.notify_reminders AND COALESCE(st.slack_webhook_url, '') <> ''
      AND NOT EXISTS (
        SELECT 1 FROM time_entries te
        WHERE te.workspace_id = ws.id AND te.user_id = m.user_id
          AND te.created_at >= date_trunc('week', now()))
    GROUP BY ws.id, st.slack_webhook_url
$$;
-- +goose StatementEnd
GRANT EXECUTE ON FUNCTION time_reminder_digest() TO openlane_app;

-- +goose Down
DROP FUNCTION IF EXISTS time_reminder_digest();
ALTER TABLE workspace_settings DROP COLUMN IF EXISTS notify_reminders;
