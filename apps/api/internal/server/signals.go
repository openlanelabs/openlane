package server

import (
	"net/http"
)

// Signals v1 (P2 §15.6 + §14): deterministic rules, each signal with a
// story + evidence links ('Every signal has evidence links, not just
// score'). Computed on read — the data is small; ML ranking and LLM
// narration arrive with the BYO-LLM router.

type signalOut struct {
	Kind      string `json:"kind"`
	Severity  string `json:"severity"` // red | warn
	ProjectID string `json:"project_id"`
	Project   string `json:"project"`
	Title     string `json:"title"`
	Evidence  []byte `json:"evidence"`
}

func (s *Server) listSignals(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true), set_config('app.user_id', $2, true)",
		workspaceFromCtx(ctx), userFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	var enabled bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT agents_enabled FROM workspace_settings
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid), true)`).
		Scan(&enabled); err != nil || !enabled {
		problem(w, http.StatusServiceUnavailable, "agents disabled for this workspace (kill switch)")
		return
	}

	rows, err := tx.Query(ctx, `
	WITH logged AS (
		SELECT project_id, sum(minutes) AS mins
		FROM time_entries WHERE deleted_at IS NULL GROUP BY project_id
	)
	SELECT kind, severity, project_id::text, project, title, evidence FROM (
		-- portal_silent: live portal link unused 7d+ AND overdue customer work
		SELECT 'portal_silent' AS kind, 'red' AS severity, p.id::text AS project_id, p.name AS project,
		       format('Customer hasn''t used the portal in %sd and has %s overdue customer task(s)',
		              COALESCE(EXTRACT(DAY FROM now() - pl.last_used_at)::int, 999),
		              count(t.id)) AS title,
		       jsonb_build_object('link_id', pl.id::text, 'last_used_at', pl.last_used_at::text,
		                          'overdue_task_ids', array_agg(t.id::text)) AS evidence
		FROM projects p
		JOIN portal_links pl ON pl.project_id = p.id AND pl.status = 'active'
		JOIN tasks t ON t.project_id = p.id AND t.deleted_at IS NULL
		    AND t.customer_visible AND t.due_at < now()
		    AND t.status NOT IN ('done','waived')
		WHERE p.deleted_at IS NULL AND p.status = 'active'
		  AND (pl.last_used_at IS NULL OR pl.last_used_at < now() - interval '7 days')
		GROUP BY p.id, p.name, pl.id, pl.last_used_at

		UNION ALL
		-- overdue_cluster: 3+ overdue customer tasks on one project
		SELECT 'overdue_cluster', 'red', p.id::text, p.name,
		       format('%s overdue customer tasks on this project', count(t.id)),
		       jsonb_build_object('overdue_task_ids', array_agg(t.id::text))
		FROM projects p
		JOIN tasks t ON t.project_id = p.id AND t.deleted_at IS NULL
		    AND t.customer_visible AND t.due_at < now()
		    AND t.status NOT IN ('done','waived')
		WHERE p.deleted_at IS NULL AND p.status = 'active'
		GROUP BY p.id, p.name
		HAVING count(t.id) >= 3

		UNION ALL
		-- budget_burn: logged ≥80% of budget hours (red at 100%)
		SELECT 'budget_burn',
		       CASE WHEN COALESCE(l.mins,0) >= p.budget_hours * 60 THEN 'red' ELSE 'warn' END,
		       p.id::text, p.name,
		       format('%sh of %sh budget logged (%s%%)',
		              round(COALESCE(l.mins,0)/60.0), p.budget_hours,
		              round(100.0 * COALESCE(l.mins,0) / (p.budget_hours * 60))),
		       jsonb_build_object('logged_minutes', COALESCE(l.mins,0), 'budget_hours', p.budget_hours)
		FROM projects p
		LEFT JOIN logged l ON l.project_id = p.id
		WHERE p.deleted_at IS NULL AND p.status = 'active'
		  AND p.budget_hours IS NOT NULL AND p.budget_hours > 0
		  AND COALESCE(l.mins,0) >= p.budget_hours * 60 * 0.8

		UNION ALL
		-- margin_dip: §324 margin under 20% where money has moved
		SELECT 'margin_dip', 'red', m.project_id::text, m.name,
		       format('Margin at %s%% (billed %s, cost %s)',
		              m.margin_pct, m.billed, m.cost),
		       jsonb_build_object('billed', m.billed, 'cost', m.cost, 'margin_pct', m.margin_pct)
		FROM (
		    SELECT pr.id AS project_id, pr.name,
		           sum(EXTRACT(EPOCH FROM (te.ended_at - te.started_at))/3600 * rc.hourly_rate)::numeric(12,2) AS billed,
		           sum(EXTRACT(EPOCH FROM (te.ended_at - te.started_at))/3600 * COALESCE(pp.cost_rate, 0))::numeric(12,2) AS cost,
		           round(100.0 * (sum(EXTRACT(EPOCH FROM (te.ended_at - te.started_at))/3600 * rc.hourly_rate)
		                          - sum(EXTRACT(EPOCH FROM (te.ended_at - te.started_at))/3600 * COALESCE(pp.cost_rate, 0)))
		                 / NULLIF(sum(EXTRACT(EPOCH FROM (te.ended_at - te.started_at))/3600 * rc.hourly_rate), 0))::int AS margin_pct
		    FROM projects pr
		    JOIN time_entries te ON te.project_id = pr.id
		         AND te.status IN ('approved','invoiced') AND te.deleted_at IS NULL
		    LEFT JOIN people pp ON pp.user_id = te.user_id AND pp.workspace_id = te.workspace_id
		    LEFT JOIN LATERAL (
		        SELECT rcr.hourly_rate
		        FROM rate_card_rates rcr
		        JOIN rate_cards rc ON rc.id = rcr.rate_card_id AND rc.is_active
		        WHERE rcr.role = COALESCE(pp.role, '')
		          AND rc.workspace_id = te.workspace_id
		          AND (rc.customer_id = pr.customer_id OR rc.customer_id IS NULL)
		        ORDER BY rc.customer_id NULLS LAST
		        LIMIT 1
		    ) rc ON true
		    WHERE pr.deleted_at IS NULL
		    GROUP BY pr.id, pr.name
		    HAVING sum(EXTRACT(EPOCH FROM (te.ended_at - te.started_at))/3600 * rc.hourly_rate) > 0
		) m
		WHERE m.margin_pct < 20

		UNION ALL
		-- approval_stale: pending approval > 7 days
		SELECT 'approval_stale', 'warn', a.project_id::text, p.name,
		       format('Approval "%s" pending %sd', a.title,
		              EXTRACT(DAY FROM now() - a.created_at)::int),
		       jsonb_build_object('approval_id', a.id::text, 'created_at', a.created_at::text)
		FROM approvals a
		JOIN projects p ON p.id = a.project_id
		WHERE a.status = 'pending' AND a.deleted_at IS NULL
		  AND a.created_at < now() - interval '7 days'
	) x
	ORDER BY CASE severity WHEN 'red' THEN 0 ELSE 1 END, project
	LIMIT 100`)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []signalOut{}
	for rows.Next() {
		var sig signalOut
		if err := rows.Scan(&sig.Kind, &sig.Severity, &sig.ProjectID, &sig.Project, &sig.Title, &sig.Evidence); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		out = append(out, sig)
	}
	if err := rows.Err(); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_runs (workspace_id, agent, status, model, input_ref, output_ref, finished_at)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'signals', 'succeeded', 'none',
		        'signals', $1, now())`,
		countOut(len(out))); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"signals": out})
}

func countOut(n int) string {
	if n == 1 {
		return "1 signal"
	}
	return itoa(n) + " signals"
}
