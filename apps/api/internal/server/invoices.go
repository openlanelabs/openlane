package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Invoice drafts (P1 §319-§328). Line items are JSONB snapshots of
// (task, role, minutes, rate, amount) computed at generation — the invoice
// is a frozen record, later edits to rate cards don't rewrite history.

type invoiceOut struct {
	ID          string          `json:"id"`
	CustomerID  string          `json:"customer_id"`
	Status      string          `json:"status"`
	PeriodStart string          `json:"period_start"`
	PeriodEnd   string          `json:"period_end"`
	LineItems   json.RawMessage `json:"line_items"`
	Subtotal    string          `json:"subtotal"`
	Notes       *string         `json:"notes"`
	CreatedAt   string          `json:"created_at"`
}

const invoiceCols = `id, customer_id, status, period_start::text, period_end::text, line_items, subtotal, notes, created_at`

type invoiceRow struct {
	out invoiceOut
}

// scanInvoice works for pgx.Row and pgx.Rows (both have Scan).
func scanInvoice[T interface{ Scan(dest ...any) error }](row T) (invoiceRow, error) {
	var r invoiceRow
	var created time.Time
	var notes *string
	err := row.Scan(&r.out.ID, &r.out.CustomerID, &r.out.Status, &r.out.PeriodStart,
		&r.out.PeriodEnd, &r.out.LineItems, &r.out.Subtotal, &notes, &created)
	if err != nil {
		return r, err
	}
	r.out.Notes = notes
	r.out.CreatedAt = created.UTC().Format(time.RFC3339)
	return r, nil
}

// generateInvoice: POST /v1/customers/{id}/invoices/generate
// Builds a draft from approved time entries in [start, end], priced by the
// customer's active rate card (fallback: the workspace default card), and
// marks the consumed entries invoiced in the same tx — double-billing is
// structurally impossible.
func (s *Server) generateInvoice(w http.ResponseWriter, r *http.Request) {
	customerID := r.PathValue("id")
	var req struct {
		PeriodStart string  `json:"period_start"`
		PeriodEnd   string  `json:"period_end"`
		Notes       *string `json:"notes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if _, err := time.Parse("2006-01-02", req.PeriodStart); err != nil {
		problem(w, http.StatusBadRequest, "period_start must be YYYY-MM-DD")
		return
	}
	if _, err := time.Parse("2006-01-02", req.PeriodEnd); err != nil {
		problem(w, http.StatusBadRequest, "period_end must be YYYY-MM-DD")
		return
	}
	if req.PeriodEnd < req.PeriodStart {
		problem(w, http.StatusBadRequest, "period_end must be >= period_start")
		return
	}

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

	var unbilled int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM time_entries te
		JOIN projects p ON p.id = te.project_id
		WHERE te.workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
		  AND te.status = 'approved'
		  AND te.started_at::date BETWEEN $1 AND $2
		  AND p.customer_id = $3::uuid`,
		req.PeriodStart, req.PeriodEnd, customerID).Scan(&unbilled); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if unbilled == 0 {
		problem(w, http.StatusNotFound, "no unbilled approved time in this period")
		return
	}

	// ensure every billed role has a rate; report the first gap
	var gap string
	if err := tx.QueryRow(ctx, `
		SELECT DISTINCT m.role FROM time_entries te
		JOIN projects p ON p.id = te.project_id AND p.customer_id = $1::uuid
		JOIN users u ON u.id = te.user_id
		JOIN memberships m ON m.user_id = u.id AND m.workspace_id = te.workspace_id
		WHERE te.workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
		  AND te.status = 'approved'
		  AND te.started_at::date BETWEEN $2 AND $3
		  AND NOT EXISTS (
		    SELECT 1 FROM rate_card_rates rcr
		    JOIN rate_cards rc ON rc.id = rcr.rate_card_id AND rc.is_active
		      AND (rc.customer_id = p.customer_id OR rc.customer_id IS NULL)
		    WHERE rcr.role = m.role
		      AND (rc.customer_id IS NOT NULL OR NOT EXISTS (
		        SELECT 1 FROM rate_cards c2 JOIN rate_card_rates r2 ON r2.rate_card_id = c2.id
		        WHERE c2.workspace_id = te.workspace_id AND c2.customer_id = p.customer_id
		          AND c2.is_active AND r2.role = m.role))
		    ORDER BY rcr.hourly_rate DESC
		    LIMIT 1)
		ORDER BY m.role LIMIT 1`,
		customerID, req.PeriodStart, req.PeriodEnd).Scan(&gap); err == nil && gap != "" {
		problem(w, http.StatusBadRequest, "no rate for role \""+gap+"\" — add it to the customer or default rate card")
		return
	}

	inv, err := scanInvoice(tx.QueryRow(ctx, `
		WITH billed AS (
			SELECT te.id AS entry_id, te.task_id::text, m.role, te.minutes,
			       COALESCE((SELECT rcr.hourly_rate
			                 FROM rate_card_rates rcr
			                 JOIN rate_cards rc ON rc.id = rcr.rate_card_id AND rc.is_active
			                 WHERE rcr.role = m.role AND rc.workspace_id = te.workspace_id
			                   AND rc.customer_id = p.customer_id
			         ), (SELECT rcr.hourly_rate
			             FROM rate_card_rates rcr
			             JOIN rate_cards rc ON rc.id = rcr.rate_card_id AND rc.is_active
			             WHERE rcr.role = m.role AND rc.workspace_id = te.workspace_id
			               AND rc.customer_id IS NULL
			         )) AS rate
			FROM time_entries te
			JOIN projects p ON p.id = te.project_id AND p.customer_id = $1::uuid
			JOIN memberships m ON m.user_id = te.user_id AND m.workspace_id = te.workspace_id
			WHERE te.workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
			  AND te.status = 'approved'
			  AND te.started_at::date BETWEEN $2 AND $3
		),
		lines AS (
			SELECT jsonb_agg(jsonb_build_object(
			           'task_id', task_id, 'role', role, 'minutes', minutes,
			           'rate', rate, 'amount', round(minutes * rate / 60.0, 2))) AS items,
			       round(sum(minutes * rate / 60.0), 2) AS subtotal
			FROM billed
		),
		ins AS (
			INSERT INTO invoices (workspace_id, customer_id, period_start, period_end, line_items, subtotal, notes, created_by)
			SELECT NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1::uuid, $2, $3,
			       lines.items, lines.subtotal, $4,
			       NULLIF(current_setting('app.user_id', true), '')::uuid
			FROM lines
			RETURNING `+invoiceCols+`
		),
		mark AS (
			UPDATE time_entries te SET status = 'invoiced', updated_at = now()
			FROM projects p
			WHERE p.id = te.project_id AND p.customer_id = $1::uuid
			  AND te.workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
			  AND te.status = 'approved'
			  AND te.started_at::date BETWEEN $2 AND $3
			RETURNING 1
		)
		SELECT `+invoiceCols+` FROM ins`,
		customerID, req.PeriodStart, req.PeriodEnd, req.Notes))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			problem(w, http.StatusBadRequest, "customer not found in this workspace")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'invoice', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'invoice.created', 'api', $2)`,
		inv.out.ID, `{"customer_id":"`+inv.out.CustomerID+`","period":"`+inv.out.PeriodStart+`..`+inv.out.PeriodEnd+`"}`); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, inv.out)
}

// listInvoices: GET /v1/invoices
func (s *Server) listInvoices(w http.ResponseWriter, r *http.Request) {
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
	q := r.URL.Query()
	status, customer := q.Get("status"), q.Get("customer_id")
	rows, err := tx.Query(ctx, `
		SELECT `+invoiceCols+` FROM invoices
		WHERE deleted_at IS NULL
		  AND ($1 = '' OR status = $1)
		  AND ($2 = '' OR customer_id = $2::uuid)
		ORDER BY created_at DESC LIMIT 200`,
		status, customer)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	var out []invoiceOut
	for rows.Next() {
		inv, err := scanInvoice(rows)
		if err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		out = append(out, inv.out)
	}
	if out == nil {
		out = []invoiceOut{}
	}
	writeJSON(w, http.StatusOK, out)
}

// getInvoice: GET /v1/invoices/{id}
func (s *Server) getInvoice(w http.ResponseWriter, r *http.Request) {
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
	inv, err := scanInvoice(tx.QueryRow(ctx, `
		SELECT `+invoiceCols+` FROM invoices WHERE id = $1::uuid AND deleted_at IS NULL`, r.PathValue("id")))
	if errors.Is(err, pgx.ErrNoRows) {
		problem(w, http.StatusNotFound, "invoice not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, inv.out)
}

// patchInvoice: PATCH /v1/invoices/{id} — status transitions only in v1:
// draft→sent, sent→paid, draft|sent|paid→void. Notes editable while draft.
func (s *Server) patchInvoice(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Status *string `json:"status"`
		Notes  *string `json:"notes"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.Status == nil && req.Notes == nil {
		problem(w, http.StatusBadRequest, "nothing to update")
		return
	}
	if req.Status != nil {
		switch *req.Status {
		case "sent", "paid", "void":
		default:
			problem(w, http.StatusBadRequest, "status must be sent|paid|void")
			return
		}
	}

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

	var cur string
	err = tx.QueryRow(ctx, `SELECT status FROM invoices WHERE id = $1::uuid AND deleted_at IS NULL`,
		r.PathValue("id")).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		problem(w, http.StatusNotFound, "invoice not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	if req.Status != nil {
		allowed := map[string][]string{
			"draft": {"sent", "void"},
			"sent":  {"paid", "void"},
			"paid":  {"void"},
			"void":  {},
		}
		ok := false
		for _, s2 := range allowed[cur] {
			if s2 == *req.Status {
				ok = true
			}
		}
		if !ok {
			problem(w, http.StatusBadRequest, "invalid transition "+cur+"→"+*req.Status)
			return
		}
	}
	if req.Notes != nil && cur != "draft" {
		problem(w, http.StatusBadRequest, "notes locked once sent")
		return
	}

	inv, err := scanInvoice(tx.QueryRow(ctx, `
		UPDATE invoices SET
		  status = COALESCE($2, status),
		  notes = COALESCE($3, notes),
		  updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING `+invoiceCols,
		r.PathValue("id"), req.Status, req.Notes))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	action := "invoice.updated"
	if req.Status != nil {
		action = "invoice." + *req.Status
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'invoice', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, $2, 'api', $3)`,
		inv.out.ID, action, `{"status":"`+inv.out.Status+`"}`); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, inv.out)
}
