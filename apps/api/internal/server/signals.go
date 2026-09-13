package server

import (
	"fmt"
	"net/http"
	"strings"
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
		-- margin_dip moved to Go: appended post-query from the
		-- canonical marginRows() so it can never drift from
		-- /margins or the Finance Guardian (and stays parametrized —
		-- no data ever interpolates into SQL text).
		SELECT 'margin_dip' AS kind, 'red' AS severity, ''::text AS project_id, '' AS project,
		       '' AS title, '{}'::jsonb AS evidence
		WHERE false

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

	// margin_dip: appended from the canonical marginRows() — the same
	// math as /margins and the Finance Guardian (the EPOCH-hours math
	// this replaces billed ended-started spans, which silently diverged
	// from the minutes-based billing truth).
	if mrows, merr := marginRows(ctx, tx); merr == nil {
		for _, m := range mrows {
			if m.Billed <= 0 {
				continue
			}
			pct := int((m.Billed - m.Cost) / m.Billed * 100)
			if pct >= 20 {
				continue
			}
			out = append(out, signalOut{Kind: "margin_dip", Severity: "red",
				ProjectID: m.ProjectID, Project: m.Name,
				Title:    fmt.Sprintf("Margin at %d%% (billed %.2f, cost %.2f)", pct, m.Billed, m.Cost),
				Evidence: mustJSON(map[string]any{"billed": m.Billed, "cost": m.Cost, "margin_pct": pct})})
		}
	}

	// ?narrate=1 (manager+, P2 §15.6): one cheap-model LLM pass that
	// drafts a two-sentence portfolio note. Deterministic titles stay —
	// the LLM never replaces evidence. No config / provider failure →
	// degrade silently (narrative omitted), agent_run records the miss.
	narrative := ""
	if r.URL.Query().Get("narrate") == "1" && len(out) > 0 {
		var lines []string
		for i, sig := range out {
			if i >= 10 {
				break
			}
			lines = append(lines, sig.Severity+" "+sig.Kind+" — "+sig.Project+": "+sig.Title)
		}
		if cfg, err := loadLLMConfig(ctx, tx); err == nil {
			prompt := "Summarize for a delivery lead, max 2 sentences, no preamble, " +
				"say what to act on first:\n" + strings.Join(lines, "\n")
			if res, err := llmComplete(ctx, cfg, "cheap",
				"You summarize project delivery risk signals for a professional services automation tool.",
				prompt); err == nil {
				narrative = res.Text
				if _, err := tx.Exec(ctx, `
					INSERT INTO agent_runs (workspace_id, agent, status, model, input_ref, output_ref, cost_cents, finished_at)
					VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'signals', 'succeeded', $1,
					        'signals:narrate', left($2, 200), $3, now())`,
					res.Model, narrative, llmCostCents(res)); err != nil {
					problem(w, http.StatusInternalServerError, "internal error")
					return
				}
			}
		}
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
	writeJSON(w, http.StatusOK, map[string]any{"signals": out, "narrative": narrative})
}

func countOut(n int) string {
	if n == 1 {
		return "1 signal"
	}
	return itoa(n) + " signals"
}
