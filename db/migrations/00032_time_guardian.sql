-- +goose Up
-- Time Guardian (P2 §15.5 + §307/308): read-only policy scan of
-- yesterday. Guardian flags, never auto-fixes. Cross-workspace system
-- scan → SECURITY DEFINER (the time_reminder_digest precedent).

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION time_guardian_digest(day date)
RETURNS TABLE (workspace_id uuid, slack_url text, flags jsonb)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT ws.id, COALESCE(st.slack_webhook_url, ''),
           jsonb_agg(jsonb_build_object('kind', f.kind, 'who', f.who, 'detail', f.detail)
                     ORDER BY f.kind, f.who)
    FROM workspace_settings st
    JOIN workspaces ws ON ws.id = st.workspace_id
    JOIN LATERAL (
        -- missing-day: active member with a linked user + capacity who
        -- logged nothing on a weekday (§307 'flags gaps')
        SELECT 'missing_day' AS kind, u.display_name AS who,
               format('%s logged no time on %s', u.display_name, to_char($1, 'Dy Mon DD')) AS detail
        FROM people p
        JOIN users u ON u.id = p.user_id
        WHERE p.workspace_id = ws.id AND p.active
          AND p.capacity_hrs > 0 AND p.deleted_at IS NULL
          AND u.id IN (SELECT user_id FROM memberships WHERE workspace_id = ws.id)
          AND EXTRACT(ISODOW FROM $1) < 6
          AND NOT EXISTS (
              SELECT 1 FROM time_entries te
              WHERE te.workspace_id = ws.id AND te.user_id = p.user_id
                AND te.deleted_at IS NULL
                AND te.started_at::date = $1)
        UNION ALL
        -- over-max: >12h in the day (§308). Single entries cap at
        -- 720m (CHECK), so >12h is only visible across multiple logs.
        SELECT 'over_max', u.display_name,
               format('%s logged %sh on %s (max 12h)', u.display_name,
                      round(sum(te.minutes) / 60.0), to_char($1, 'Dy Mon DD'))
        FROM time_entries te
        JOIN users u ON u.id = te.user_id
        WHERE te.workspace_id = ws.id AND te.deleted_at IS NULL
          AND te.started_at::date = $1
        GROUP BY u.display_name
        HAVING sum(te.minutes) > 12 * 60
    ) f ON true
    WHERE COALESCE(st.slack_webhook_url, '') <> ''
      AND COALESCE(st.agents_enabled, true)
    GROUP BY ws.id, st.slack_webhook_url
$$;
-- +goose StatementEnd
GRANT EXECUTE ON FUNCTION time_guardian_digest(date) TO openlane_app;

-- +goose Down
DROP FUNCTION IF EXISTS time_guardian_digest(date);
