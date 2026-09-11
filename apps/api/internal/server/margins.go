package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// Margins (P1 §324): (billed - cost)/billed per project + portfolio.
// cost = logged minutes × the author's person cost_rate; billed =
// the same minutes × the customer's rate-card price (rate resolution
// mirrors invoice generation #83). Internal only (§207).

type marginOut struct {
	ProjectID   string   `json:"project_id"`
	Name        string   `json:"name"`
	LoggableHrs float64  `json:"logged_hours"`
	Cost        float64  `json:"cost"`
	Billed      float64  `json:"billed"`
	Margin      *float64 `json:"margin"`   // null when billed = 0
	Unpriced    int      `json:"unpriced"` // minutes by authors with no person link
}

// projectMargin: shared computation under the caller's ws ctx.
// Billed counts approved+invoiced entries (the billable states);
// draft/submitted/rejected time is not yet money.
func projectMargin(ctx context.Context, tx pgx.Tx, projectID string) (*marginOut, error) {
	var out marginOut
	err := tx.QueryRow(ctx, `
		WITH pr AS (
			SELECT p.id, p.name, p.customer_id FROM projects p
			WHERE p.id = $1::uuid AND p.deleted_at IS NULL
		),
		te AS (
			SELECT te.minutes, te.user_id,
			       COALESCE((SELECT pe.cost_rate FROM people pe
			                 WHERE pe.user_id = te.user_id AND pe.deleted_at IS NULL
			                   AND pe.workspace_id = te.workspace_id
			                 LIMIT 1), 0) AS cost_rate,
			       COALESCE((SELECT rcr.hourly_rate
			                 FROM rate_card_rates rcr
			                 JOIN rate_cards rc ON rc.id = rcr.rate_card_id AND rc.is_active
			                 JOIN pr ON rc.customer_id = pr.customer_id
			                 WHERE rcr.role = m.role AND rc.workspace_id = te.workspace_id
			                 LIMIT 1),
			                (SELECT rcr.hourly_rate
			                 FROM rate_card_rates rcr
			                 JOIN rate_cards rc ON rc.id = rcr.rate_card_id AND rc.is_active
			                 WHERE rcr.role = m.role AND rc.workspace_id = te.workspace_id
			                   AND rc.customer_id IS NULL
			                 LIMIT 1), 0) AS bill_rate,
			       (SELECT count(*) FROM people pe
			        WHERE pe.user_id = te.user_id AND pe.deleted_at IS NULL
			          AND pe.workspace_id = te.workspace_id) AS priced
			FROM time_entries te
			JOIN memberships m ON m.user_id = te.user_id AND m.workspace_id = te.workspace_id
			WHERE te.project_id = (SELECT id FROM pr)
			  AND te.status IN ('approved', 'invoiced')
			  AND te.deleted_at IS NULL
		)
		SELECT pr.id::text, pr.name,
		       COALESCE(sum(te.minutes) / 60.0, 0),
		       COALESCE(sum(te.minutes * te.cost_rate / 60.0), 0),
		       COALESCE(sum(te.minutes * te.bill_rate / 60.0), 0),
		       COALESCE(sum(CASE WHEN te.priced = 0 THEN te.minutes ELSE 0 END), 0)
		FROM pr LEFT JOIN te ON true
		GROUP BY pr.id, pr.name`,
		projectID).Scan(&out.ProjectID, &out.Name, &out.LoggableHrs, &out.Cost, &out.Billed, &out.Unpriced)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, pgx.ErrNoRows
	}
	if err != nil {
		return nil, err
	}
	if out.Billed > 0 {
		m := (out.Billed - out.Cost) / out.Billed
		out.Margin = &m
	}
	return &out, nil
}

// getProjectMargin: GET /v1/projects/{id}/margin
func (s *Server) getProjectMargin(w http.ResponseWriter, r *http.Request) {
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
	out, err := projectMargin(ctx, tx, r.PathValue("id"))
	if errors.Is(err, pgx.ErrNoRows) {
		problem(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// listMargins: GET /v1/margins — portfolio rollup.
func (s *Server) listMargins(w http.ResponseWriter, r *http.Request) {
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

	// one row per project via LATERAL reuse of the shared computation
	rows, err := tx.Query(ctx, `
		SELECT p.id::text, p.name, m.logged_hours, m.cost, m.billed, m.unpriced
		FROM projects p
		CROSS JOIN LATERAL (
			WITH te AS (
				SELECT te.minutes, te.user_id,
				       COALESCE((SELECT pe.cost_rate FROM people pe
				                 WHERE pe.user_id = te.user_id AND pe.deleted_at IS NULL
				                   AND pe.workspace_id = te.workspace_id LIMIT 1), 0) AS cost_rate,
				       COALESCE((SELECT rcr.hourly_rate
				                 FROM rate_card_rates rcr
				                 JOIN rate_cards rc ON rc.id = rcr.rate_card_id AND rc.is_active
				                 WHERE rcr.role = m2.role AND rc.workspace_id = te.workspace_id
				                   AND rc.customer_id = p.customer_id LIMIT 1),
				                (SELECT rcr.hourly_rate
				                 FROM rate_card_rates rcr
				                 JOIN rate_cards rc ON rc.id = rcr.rate_card_id AND rc.is_active
				                 WHERE rcr.role = m2.role AND rc.workspace_id = te.workspace_id
				                   AND rc.customer_id IS NULL LIMIT 1), 0) AS bill_rate,
				       (SELECT count(*) FROM people pe
				        WHERE pe.user_id = te.user_id AND pe.deleted_at IS NULL
				          AND pe.workspace_id = te.workspace_id) AS priced
				FROM time_entries te
				JOIN memberships m2 ON m2.user_id = te.user_id AND m2.workspace_id = te.workspace_id
				WHERE te.project_id = p.id
				  AND te.status IN ('approved', 'invoiced')
				  AND te.deleted_at IS NULL
			)
			SELECT COALESCE(sum(minutes) / 60.0, 0) AS logged_hours,
			       COALESCE(sum(minutes * cost_rate / 60.0), 0) AS cost,
			       COALESCE(sum(minutes * bill_rate / 60.0), 0) AS billed,
			       COALESCE(sum(CASE WHEN priced = 0 THEN minutes ELSE 0 END), 0) AS unpriced
			FROM te
		) m
		WHERE p.deleted_at IS NULL
		ORDER BY m.billed DESC
		LIMIT 200`)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()

	type portfolio struct {
		Projects []marginOut `json:"projects"`
		Totals   struct {
			LoggedHours float64  `json:"logged_hours"`
			Cost        float64  `json:"cost"`
			Billed      float64  `json:"billed"`
			Margin      *float64 `json:"margin"`
		} `json:"totals"`
	}
	out := portfolio{Projects: []marginOut{}}
	for rows.Next() {
		var m marginOut
		if err := rows.Scan(&m.ProjectID, &m.Name, &m.LoggableHrs, &m.Cost, &m.Billed, &m.Unpriced); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		if m.Billed > 0 {
			mv := (m.Billed - m.Cost) / m.Billed
			m.Margin = &mv
		}
		out.Projects = append(out.Projects, m)
		out.Totals.LoggedHours += m.LoggableHrs
		out.Totals.Cost += m.Cost
		out.Totals.Billed += m.Billed
	}
	if out.Totals.Billed > 0 {
		mv := (out.Totals.Billed - out.Totals.Cost) / out.Totals.Billed
		out.Totals.Margin = &mv
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
