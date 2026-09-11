//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// fixedSpan: deterministic RFC3339 pair exactly `mins` long, backdated from a
// second-aligned base so int(ended.Sub(started).Minutes()) never races the
// RFC3339 second truncation (logTime derives minutes from the timestamps).
func fixedSpan(mins int, backHours int) (string, string) {
	base := time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(backHours) * time.Hour)
	return base.Format(time.RFC3339), base.Add(time.Duration(mins) * time.Minute).Format(time.RFC3339)
}

func budgetLevel(t *testing.T, projectID string) int {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var level int
	if err := pool.QueryRow(context.Background(),
		`SELECT budget_alert_level FROM projects WHERE id = $1`, projectID).Scan(&level); err != nil {
		t.Fatal(err)
	}
	return level
}

func TestBudgetRollupAndAlerts(t *testing.T) {
	srv, _, h, taskID := timeTestStack(t)
	defer srv.Close()
	_, _, access := devLogin(h)

	// unbudgeted: pct null, status unbudgeted, billing defaults to tm
	code, body := h.doJWT("GET", "/v1/projects/"+projA+"/budget", nil, access)
	if code != http.StatusOK {
		t.Fatalf("budget = %d %s", code, body)
	}
	var b struct {
		BudgetHours *int     `json:"budget_hours"`
		BillingType string   `json:"billing_type"`
		LoggedHours float64  `json:"logged_hours"`
		Pct         *float64 `json:"pct"`
		Status      string   `json:"status"`
	}
	json.Unmarshal(body, &b)
	if b.Pct != nil || b.Status != "unbudgeted" || b.BillingType != "tm" {
		t.Fatalf("unbudgeted shape: %s", body)
	}

	// validation: billing_type and budget_hours
	if code, body = h.doJWT("PATCH", "/v1/projects/"+projA, map[string]any{"billing_type": "barter"}, access); code != http.StatusBadRequest {
		t.Fatalf("billing_type validation = %d", code)
	}
	if code, body = h.doJWT("PATCH", "/v1/projects/"+projA, map[string]any{"budget_hours": -1}, access); code != http.StatusBadRequest {
		t.Fatalf("budget validation = %d", code)
	}

	// set budget: 10 hours
	if code, body = h.doJWT("PATCH", "/v1/projects/"+projA, map[string]any{"budget_hours": 10, "billing_type": "tm"}, access); code != http.StatusOK {
		t.Fatalf("set budget = %d %s", code, body)
	}

	// log 6h (3×2h) → 60% → warn50
	for i := 0; i < 3; i++ {
		st, en := fixedSpan(120, 2+i)
		if code, body = h.doJWT("POST", "/v1/tasks/"+taskID+"/time", map[string]any{
			"started_at": st, "ended_at": en,
		}, access); code != http.StatusCreated {
			t.Fatalf("log %d = %d %s", i, code, body)
		}
	}
	code, body = h.doJWT("GET", "/v1/projects/"+projA+"/budget", nil, access)
	json.Unmarshal(body, &b)
	if b.Status != "warn50" || b.Pct == nil || *b.Pct < 60 {
		t.Fatalf("after 6h: %s", body)
	}

	// alert level 50
	deadline := time.Now().Add(5 * time.Second)
	for budgetLevel(t, projA) != 50 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := budgetLevel(t, projA); got != 50 {
		t.Fatalf("alert level = %d (want 50)", got)
	}

	// log 3 more hours (3×1h) → 9h = 90% → warn80
	for i := 0; i < 3; i++ {
		st, en := fixedSpan(60, 10+i)
		if code, body = h.doJWT("POST", "/v1/tasks/"+taskID+"/time", map[string]any{
			"started_at": st, "ended_at": en,
		}, access); code != http.StatusCreated {
			t.Fatalf("log2 %d = %d %s", i, code, body)
		}
	}
	code, body = h.doJWT("GET", "/v1/projects/"+projA+"/budget", nil, access)
	json.Unmarshal(body, &b)
	if b.Status != "warn80" {
		t.Fatalf("after 9h: %s", body)
	}
	deadline = time.Now().Add(5 * time.Second)
	for budgetLevel(t, projA) != 80 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := budgetLevel(t, projA); got != 80 {
		t.Fatalf("alert level = %d (want 80)", got)
	}

	// log 2 more hours → 11h = 110% → over
	for i := 0; i < 2; i++ {
		st, en := fixedSpan(60, 20+i)
		if code, body = h.doJWT("POST", "/v1/tasks/"+taskID+"/time", map[string]any{
			"started_at": st, "ended_at": en,
		}, access); code != http.StatusCreated {
			t.Fatalf("log3 %d = %d %s", i, code, body)
		}
	}
	code, body = h.doJWT("GET", "/v1/projects/"+projA+"/budget", nil, access)
	json.Unmarshal(body, &b)
	if b.Status != "over" {
		t.Fatalf("after 11h: %s", body)
	}
	deadline = time.Now().Add(5 * time.Second)
	for budgetLevel(t, projA) != 100 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := budgetLevel(t, projA); got != 100 {
		t.Fatalf("alert level = %d (want 100)", got)
	}

	// raising budget re-arms the alert bands (§326 semantics)
	if code, body = h.doJWT("PATCH", "/v1/projects/"+projA, map[string]any{"budget_hours": 100}, access); code != http.StatusOK {
		t.Fatalf("raise budget = %d %s", code, body)
	}
	if got := budgetLevel(t, projA); got != 0 {
		t.Fatalf("re-arm level = %d (want 0)", got)
	}
}

func TestBudgetAlertsFiredOncePerBand(t *testing.T) {
	srv, _, h, taskID := timeTestStack(t)
	defer srv.Close()
	_, _, access := devLogin(h)

	// budget: 5 hours → 50% at 150min, 80% at 240min, 100% at 300min
	if code, body := h.doJWT("PATCH", "/v1/projects/"+projA, map[string]any{"budget_hours": 5}, access); code != http.StatusOK {
		t.Fatalf("budget = %d %s", code, body)
	}

	// one 150-min entry → exactly 50% → warn50 fires once
	st, en := fixedSpan(150, 3)
	if code, body := h.doJWT("POST", "/v1/tasks/"+taskID+"/time", map[string]any{
		"started_at": st, "ended_at": en,
	}, access); code != http.StatusCreated {
		t.Fatalf("log = %d %s", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for budgetLevel(t, projA) != 50 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := budgetLevel(t, projA); got != 50 {
		t.Fatalf("alert level never reached 50 (got %d)", got)
	}

	// 30 more min (180 total = 60%) — still in the 50 band → no re-fire
	st2, en2 := fixedSpan(30, 30)
	if code, body := h.doJWT("POST", "/v1/tasks/"+taskID+"/time", map[string]any{
		"started_at": st2, "ended_at": en2,
	}, access); code != http.StatusCreated {
		t.Fatalf("log2 = %d %s", code, body)
	}
	time.Sleep(500 * time.Millisecond) // let any racing goroutine settle
	if got := budgetLevel(t, projA); got != 50 {
		t.Fatalf("re-fire in band = %d (want 50 — no double alert)", got)
	}
}
