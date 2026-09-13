//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ML risk ranking (§357): additive risk_score + factors; severity is
// the untouched action contract; ordering is model-driven with
// severity as tiebreaker.

func TestSignalsRiskRanking(t *testing.T) {
	_, _, h := filesTestStack(t)
	ap, err := pgxpool.New(context.Background(), adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ap.Close() })
	ctx := context.Background()

	// seed: one project with a 12-overdue cluster (thin red, heavy
	// factors) vs one with stale approval (warn). The heavy warn must
	// outrank a bare red when the model says so.
	for i := 0; i < 12; i++ {
		if _, err := ap.Exec(ctx, `INSERT INTO tasks (workspace_id, project_id, title, owner_type, customer_visible, due_at, status)
			VALUES ($1, $2, $3, 'customer', true, now() - interval '9 days', 'todo')`,
			wsA, projA, "Overdue customer task"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ap.Exec(ctx, `INSERT INTO approvals (workspace_id, project_id, title, status)
		VALUES ($1, $2, 'SoG review', 'pending')`, wsA, projA); err != nil {
		t.Fatal(err)
	}
	// age the approval past the 7d stale gate
	if _, err := ap.Exec(ctx, `UPDATE approvals SET created_at = now() - interval '10 days'
		WHERE workspace_id = $1 AND title = 'SoG review'`, wsA); err != nil {
		t.Fatal(err)
	}

	code, body := h.doWS("GET", "/v1/agents/signals", nil, wsA)
	if code != http.StatusOK {
		t.Fatalf("signals code=%d body=%s", code, body)
	}
	var res struct {
		Signals []struct {
			Kind      string          `json:"kind"`
			Severity  string          `json:"severity"`
			RiskScore float64         `json:"risk_score"`
			Factors   json.RawMessage `json:"factors"`
			Title     string          `json:"title"`
		} `json:"signals"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Signals) < 2 {
		t.Fatalf("expected ≥2 signals, got %d (%v)", len(res.Signals), res.Signals)
	}

	// every signal has a score in 0..1 and factor decomposition
	scores := map[string]float64{}
	for _, sig := range res.Signals {
		if sig.RiskScore <= 0 || sig.RiskScore >= 1 {
			t.Fatalf("%s score out of (0,1): %v", sig.Kind, sig.RiskScore)
		}
		var fs []struct {
			Name   string  `json:"name"`
			Weight float64 `json:"weight"`
		}
		if err := json.Unmarshal(sig.Factors, &fs); err != nil || len(fs) == 0 {
			t.Fatalf("%s factors: %s", sig.Kind, sig.Factors)
		}
		scores[sig.Kind] = sig.RiskScore
	}

	// the 12-overdue cluster (capped overdue factor + red) must
	// outrank the stale-approval warn
	if scores["overdue_cluster"] <= scores["approval_stale"] {
		t.Fatalf("ranking not model-driven: overdue_cluster=%v approval_stale=%v",
			scores["overdue_cluster"], scores["approval_stale"])
	}

	// ordering is score-desc in the response
	for i := 1; i < len(res.Signals); i++ {
		if res.Signals[i].RiskScore > res.Signals[i-1].RiskScore {
			t.Fatal("signals not ordered by risk_score desc")
		}
	}

	// severity unchanged: red is still red
	for _, sig := range res.Signals {
		if sig.Kind == "overdue_cluster" && sig.Severity != "red" {
			t.Fatal("severity re-ranked — the action contract must not shift")
		}
	}
}
