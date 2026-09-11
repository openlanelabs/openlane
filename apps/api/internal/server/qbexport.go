package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// QuickBooks CSV export (P1 §322): QB Online imports CSVs
// (time-activity / invoice import). v1 = files an operator uploads;
// OAuth push sync is a later phase. stdlib encoding/csv only —
// hand-rolled escaping is how the audit-JSON bug happened.

// qbLine mirrors QB's invoice-import column order.
type qbLine struct {
	Customer    string
	InvoiceNo   string
	Date        string
	DueDate     string
	Description string
	Qty         string
	Rate        string
	Amount      string
}

func writeQBCSV(w http.ResponseWriter, filename string, lines []qbLine) {
	var buf bytes.Buffer
	cw := csv.NewWriter(&buf)
	_ = cw.Write([]string{"Customer", "InvoiceNo", "Date", "DueDate", "Description", "Qty", "Rate", "Amount"})
	for _, l := range lines {
		_ = cw.Write([]string{l.Customer, l.InvoiceNo, l.Date, l.DueDate, l.Description, l.Qty, l.Rate, l.Amount})
	}
	cw.Flush()
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	_, _ = w.Write(buf.Bytes())
}

// exportInvoice: GET /v1/invoices/{id}/export
func (s *Server) exportInvoice(w http.ResponseWriter, r *http.Request) {
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

	var customer, periodStart, periodEnd, items, subtotal, status string
	err = tx.QueryRow(ctx, `
		SELECT c.name, i.period_start::text, i.period_end::text,
		       i.line_items::text, i.subtotal::text, i.status
		FROM invoices i
		JOIN customers c ON c.id = i.customer_id
		WHERE i.id = $1::uuid AND i.deleted_at IS NULL`, r.PathValue("id")).
		Scan(&customer, &periodStart, &periodEnd, &items, &subtotal, &status)
	if errors.Is(err, pgx.ErrNoRows) {
		problem(w, http.StatusNotFound, "invoice not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	lines := invoiceToQB(customer, r.PathValue("id"), periodStart, periodEnd, items)
	writeQBCSV(w, "invoice-"+r.PathValue("id")+".csv", lines)
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
}

// invoiceToQB: JSONB line items → QB rows. Qty in hours (2dp), Rate and
// Amount as-is. Description carries role + task.
func invoiceToQB(customer, invoiceID, periodStart, periodEnd, items string) []qbLine {
	type item struct {
		TaskID  *string `json:"task_id"`
		Role    string  `json:"role"`
		Minutes float64 `json:"minutes"`
		Rate    float64 `json:"rate"`
		Amount  float64 `json:"amount"`
	}
	var parsed []item
	if err := json.Unmarshal([]byte(items), &parsed); err != nil || len(parsed) == 0 {
		return []qbLine{}
	}
	lines := make([]qbLine, 0, len(parsed))
	for _, it := range parsed {
		desc := it.Role + " time"
		if it.TaskID != nil && *it.TaskID != "" {
			desc += " (task " + *it.TaskID + ")"
		}
		lines = append(lines, qbLine{
			Customer:    customer,
			InvoiceNo:   invoiceID,
			Date:        periodStart,
			DueDate:     periodEnd,
			Description: desc,
			Qty:         strconv.FormatFloat(it.Minutes/60, 'f', 2, 64),
			Rate:        strconv.FormatFloat(it.Rate, 'f', 2, 64),
			Amount:      strconv.FormatFloat(it.Amount, 'f', 2, 64),
		})
	}
	return lines
}

// exportInvoicesBulk: GET /v1/invoices/export?from=&to=
// Month-end batch: all non-void invoices whose period intersects [from,to].
func (s *Server) exportInvoicesBulk(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from, to := q.Get("from"), q.Get("to")
	if _, err := time.Parse("2006-01-02", from); err != nil {
		problem(w, http.StatusBadRequest, "from must be YYYY-MM-DD")
		return
	}
	if _, err := time.Parse("2006-01-02", to); err != nil {
		problem(w, http.StatusBadRequest, "to must be YYYY-MM-DD")
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

	rows, err := tx.Query(ctx, `
		SELECT c.name, i.id::text, i.period_start::text, i.period_end::text, i.line_items::text
		FROM invoices i
		JOIN customers c ON c.id = i.customer_id
		WHERE i.deleted_at IS NULL
		  AND i.status <> 'void'
		  AND i.period_start <= $1::date AND i.period_end >= $2::date
		ORDER BY c.name, i.period_start`, to, from)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()

	lines := []qbLine{}
	for rows.Next() {
		var customer, invoiceID, periodStart, periodEnd, items string
		if err := rows.Scan(&customer, &invoiceID, &periodStart, &periodEnd, &items); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		lines = append(lines, invoiceToQB(customer, invoiceID, periodStart, periodEnd, items)...)
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeQBCSV(w, "invoices-"+from+"-"+to+".csv", lines)
}
