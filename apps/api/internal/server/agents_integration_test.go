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

// TestAgentRunsSurface: role gates, kill-switch toggle + audit, run
// list isolation.
func TestAgentRunsSurface(t *testing.T) {
	srv, pool, h := filesTestStack(t)
	_ = srv
	_ = pool
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adminPool.Close() })

	// asha admin (seed-ws) + ravi member; ravi person for guardian runs
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

	// seed a few runs scoped as the API would
	if _, err := adminPool.Exec(ctx, `
		SELECT set_config('app.workspace_id', '11111111-1111-1111-1111-111111111111', false)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := adminPool.Exec(ctx, `
			INSERT INTO agent_runs (workspace_id, agent, status, model, cost_cents, input_ref, output_ref)
			VALUES ('11111111-1111-1111-1111-111111111111', 'guardian', 'succeeded', 'none', 0, $1, '1 flags')`,
			fmt.Sprintf("guardian:2026-09-%02d", 10+i)); err != nil {
			t.Fatal(err)
		}
	}
	// member: 403 on all reads + the toggle
	rcode, rbody, raviTok := loginAs(h, "ravi@acme.test")
	if rcode != 200 || raviTok == "" {
		t.Fatalf("ravi login = %d %s", rcode, rbody)
	}
	if code, _ := h.doJWT("GET", "/v1/agents/runs", nil, raviTok); code != http.StatusForbidden {
		t.Fatalf("member runs = %d, want 403", code)
	}
	if code, _ := h.doJWT("GET", "/v1/agents/status", nil, raviTok); code != http.StatusForbidden {
		t.Fatalf("member status = %d, want 403", code)
	}
	if code, _ := h.doJWT("PUT", "/v1/agents/status", map[string]any{"enabled": false}, raviTok); code != http.StatusForbidden {
		t.Fatalf("member toggle = %d, want 403", code)
	}

	// admin: list shows only ws A's 3
	acode, abody, ashaTok := devLogin(h)
	if acode != 200 || ashaTok == "" {
		t.Fatalf("asha login = %d %s", acode, abody)
	}
	code, body := h.doJWT("GET", "/v1/agents/runs", nil, ashaTok)
	if code != http.StatusOK {
		t.Fatalf("runs = %d %s", code, body)
	}
	var listed struct {
		Runs []map[string]any `json:"runs"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Runs) != 3 {
		t.Fatalf("runs = %d, want 3 (ws A only)", len(listed.Runs))
	}

	// status: enabled true (no settings row yet)
	code, body = h.doJWT("GET", "/v1/agents/status", nil, ashaTok)
	if code != http.StatusOK {
		t.Fatalf("status = %d %s", code, body)
	}
	var st struct {
		Enabled bool     `json:"enabled"`
		Agents  []string `json:"agents"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Enabled || len(st.Agents) == 0 {
		t.Fatalf("status = %+v", st)
	}

	// kill switch off + audit row
	if code, body := h.doJWT("PUT", "/v1/agents/status", map[string]any{"enabled": false}, ashaTok); code != http.StatusOK {
		t.Fatalf("toggle = %d %s", code, body)
	}
	var n int
	if err := adminPool.QueryRow(ctx, `
		SELECT count(*) FROM audit_logs WHERE action = 'agents.kill_switch'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows = %d err %v", n, err)
	}
	// and the flag actually flipped
	code, body = h.doJWT("GET", "/v1/agents/status", nil, ashaTok)
	_ = json.Unmarshal([]byte(body), &st)
	if st.Enabled {
		t.Fatal("kill switch should be off")
	}
	// bad body
	if code, _ := h.doJWT("PUT", "/v1/agents/status", map[string]any{}, ashaTok); code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", code)
	}
}
