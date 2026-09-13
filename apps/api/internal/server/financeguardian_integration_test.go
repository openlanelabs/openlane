//go:build integration

package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestFinanceGuardian: §15.5 — configurable margin thresholds, flags
// never fixes. Zero writes outside agent_runs (asserted by row counts).
func TestFinanceGuardian(t *testing.T) {
	_, _, h := filesTestStack(t)
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adminPool.Close() })
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO users (id, email, display_name) VALUES
		  ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'asha@acme.test', 'Asha'),
		  ('bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'ravi@acme.test', 'ravi')
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO memberships (workspace_id, user_id, role) VALUES
		  ('11111111-1111-1111-1111-111111111111', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'admin'),
		  ('11111111-1111-1111-1111-111111111111', 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'member')
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	// margin data: asha cost 200, admin rate 100 → 600m approved →
	// billed 1000, cost 2000 → margin -100% (red at any threshold)
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO people (workspace_id, user_id, name, role, cost_rate, capacity_hrs, active)
		VALUES ('11111111-1111-1111-1111-111111111111', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
			'Asha', 'admin', 200, 40, true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO rate_cards (workspace_id, name, is_active)
		VALUES ('11111111-1111-1111-1111-111111111111', 'Guardian Default', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO rate_card_rates (workspace_id, rate_card_id, role, hourly_rate)
		SELECT '11111111-1111-1111-1111-111111111111', rc.id, 'admin', 100
		FROM rate_cards rc WHERE rc.workspace_id = '11111111-1111-1111-1111-111111111111'
		  AND rc.name = 'Guardian Default'`); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO time_entries (workspace_id, project_id, user_id, started_at, ended_at, minutes, status)
		VALUES ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555',
			'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', now() - interval '10 hours', now() - interval '9 hours',
			600, 'approved')`); err != nil {
		t.Fatal(err)
	}

	acode, _, ashaTok := devLogin(h)
	if acode != 200 {
		t.Fatalf("asha login = %d", acode)
	}
	rcode, _, raviTok := loginAs(h, "ravi@acme.test")
	if rcode != 200 {
		t.Fatalf("ravi login = %d", rcode)
	}

	// member 403 on guardian
	if code, _ := h.doJWT("GET", "/v1/agents/finance/guardian", nil, raviTok); code != 403 {
		t.Fatalf("member guardian = %d, want 403", code)
	}
	// member 403 on config PUT; admin OK
	if code, _ := h.doJWT("PUT", "/v1/agents/finance/config",
		map[string]any{"margin_red_pct": 50}, raviTok); code != 403 {
		t.Fatalf("member config PUT = %d, want 403", code)
	}
	// warn < red rejected
	if code, _ := h.doJWT("PUT", "/v1/agents/finance/config",
		map[string]any{"margin_warn_pct": 10, "margin_red_pct": 20}, ashaTok); code != 400 {
		t.Fatalf("warn<red = %d, want 400", code)
	}
	if code, _ := h.doJWT("PUT", "/v1/agents/finance/config",
		map[string]any{"margin_warn_pct": 60, "margin_red_pct": 50}, ashaTok); code != 200 {
		t.Fatalf("config PUT = %d, want 200", code)
	}

	// snapshot row counts — the guardian must write NOTHING
	countRows := func(q string) int {
		var n int
		if err := adminPool.QueryRow(ctx, q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	tasksBefore := countRows(`SELECT count(*) FROM tasks`)
	invBefore := countRows(`SELECT count(*) FROM invoices`)
	teBefore := countRows(`SELECT count(*) FROM time_entries`)

	code, out := h.doJWT("GET", "/v1/agents/finance/guardian", nil, ashaTok)
	if code != 200 {
		t.Fatalf("guardian = %d %s", code, out)
	}
	var res struct {
		Enabled    bool `json:"enabled"`
		Thresholds struct {
			WarnPct int `json:"warn_pct"`
			RedPct  int `json:"red_pct"`
		} `json:"thresholds"`
		Flags []struct {
			ProjectID  string  `json:"project_id"`
			Name       string  `json:"name"`
			Billed     float64 `json:"billed"`
			Cost       float64 `json:"cost"`
			MarginPct  *int    `json:"margin_pct"`
			Severity   string  `json:"severity"`
			Threshold  int     `json:"threshold"`
			Suggestion string  `json:"suggestion"`
		} `json:"flags"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Enabled || res.Thresholds.WarnPct != 60 || res.Thresholds.RedPct != 50 {
		t.Fatalf("config not applied: %+v", res.Thresholds)
	}
	// projA: margin -100% → red at the custom threshold
	found := false
	for _, f := range res.Flags {
		if f.ProjectID == "55555555-5555-5555-5555-555555555555" {
			found = true
			if f.Severity != "red" || f.MarginPct == nil || *f.MarginPct != -100 {
				t.Fatalf("projA flag = %+v, want red -100%%", f)
			}
			if f.Suggestion == "" {
				t.Fatal("flag without suggestion")
			}
		}
	}
	if !found {
		t.Fatal("projA not flagged")
	}
	// ZERO writes to business tables
	if n := countRows(`SELECT count(*) FROM tasks`); n != tasksBefore {
		t.Fatalf("guardian wrote tasks: %d -> %d", tasksBefore, n)
	}
	if n := countRows(`SELECT count(*) FROM invoices`); n != invBefore {
		t.Fatalf("guardian wrote invoices: %d -> %d", invBefore, n)
	}
	if n := countRows(`SELECT count(*) FROM time_entries`); n != teBefore {
		t.Fatalf("guardian wrote time: %d -> %d", teBefore, n)
	}
	// agent_runs metered
	var runCt int
	if err := adminPool.QueryRow(ctx,
		`SELECT count(*) FROM agent_runs WHERE agent='guardian'`).Scan(&runCt); err != nil || runCt < 1 {
		t.Fatalf("guardian runs = %d err %v", runCt, err)
	}

	// min_billed filters noise: raise it above projA's billed
	if code, _ := h.doJWT("PUT", "/v1/agents/finance/config",
		map[string]any{"min_billed": 100000}, ashaTok); code != 200 {
		t.Fatal("config PUT min_billed failed")
	}
	code, out = h.doJWT("GET", "/v1/agents/finance/guardian", nil, ashaTok)
	if code != 200 {
		t.Fatalf("guardian 2 = %d", code)
	}
	var res2 struct {
		Flags []json.RawMessage `json:"flags"`
	}
	if err := json.Unmarshal(out, &res2); err != nil {
		t.Fatal(err)
	}
	if len(res2.Flags) != 0 {
		t.Fatalf("min_billed filter failed: %d flags", len(res2.Flags))
	}
	// enabled=false → empty + enabled false
	if code, _ := h.doJWT("PUT", "/v1/agents/finance/config",
		map[string]any{"min_billed": 0, "enabled": false}, ashaTok); code != 200 {
		t.Fatal("config disable failed")
	}
	code, out = h.doJWT("GET", "/v1/agents/finance/guardian", nil, ashaTok)
	if code != 200 {
		t.Fatal("guardian 3 failed")
	}
	var res3 struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.Unmarshal(out, &res3); err != nil || res3.Enabled {
		t.Fatal("guardian should report enabled=false")
	}
	// GET config member-readable
	if code, _ := h.doJWT("GET", "/v1/agents/finance/config", nil, raviTok); code != 200 {
		t.Fatalf("member config GET = %d, want 200", code)
	}
	// kill switch
	if _, err := adminPool.Exec(ctx, `INSERT INTO workspace_settings (workspace_id)
		VALUES ('11111111-1111-1111-1111-111111111111') ON CONFLICT (workspace_id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `UPDATE workspace_settings SET agents_enabled = false
		WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.doJWT("GET", "/v1/agents/finance/guardian", nil, ashaTok); code != 503 {
		t.Fatalf("kill switch guardian = %d, want 503", code)
	}
	if _, err := adminPool.Exec(ctx, `UPDATE workspace_settings SET agents_enabled = true
		WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`); err != nil {
		t.Fatal(err)
	}
}
