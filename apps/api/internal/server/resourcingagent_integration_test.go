//go:build integration

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestResourcingSuggest: §15.4 v1 — ranking, reasons, the anti-bias
// penalty, over-capacity exclusion, role gates, kill switch.
func TestResourcingSuggest(t *testing.T) {
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

	// meera: 5/5 skills, but already loaded 30/40h (senior everyone picks)
	// ravi-person: 5/5 skills, 0h allocated (fresh)
	// kiran: 3/5 skills, 0h
	// zoe: over capacity (45/40) → excluded
	for _, p := range []struct {
		id, name, role string
		skills         string
		cap            string
	}{} {
		_ = p
	}
	pg := func(q string, args ...any) {
		if _, err := adminPool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	pg(`SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false)`)
	pg(`INSERT INTO people (workspace_id, name, role, skills, capacity_hrs, active) VALUES
		('11111111-1111-1111-1111-111111111111', 'Meera', 'Architect', ARRAY['go','react','sql','aws','k8s'], 40, true),
		('11111111-1111-1111-1111-111111111111', 'Ramesh', 'Architect', ARRAY['go','react','sql','aws','k8s'], 40, true),
		('11111111-1111-1111-1111-111111111111', 'Kiran', 'Engineer', ARRAY['go','react','sql'], 40, true),
		('11111111-1111-1111-1111-111111111111', 'Zoe', 'Engineer', ARRAY['go','react','sql','aws','k8s'], 40, true)`)
	// meera: hard allocation covering next week at 30h/w
	pg(fmt.Sprintf(`INSERT INTO allocations (workspace_id, project_id, person_id, role, hours_week, starts_on, ends_on, kind)
		SELECT '11111111-1111-1111-1111-111111111111', '%s', p.id, 'Architect', 30,
		       current_date + 7, current_date + 13, 'hard'
		FROM people p WHERE p.name = 'Meera'`, projA))
	// zoe: 45h/w → over capacity
	pg(fmt.Sprintf(`INSERT INTO allocations (workspace_id, project_id, person_id, role, hours_week, starts_on, ends_on, kind)
		SELECT '11111111-1111-1111-1111-111111111111', '%s', p.id, 'Engineer', 45,
		       current_date + 7, current_date + 13, 'hard'
		FROM people p WHERE p.name = 'Zoe'`, projA))

	// member 403
	rcode, rbody, raviTok := loginAs(h, "ravi@acme.test")
	if rcode != 200 || raviTok == "" {
		t.Fatalf("ravi login = %d %s", rcode, rbody)
	}
	if code, _ := h.doJWT("POST", "/v1/agents/resourcing/suggest", map[string]any{
		"project_id": projA, "role": "Architect", "hours_week": 20,
		"starts_on": "2026-09-14", "ends_on": "2026-09-27",
		"skills": []string{"go", "react", "sql", "aws", "k8s"},
	}, raviTok); code != http.StatusForbidden {
		t.Fatalf("member suggest = %d, want 403", code)
	}

	acode, abody, ashaTok := devLogin(h)
	if acode != 200 || ashaTok == "" {
		t.Fatalf("asha login = %d %s", acode, abody)
	}
	suggest := func() (int, []byte) {
		return h.doJWT("POST", "/v1/agents/resourcing/suggest", map[string]any{
			"project_id": projA, "role": "Architect", "hours_week": 20,
			"starts_on": "2026-09-14", "ends_on": "2026-09-27",
			"skills": []string{"go", "react", "sql", "aws", "k8s"},
		}, ashaTok)
	}
	code, body := suggest()
	if code != http.StatusOK {
		t.Fatalf("suggest = %d %s", code, body)
	}
	var res struct {
		Candidates []candidateOut `json:"candidates"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Candidates) < 2 {
		t.Fatalf("candidates = %d, want >= 2", len(res.Candidates))
	}
	// ramesh (0h allocated) must outrank meera (30h) — the §15.4
	// anti-bias penalty: same skills, less load
	if res.Candidates[0].Name != "Ramesh" {
		t.Fatalf("top = %s (%s), want Ramesh — the load-balance penalty", res.Candidates[0].Name, res.Candidates[0].Reason)
	}
	if res.Candidates[0].Reason == "" {
		t.Fatal("reason string missing (§15.4 'explains why')")
	}
	// zoe excluded (over capacity)
	for _, c := range res.Candidates {
		if c.Name == "Zoe" {
			t.Fatal("zoe should be excluded (over capacity)")
		}
	}
	// agent_runs row
	var n int
	if err := adminPool.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE agent = 'resourcing'`).
		Scan(&n); err != nil || n != 1 {
		t.Fatalf("agent_runs = %d err %v", n, err)
	}

	// kill switch → 503
	pg(`INSERT INTO workspace_settings (workspace_id) VALUES ('11111111-1111-1111-1111-111111111111')
		ON CONFLICT (workspace_id) DO NOTHING`)
	pg(`UPDATE workspace_settings SET agents_enabled = false
		WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`)
	if code, _ := suggest(); code != http.StatusServiceUnavailable {
		t.Fatalf("kill switch suggest = %d, want 503", code)
	}

	// bad body
	if code, _ := h.doJWT("POST", "/v1/agents/resourcing/suggest", map[string]any{
		"project_id": projA, "hours_week": 20,
		"starts_on": "2026-09-14", "ends_on": "2026-09-01",
	}, ashaTok); code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", code)
	}
	// re-enable, then unknown project
	pg(`UPDATE workspace_settings SET agents_enabled = true
		WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`)
	if code, _ := h.doJWT("POST", "/v1/agents/resourcing/suggest", map[string]any{
		"project_id": "99999999-9999-9999-9999-999999999999", "hours_week": 20,
		"starts_on": "2026-09-14", "ends_on": "2026-09-27",
	}, ashaTok); code != http.StatusNotFound {
		t.Fatalf("unknown project = %d", code)
	}
}
