package server

import (
	"context"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// Budgets (P1 §319): per-project hours, billing type, threshold alerts.
// Alerts fire from the time-entry write that crosses the band —
// real-time by construction (§319 "not nightly"). Portal never sees
// budget — internal financials (§207).

type budgetOut struct {
	BudgetHours   *int     `json:"budget_hours"`
	BillingType   string   `json:"billing_type"`
	LoggedMinutes int      `json:"logged_minutes"`
	LoggedHours   float64  `json:"logged_hours"`
	Pct           *float64 `json:"pct"`
	Status        string   `json:"status"` // unbudgeted|ok|warn50|warn80|over
}

// getProjectBudget: computed rollup for one project.
func (s *Server) getProjectBudget(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
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
	out, err := budgetRollup(ctx, tx, r.PathValue("id"))
	if err != nil {
		if err == pgx.ErrNoRows {
			problem(w, http.StatusNotFound, "project not found")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// budgetRollup: shared by the endpoint + the logTime alert path. Runs
// under the caller's ws ctx.
func budgetRollup(ctx context.Context, tx pgx.Tx, projectID string) (*budgetOut, error) {
	var out budgetOut
	var budgetHours *int
	err := tx.QueryRow(ctx, `
		SELECT budget_hours, billing_type
		FROM projects WHERE id = $1 AND deleted_at IS NULL`, projectID).
		Scan(&budgetHours, &out.BillingType)
	if err != nil {
		return nil, pgx.ErrNoRows
	}
	out.BudgetHours = budgetHours
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(minutes), 0) FROM time_entries
		WHERE project_id = $1 AND deleted_at IS NULL`, projectID).
		Scan(&out.LoggedMinutes); err != nil {
		return nil, err
	}
	out.LoggedHours = float64(out.LoggedMinutes) / 60.0
	out.Status = "unbudgeted"
	if budgetHours != nil && *budgetHours > 0 {
		pct := out.LoggedHours / float64(*budgetHours) * 100
		out.Pct = &pct
		switch {
		case pct >= 100:
			out.Status = "over"
		case pct >= 80:
			out.Status = "warn80"
		case pct >= 50:
			out.Status = "warn50"
		default:
			out.Status = "ok"
		}
	}
	return &out, nil
}

// checkBudgetAlert: post-commit hook from logTime. Opens its own short
// tx (notifyEvent will open another for settings+enqueue — see
// notify.go for why they stay separate). Fires at most one threshold
// crossing per entry write; budget_alert_level makes each band once.
func (s *Server) checkBudgetAlert(ctx context.Context, workspaceID, projectID string) {
	cctx := context.WithoutCancel(ctx)
	if workspaceID == "" {
		return
	}
	tx, err := s.pool.Begin(cctx)
	if err != nil {
		return
	}
	defer func() { _ = tx.Rollback(cctx) }()
	if _, err := tx.Exec(cctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true)", workspaceID); err != nil {
		return
	}
	var budgetHours *int
	var logged, level int
	var name string
	err = tx.QueryRow(cctx, `
		SELECT p.budget_hours, p.budget_alert_level, p.name,
		       (SELECT COALESCE(sum(te.minutes), 0) FROM time_entries te
		        WHERE te.project_id = p.id AND te.deleted_at IS NULL)
		FROM projects p WHERE p.id = $1::uuid AND p.deleted_at IS NULL`, projectID).
		Scan(&budgetHours, &level, &name, &logged)
	if err != nil || budgetHours == nil || *budgetHours <= 0 {
		return // unbudgeted: no alerts (§319)
	}
	hours := float64(logged) / 60.0
	pct := hours / float64(*budgetHours) * 100
	newLevel := 0
	switch {
	case pct >= 100:
		newLevel = 100
	case pct >= 80:
		newLevel = 80
	case pct >= 50:
		newLevel = 50
	}
	if newLevel <= level || newLevel == 0 {
		return // band already announced or still under 50%
	}
	if _, err := tx.Exec(cctx, `
		UPDATE projects SET budget_alert_level = $2, updated_at = now()
		WHERE id = $1 AND budget_alert_level < $2`, projectID, newLevel); err != nil {
		return
	}
	if err := tx.Commit(cctx); err != nil {
		return
	}
	event := "budget.warning"
	if newLevel == 100 {
		event = "budget.exceeded"
	}
	hh := int(hours)
	s.notifyEvent(cctx, workspaceID, event,
		event+" — "+name+": "+itoa(hh)+"h of "+itoa(*budgetHours)+"h logged ("+itoa(int(pct))+"%)", projectID)
}
