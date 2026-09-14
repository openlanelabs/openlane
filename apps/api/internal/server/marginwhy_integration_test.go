//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Margin variance why (§324/§528): deterministic drivers + a
// plain-English sentence. The why must match /margins exactly
// (same canonical computation) — drift is the failure mode.

func TestMarginWhy(t *testing.T) {
	h := marginTestStack(t)
	_, _, access := devLogin(h)
	ap, err := pgxpool.New(t.Context(), adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()

	// force the §324 shape: tiny budget (1.5h logged > 0.5h budget →
	// overrun) and Asha at 75 vs blended — she IS the blended (only
	// person), so concentration needs a second person above her.
	// Raise Asha's cost to 150 after the entry: blended was 75 at
	// compute time... blended is CURRENT rates; entry cost is too
	// (rate resolved live). Simpler: add a second premium person and
	// log a bit as them via direct insert.
	if _, err := ap.Exec(context.Background(), `UPDATE projects SET budget_hours = 0.5 WHERE id = $1`, projA); err != nil {
		t.Fatal(err)
	}
	// second person at 3× blended (75) = 225/h — concentration driver.
	// Direct insert: the API requires an existing membership for
	// user_id linkage; the suite's pattern for synthetic users.
	if _, err := ap.Exec(context.Background(), `INSERT INTO users (id, email, display_name)
		VALUES ('bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'priya@acme.test', 'Priya')
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Exec(context.Background(), `INSERT INTO people
		(workspace_id, user_id, name, role, cost_rate, bill_rate)
		VALUES ($1, 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'Priya Contractor', 'admin', 225, 300)`,
		wsA); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Exec(context.Background(), `INSERT INTO memberships (workspace_id, user_id, role)
		VALUES ($1, 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'admin')
		ON CONFLICT DO NOTHING`, wsA); err != nil {
		t.Fatal(err)
	}
	// 0.5h approved entry by Priya on projA (task-less, like the
	// direct inserts the suite uses)
	if _, err := ap.Exec(context.Background(), `INSERT INTO time_entries
		(workspace_id, project_id, user_id, task_id, started_at, ended_at, minutes, status)
		VALUES ($1, $2, 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', NULL,
		        now() - interval '2 hours', now() - interval '90 minutes', 30, 'approved')`,
		wsA, projA); err != nil {
		t.Fatal(err)
	}

	code, body := h.doJWT("GET", "/v1/margins/"+projA+"/why", nil, access)
	if code != http.StatusOK {
		t.Fatalf("why = %d %s", code, body)
	}
	var res struct {
		Drivers []struct {
			Kind  string  `json:"kind"`
			Label string  `json:"label"`
			Delta float64 `json:"delta_cost"`
		} `json:"drivers"`
		Sentence string `json:"sentence"`
		Margin   struct {
			LoggedHours float64  `json:"logged_hours"`
			Cost        float64  `json:"cost"`
			Billed      float64  `json:"billed"`
			Margin      *float64 `json:"margin"`
			Unpriced    int      `json:"unpriced"`
		} `json:"margin"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}

	// margin matches the canonical row: 2h logged (1.5 Asha + 0.5
	// Priya), cost = 1.5×75 + 0.5×225 = 225, billed = 2×200 = 400
	if res.Margin.LoggedHours != 2 || res.Margin.Cost != 225 || res.Margin.Billed != 400 {
		t.Fatalf("margin row drift: %+v", res.Margin)
	}

	// both drivers present
	kinds := map[string]bool{}
	for _, d := range res.Drivers {
		kinds[d.Kind] = true
	}
	if !kinds["budget_overrun"] {
		t.Fatalf("missing budget_overrun: %+v", res.Drivers)
	}
	if !kinds["cost_concentration"] {
		t.Fatalf("missing cost_concentration (Priya 225 vs blended 112.5): %+v", res.Drivers)
	}

	// ordered by |delta| desc
	for i := 1; i < len(res.Drivers); i++ {
		if absF(res.Drivers[i].Delta) > absF(res.Drivers[i-1].Delta) {
			t.Fatal("drivers not ordered by |delta| desc")
		}
	}

	// sentence is plain English citing the drivers
	if res.Sentence == "" {
		t.Fatal("empty sentence")
	}
	if !strings.Contains(res.Sentence, "over the") && !strings.Contains(res.Sentence, "at $") {
		t.Fatalf("sentence lacks driver labels: %q", res.Sentence)
	}

	// clean project → on-plan sentence
	var cleanID string
	if err := ap.QueryRow(context.Background(),
		`INSERT INTO projects (workspace_id, customer_id, name, status, budget_hours)
		 VALUES ($1, (SELECT customer_id FROM projects WHERE id = $2), 'Clean proj', 'active', 500)
		 RETURNING id`, wsA, projA).Scan(&cleanID); err != nil {
		t.Fatal(err)
	}
	code2, body2 := h.doJWT("GET", "/v1/margins/"+cleanID+"/why", nil, access)
	if code2 != http.StatusOK {
		t.Fatalf("clean why = %d %s", code2, body2)
	}
	var res2 struct {
		Drivers   []struct{} `json:"drivers"`
		Sentence  string     `json:"sentence"`
	}
	if err := json.Unmarshal(body2, &res2); err != nil {
		t.Fatal(err)
	}
	if len(res2.Drivers) != 0 || res2.Sentence != "Margin on plan — no adverse drivers." {
		t.Fatalf("clean project should be on plan: %q %+v", res2.Sentence, res2.Drivers)
	}
}

func absF(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
