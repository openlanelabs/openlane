//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestSignals: each §15.6 v1 rule fires with story + evidence; the
// negative cases stay silent; kill switch; role gate.
func TestSignals(t *testing.T) {
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

	pg := func(q string, args ...any) {
		if _, err := adminPool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	pg(`SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false)`)
	// project with budget 100h
	pg(`INSERT INTO projects (id, workspace_id, customer_id, name, status, budget_hours) VALUES
		('77777777-7777-7777-7777-777777777777', '11111111-1111-1111-1111-111111111111',
		 '33333333-3333-3333-3333-333333333333', 'Signal Test', 'active', 100)`)
	// stale portal link (never used)
	pg(`INSERT INTO portal_links (workspace_id, project_id, contact_id, token_hash, status, expires_at) VALUES
		('11111111-1111-1111-1111-111111111111', '77777777-7777-7777-7777-777777777777',
		 '44444444-4444-4444-4444-444444444444',
		 encode(sha256('signals-test-token-aaaaaaaaaaaaaaaaaaaaaaaaaaa'::bytea),'hex'), 'active', now() + interval '30 days')`)
	// 3 overdue customer tasks (triggers portal_silent AND overdue_cluster)
	for i := 0; i < 3; i++ {
		pg(`INSERT INTO tasks (workspace_id, project_id, title, owner_type, customer_visible, status, due_at) VALUES
			('11111111-1111-1111-1111-111111111111', '77777777-7777-7777-7777-777777777777',
			 'Overdue customer task', 'customer', true, 'todo', now() - interval '2 days')`)
	}
	// 85h logged → budget_burn warn (85% of 100h)
	// 85h = 5100m as 8×720 − 60: entries capped at 720 by CHECK
	for i := 0; i < 7; i++ {
		pg(`INSERT INTO time_entries (workspace_id, project_id, user_id, started_at, ended_at, minutes, status) VALUES
			('11111111-1111-1111-1111-111111111111', '77777777-7777-7777-7777-777777777777',
			 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
			 now() - interval '90 hours', now() - interval '90 hours' + interval '12 hours', 720, 'draft')`)
	}
	pg(`INSERT INTO time_entries (workspace_id, project_id, user_id, started_at, ended_at, minutes, status) VALUES
		('11111111-1111-1111-1111-111111111111', '77777777-7777-7777-7777-777777777777',
		 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
		 now() - interval '50 hours', now() - interval '49 hours', 60, 'draft')`)
	// stale approval (10d)
	pg(`INSERT INTO approvals (workspace_id, project_id, title, status, created_at) VALUES
		('11111111-1111-1111-1111-111111111111', '77777777-7777-7777-7777-777777777777',
		 'SOW sign-off', 'pending', now() - interval '10 days')`)

	// rate card + approved time for margin_dip: high cost person, low rate card → negative margin
	pg(`INSERT INTO people (workspace_id, user_id, name, role, cost_rate, capacity_hrs, active) VALUES
		('11111111-1111-1111-1111-111111111111', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
		 'Asha', 'Architect', 200, 40, true)`)
	pg(`INSERT INTO rate_cards (workspace_id, name, is_active)
		VALUES ('11111111-1111-1111-1111-111111111111', 'Default', true)`)
	pg(`INSERT INTO rate_card_rates (workspace_id, rate_card_id, role, hourly_rate)
		SELECT '11111111-1111-1111-1111-111111111111', rc.id, 'Architect', 100
		FROM rate_cards rc WHERE rc.workspace_id = '11111111-1111-1111-1111-111111111111' AND rc.name = 'Default'`)
	pg(`INSERT INTO time_entries (workspace_id, project_id, user_id, started_at, ended_at, minutes, status) VALUES
		('11111111-1111-1111-1111-111111111111', '77777777-7777-7777-7777-777777777777',
		 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
		 now() - interval '90 hours', now() - interval '80 hours', 600, 'approved')`)

	// member 403
	rcode, rbody, raviTok := loginAs(h, "ravi@acme.test")
	if rcode != 200 || raviTok == "" {
		t.Fatalf("ravi login = %d %s", rcode, rbody)
	}
	if code, _ := h.doJWT("GET", "/v1/agents/signals", nil, raviTok); code != http.StatusForbidden {
		t.Fatalf("member signals = %d, want 403", code)
	}

	acode, abody, ashaTok := devLogin(h)
	if acode != 200 || ashaTok == "" {
		t.Fatalf("asha login = %d %s", acode, abody)
	}
	code, body := h.doJWT("GET", "/v1/agents/signals", nil, ashaTok)
	if code != http.StatusOK {
		t.Fatalf("signals = %d %s", code, body)
	}
	var res struct {
		Signals []signalOut `json:"signals"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]signalOut{}
	for _, sig := range res.Signals {
		kinds[sig.Kind] = sig
	}
	for _, want := range []string{"portal_silent", "overdue_cluster", "budget_burn", "approval_stale", "margin_dip"} {
		if _, ok := kinds[want]; !ok {
			t.Fatalf("missing signal %q; got %+v", want, res.Signals)
		}
	}
	// stories + evidence present
	if kinds["portal_silent"].Title == "" || len(kinds["portal_silent"].Evidence) == 0 {
		t.Fatal("portal_silent missing story/evidence")
	}
	if kinds["budget_burn"].Severity != "warn" {
		t.Fatalf("budget_burn severity = %s at 85%%, want warn", kinds["budget_burn"].Severity)
	}
	// red first
	if len(res.Signals) > 1 && res.Signals[0].Severity != "red" {
		t.Fatalf("first signal = %s, want red ordering", res.Signals[0].Severity)
	}
	// agent_runs row
	var n int
	if err := adminPool.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE agent = 'signals'`).
		Scan(&n); err != nil || n != 1 {
		t.Fatalf("agent_runs = %d err %v", n, err)
	}

	// kill switch → 503
	pg(`INSERT INTO workspace_settings (workspace_id) VALUES ('11111111-1111-1111-1111-111111111111')
		ON CONFLICT (workspace_id) DO NOTHING`)
	pg(`UPDATE workspace_settings SET agents_enabled = false
		WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`)
	if code, _ := h.doJWT("GET", "/v1/agents/signals", nil, ashaTok); code != http.StatusServiceUnavailable {
		t.Fatalf("kill switch signals = %d, want 503", code)
	}
}
