//go:build integration

package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// invoiceWithEscapedData: an invoice whose line-item roles include
// commas/quotes — CSV escaping must survive.
func invoiceWithEscapedData(t *testing.T, access string) string {
	t.Helper()
	// seed a draft invoice directly with nasty data
	ap, err := pgxpool.New(t.Context(), adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	items := `[{"task_id":null,"role":"dev, \"senior\"","minutes":60,"rate":100,"amount":100}]`
	var id string
	if err := ap.QueryRow(t.Context(), `INSERT INTO invoices (workspace_id, customer_id, status, period_start, period_end, line_items, subtotal)
		VALUES ($1, $2, 'draft', '2026-01-01', '2026-01-31', $3, 100) RETURNING id::text`,
		wsA, custID, items).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestInvoiceExportCSV(t *testing.T) {
	h, _, access := invoiceTestStack(t)

	// generate a real invoice first (1.5h @ 200 = 300)
	today := time.Now().UTC().Format("2006-01-02")
	from := time.Now().UTC().AddDate(0, 0, -7).Format("2006-01-02")
	code, body := h.doJWT("POST", "/v1/customers/"+custID+"/invoices/generate", map[string]any{
		"period_start": from, "period_end": today,
	}, access)
	if code != http.StatusCreated {
		t.Fatalf("generate = %d %s", code, body)
	}
	var inv struct{ ID string }
	json.Unmarshal(body, &inv)

	// export it
	code2, raw := h.doJWT("GET", "/v1/invoices/"+inv.ID+"/export", nil, access)
	if code2 != http.StatusOK {
		t.Fatalf("export = %d %s", code2, raw)
	}
	if ct := exportContentType(h, access); !strings.HasPrefix(ct, "text/csv") {
		t.Fatalf("content-type = %s", ct)
	}
	csvText := string(raw)
	for _, want := range []string{"Customer,InvoiceNo,Date,DueDate,Description,Qty,Rate,Amount", "Adobe", "admin time", "1.50", "200.00", "300.00"} {
		if !strings.Contains(csvText, want) {
			t.Fatalf("csv missing %q:\n%s", want, csvText)
		}
	}

	// foreign invoice → 404
	if code, _ = h.doJWT("GET", "/v1/invoices/99999999-9999-9999-9999-999999999999/export", nil, access); code != http.StatusNotFound {
		t.Fatalf("foreign export = %d", code)
	}
}

// exportContentType: header check needs a raw request — doJWT doesn't
// return headers.
func exportContentType(h *httptestSrv, access string) string {
	req, _ := http.NewRequest("GET", h.URL+"/v1/invoices/export?from=2026-01-01&to=2026-01-31", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("X-Workspace-Id", wsA)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	return resp.Header.Get("Content-Type")
}

func TestInvoiceExportBulkAndEscaping(t *testing.T) {
	h, _, access := invoiceTestStack(t)

	// escaped-role invoice
	id := invoiceWithEscapedData(t, access)

	// bulk export window covering it
	code, raw := h.doJWT("GET", "/v1/invoices/export?from=2026-01-01&to=2026-01-31", nil, access)
	if code != http.StatusOK {
		t.Fatalf("bulk = %d %s", code, raw)
	}
	csvText := string(raw)
	if !strings.Contains(csvText, `"dev, ""senior"" time"`) {
		t.Fatalf("csv escaping broken:\n%s", csvText)
	}
	// header row present
	if !strings.HasPrefix(csvText, "Customer,") {
		t.Fatalf("no header:\n%s", csvText[:100])
	}
	// void invoices excluded: void it, re-export, gone
	if code, body := h.doJWT("PATCH", "/v1/invoices/"+id, map[string]any{"status": "void"}, access); code != http.StatusOK {
		t.Fatalf("void = %d %s", code, body)
	}
	code, raw2 := h.doJWT("GET", "/v1/invoices/export?from=2026-01-01&to=2026-01-31", nil, access)
	if code != http.StatusOK {
		t.Fatalf("bulk2 = %d", code)
	}
	if strings.Contains(string(raw2), "senior") {
		t.Fatalf("void invoice still exported:\n%s", raw2)
	}
	// bad window → 400
	if code, _ = h.doJWT("GET", "/v1/invoices/export?from=oops&to=2026-01-31", nil, access); code != http.StatusBadRequest {
		t.Fatalf("bad window = %d", code)
	}
}
