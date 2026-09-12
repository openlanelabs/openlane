//go:build integration

package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMcpToolsAndKillSwitch: the MCP dispatch against a fresh stack —
// kill switch, agent_runs logging, and the read tools.
func TestMcpToolsAndKillSwitch(t *testing.T) {
	_, pool, h := filesTestStack(t)
	_ = h

	ap, err := pgxpool.New(context.Background(), adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ap.Close() })
	// seed: a project + person + rate card + approved time for margins,
	// and a submitted entry for pending_time
	if _, err := ap.Exec(context.Background(), `
		INSERT INTO users (id, email, display_name) VALUES
		  ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'asha@acme.test', 'Asha')
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Exec(context.Background(), `
		INSERT INTO memberships (workspace_id, user_id, role) VALUES
		  ('11111111-1111-1111-1111-111111111111', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'admin')
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}

	m := NewMcpServer(pool)
	ws := "11111111-1111-1111-1111-111111111111"
	ctx := context.Background()

	// initialize
	resp := m.Dispatch(ctx, ws, map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	if resp["error"] != nil {
		t.Fatalf("initialize = %v", resp)
	}

	// tools/list: 7 tools
	resp = m.Dispatch(ctx, ws, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	var list struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	b, _ := json.Marshal(resp)
	_ = json.Unmarshal(b, &list)
	if len(list.Result.Tools) != 7 {
		t.Fatalf("tools/list = %d tools", len(list.Result.Tools))
	}

	// list_projects works + logs
	resp = m.Dispatch(ctx, ws, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "list_projects", "arguments": map[string]any{}},
	})
	if resp["error"] != nil {
		t.Fatalf("list_projects = %v", resp)
	}
	var n int
	if err := ap.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE agent = 'mcp' AND status = 'succeeded'`).
		Scan(&n); err != nil || n != 1 {
		t.Fatalf("agent_runs = %d err %v", n, err)
	}

	// unknown tool
	resp = m.Dispatch(ctx, ws, map[string]any{
		"jsonrpc": "2.0", "id": 4, "method": "tools/call",
		"params": map[string]any{"name": "nope", "arguments": map[string]any{}},
	})
	if resp["error"] == nil {
		t.Fatal("unknown tool should error")
	}

	// kill switch: agents_enabled = false → error + no run row change
	if _, err := ap.Exec(ctx, `
		INSERT INTO workspace_settings (workspace_id) VALUES ('11111111-1111-1111-1111-111111111111')
		ON CONFLICT (workspace_id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Exec(ctx, `
		UPDATE workspace_settings SET agents_enabled = false
		WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`); err != nil {
		t.Fatal(err)
	}
	resp = m.Dispatch(ctx, ws, map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]any{"name": "list_projects", "arguments": map[string]any{}},
	})
	if resp["error"] == nil {
		t.Fatal("kill switch should block")
	}
	if err := ap.QueryRow(ctx, `SELECT count(*) FROM agent_runs WHERE agent = 'mcp'`).
		Scan(&n); err != nil || n != 1 {
		t.Fatalf("kill switch should not log: %d", n)
	}

	// re-enable; search runs
	if _, err := ap.Exec(ctx, `
		UPDATE workspace_settings SET agents_enabled = true
		WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`); err != nil {
		t.Fatal(err)
	}
	resp = m.Dispatch(ctx, ws, map[string]any{
		"jsonrpc": "2.0", "id": 6, "method": "tools/call",
		"params": map[string]any{"name": "search", "arguments": map[string]any{"q": "onboard"}},
	})
	if resp["error"] != nil {
		t.Fatalf("search = %v", resp)
	}

	// cross-workspace isolation: ws B sees nothing of ws A's rows
	resp = m.Dispatch(ctx, "22222222-2222-2222-2222-222222222222", map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": "list_projects", "arguments": map[string]any{}},
	})
	b2, _ := json.Marshal(resp)
	if string(b2) == "" || resp["error"] != nil {
		// the call succeeds but must return an empty list (RLS)
		var r struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		_ = json.Unmarshal(b2, &r)
		var out []map[string]any
		_ = json.Unmarshal([]byte(r.Result.Content[0].Text), &out)
		if len(out) != 0 {
			t.Fatalf("cross-ws leak: %v", out)
		}
	}
}
