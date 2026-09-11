//go:build integration

package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// marginTestStack: person (asha, cost 75) + default rate card ($200/h
// for role admin) + 1.5h approved time on projA → cost 112.50,
// billed 300, margin 0.625.
func marginTestStack(t *testing.T) *httptestSrv {
	t.Helper()
	srv, _, h, taskID := timeTestStack(t)
	t.Cleanup(func() { srv.Close() })
	_, _, access := devLogin(h)

	if code, body := h.do("POST", "/v1/rate-cards", map[string]any{
		"name": "Std", "rates": []map[string]any{{"role": "admin", "hourly_rate": 200}},
	}); code != http.StatusCreated {
		t.Fatalf("rate card = %d %s", code, body)
	}
	if code, body := h.doJWT("POST", "/v1/people", map[string]any{
		"name": "Asha M", "role": "admin", "user_id": "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"cost_rate": "75", "bill_rate": "200",
	}, access); code != http.StatusCreated {
		t.Fatalf("person = %d %s", code, body)
	}

	st, en := fixedSpan(90, 3)
	if code, body := h.doJWT("POST", "/v1/tasks/"+taskID+"/time", map[string]any{
		"started_at": st, "ended_at": en,
	}, access); code != http.StatusCreated {
		t.Fatalf("log = %d %s", code, body)
	}

	// approve the entry
	ap, err := pgxpool.New(t.Context(), adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	if _, err := ap.Exec(t.Context(),
		`UPDATE time_entries SET status = 'approved' WHERE workspace_id = $1`, wsA); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestProjectMargin(t *testing.T) {
	h := marginTestStack(t)
	_, _, access := devLogin(h)

	code, body := h.doJWT("GET", "/v1/projects/"+projA+"/margin", nil, access)
	if code != http.StatusOK {
		t.Fatalf("margin = %d %s", code, body)
	}
	var m struct {
		LoggedHours float64  `json:"logged_hours"`
		Cost        float64  `json:"cost"`
		Billed      float64  `json:"billed"`
		Margin      *float64 `json:"margin"`
		Unpriced    int      `json:"unpriced"`
	}
	json.Unmarshal(body, &m)
	if m.LoggedHours != 1.5 || m.Cost != 112.5 || m.Billed != 300 {
		t.Fatalf("margin math: cost=%v billed=%v logged=%v", m.Cost, m.Billed, m.LoggedHours)
	}
	if m.Margin == nil || *m.Margin < 0.624 || *m.Margin > 0.626 {
		t.Fatalf("margin = %v (want 0.625)", m.Margin)
	}
	if m.Unpriced != 0 {
		t.Fatalf("unpriced = %d", m.Unpriced)
	}

	// foreign project → 404
	if code, _ = h.doJWT("GET", "/v1/projects/99999999-9999-9999-9999-999999999999/margin", nil, access); code != http.StatusNotFound {
		t.Fatalf("foreign = %d", code)
	}
}

func TestPortfolioMargins(t *testing.T) {
	h := marginTestStack(t)
	_, _, access := devLogin(h)

	code, body := h.doJWT("GET", "/v1/margins", nil, access)
	if code != http.StatusOK {
		t.Fatalf("margins = %d %s", code, body)
	}
	var out struct {
		Projects []struct {
			ProjectID string   `json:"project_id"`
			Billed    float64  `json:"billed"`
			Margin    *float64 `json:"margin"`
		} `json:"projects"`
		Totals struct {
			Billed float64  `json:"billed"`
			Margin *float64 `json:"margin"`
		} `json:"totals"`
	}
	json.Unmarshal(body, &out)
	if len(out.Projects) == 0 {
		t.Fatalf("empty portfolio: %s", body)
	}
	found := false
	for _, p := range out.Projects {
		if p.ProjectID == projA {
			found = true
			if p.Billed != 300 || p.Margin == nil || *p.Margin < 0.624 || *p.Margin > 0.626 {
				t.Fatalf("projA row = %+v", p)
			}
		}
	}
	if !found {
		t.Fatalf("projA missing: %s", body)
	}
	if out.Totals.Billed < 300 || out.Totals.Margin == nil || *out.Totals.Margin < 0.624 || *out.Totals.Margin > 0.626 {
		t.Fatalf("totals = %+v", out.Totals)
	}
}
