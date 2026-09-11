//go:build integration

package server

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// invoiceTestStack: time stack + a default rate card covering the seeded
// member's role + one approved 90-min entry on projA (=$300 @ $200/h).
func invoiceTestStack(t *testing.T) (*httptestSrv, string, string) {
	t.Helper()
	srv, _, h, taskID := timeTestStack(t)
	t.Cleanup(func() { srv.Close() })
	_, _, access := devLogin(h)

	if code, body := h.do("POST", "/v1/rate-cards", map[string]any{
		"name": "Std card", "rates": []map[string]any{{"role": "admin", "hourly_rate": 200}},
	}); code != http.StatusCreated {
		t.Fatalf("rate card = %d %s", code, body)
	}

	st, en := fixedSpan(90, 2)
	if code, body := h.doJWT("POST", "/v1/tasks/"+taskID+"/time", map[string]any{
		"started_at": st, "ended_at": en,
	}, access); code != http.StatusCreated {
		t.Fatalf("log = %d %s", code, body)
	}

	// approve directly (time approval flow is out of scope here)
	ap, err := pgxpool.New(t.Context(), adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	if _, err := ap.Exec(t.Context(),
		`UPDATE time_entries SET status = 'approved' WHERE workspace_id = $1`, wsA); err != nil {
		t.Fatal(err)
	}
	return h, taskID, access
}

func TestInvoiceGenerateAndLifecycle(t *testing.T) {
	h, _, access := invoiceTestStack(t)

	today := time.Now().UTC().Format("2006-01-02")
	from := time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02")
	window := map[string]any{"period_start": from, "period_end": today}

	// generate → 201, math: 90min × $200/60 = $300
	code, body := h.doJWT("POST", "/v1/customers/"+custID+"/invoices/generate", window, access)
	if code != http.StatusCreated {
		t.Fatalf("generate = %d %s", code, body)
	}
	var inv struct {
		ID        string          `json:"id"`
		Status    string          `json:"status"`
		LineItems json.RawMessage `json:"line_items"`
		Subtotal  string          `json:"subtotal"`
		PeriodEnd string          `json:"period_end"`
	}
	json.Unmarshal(body, &inv)
	if inv.Status != "draft" || inv.Subtotal != "300.00" {
		t.Fatalf("draft shape: %s", body)
	}
	var lines []map[string]any
	json.Unmarshal(inv.LineItems, &lines)
	if len(lines) != 1 || lines[0]["role"] != "admin" || lines[0]["minutes"].(float64) != 90 ||
		lines[0]["amount"].(float64) != 300 {
		t.Fatalf("line items: %s", inv.LineItems)
	}

	// regenerate same window → 404: entries are invoiced, no double billing
	if code, body = h.doJWT("POST", "/v1/customers/"+custID+"/invoices/generate", window, access); code != http.StatusNotFound {
		t.Fatalf("double-generate = %d %s (want 404)", code, body)
	}

	// list + get
	if code, body = h.doJWT("GET", "/v1/invoices", nil, access); code != http.StatusOK {
		t.Fatalf("list = %d %s", code, body)
	}
	var list []struct{ ID string }
	json.Unmarshal(body, &list)
	if len(list) != 1 || list[0].ID != inv.ID {
		t.Fatalf("list = %s", body)
	}
	if code, body = h.doJWT("GET", "/v1/invoices/"+inv.ID, nil, access); code != http.StatusOK {
		t.Fatalf("get = %d %s", code, body)
	}

	// transitions: draft→sent, sent→paid, paid→void; notes lock after send
	if code, body = h.doJWT("PATCH", "/v1/invoices/"+inv.ID, map[string]any{"notes": "net-30"}, access); code != http.StatusOK {
		t.Fatalf("notes while draft = %d %s", code, body)
	}
	if code, body = h.doJWT("PATCH", "/v1/invoices/"+inv.ID, map[string]any{"status": "sent"}, access); code != http.StatusOK {
		t.Fatalf("draft→sent = %d %s", code, body)
	}
	if code, body = h.doJWT("PATCH", "/v1/invoices/"+inv.ID, map[string]any{"notes": "edit"}, access); code != http.StatusBadRequest {
		t.Fatalf("notes after send = %d (want 400)", code)
	}
	if code, body = h.doJWT("PATCH", "/v1/invoices/"+inv.ID, map[string]any{"status": "draft"}, access); code != http.StatusBadRequest {
		t.Fatalf("sent→draft = %d (want 400)", code)
	}
	if code, body = h.doJWT("PATCH", "/v1/invoices/"+inv.ID, map[string]any{"status": "paid"}, access); code != http.StatusOK {
		t.Fatalf("sent→paid = %d %s", code, body)
	}
	if code, body = h.doJWT("PATCH", "/v1/invoices/"+inv.ID, map[string]any{"status": "void"}, access); code != http.StatusOK {
		t.Fatalf("paid→void = %d %s", code, body)
	}
	if code, body = h.doJWT("PATCH", "/v1/invoices/"+inv.ID, map[string]any{"status": "sent"}, access); code != http.StatusBadRequest {
		t.Fatalf("void→sent = %d (want 400)", code)
	}
}

func TestInvoiceCrossWorkspaceIsolation(t *testing.T) {
	h, _, access := invoiceTestStack(t)

	today := time.Now().UTC().Format("2006-01-02")
	from := time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02")

	// wrong customer id → 404 (RLS: zero unbilled rows)
	if code, _ := h.doJWT("POST", "/v1/customers/99999999-9999-9999-9999-999999999999/invoices/generate", map[string]any{
		"period_start": from, "period_end": today,
	}, access); code != http.StatusNotFound {
		t.Fatalf("foreign customer = %d (want 404)", code)
	}

	// unknown invoice id → 404
	if code, _ := h.doJWT("GET", "/v1/invoices/99999999-9999-9999-9999-999999999999", nil, access); code != http.StatusNotFound {
		t.Fatalf("foreign invoice = %d (want 404)", code)
	}

	// customer-card priority beats default card: create a customer card at
	// $100 and regenerate in a disjoint window with fresh approved time
	ap, err := pgxpool.New(t.Context(), adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	// revert the first stack's entries to approved so we can bill a second window
	if _, err := ap.Exec(t.Context(),
		`UPDATE time_entries SET status = 'approved' WHERE workspace_id = $1 AND status = 'invoiced'`, wsA); err != nil {
		t.Fatal(err)
	}
	if code, body := h.do("POST", "/v1/rate-cards", map[string]any{
		"customer_id": custID, "name": "Acme card", "rates": []map[string]any{{"role": "admin", "hourly_rate": 100}},
	}); code != http.StatusCreated {
		t.Fatalf("customer card = %d %s", code, body)
	}
	// widen window to include the original entries → bills at $150 not $300
	code, body := h.doJWT("POST", "/v1/customers/"+custID+"/invoices/generate", map[string]any{
		"period_start": from, "period_end": today,
	}, access)
	if code != http.StatusCreated {
		t.Fatalf("regen at customer rate = %d %s", code, body)
	}
	var inv struct{ Subtotal string }
	json.Unmarshal(body, &inv)
	if inv.Subtotal != "150.00" {
		t.Fatalf("customer card priority: %s (want 150.00)", body)
	}
}
