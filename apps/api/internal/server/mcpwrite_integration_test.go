//go:build integration

package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// MCP write tools + staff decide execution (§347/§537):
// agents propose (pending approval w/ payload), humans commit
// (decide route executes task/time payloads), rejections write
// nothing, duplicates are blocked, kill switch gates everything.

func TestMcpWriteToolsApprovalFlow(t *testing.T) {
	_, pool, h := filesTestStack(t)
	ap, err := pgxpool.New(context.Background(), adminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ap.Close() })

	// seed an owner user (time-entry attribution needs a real user when
	// the caller is the static/system identity)
	sctx := context.Background()
	if _, err := ap.Exec(sctx, `INSERT INTO users (id, email, display_name)
		VALUES ('aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa','asha@acme.test','Asha') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Exec(sctx, `INSERT INTO memberships (workspace_id, user_id, role)
		VALUES ($1, 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', 'owner') ON CONFLICT DO NOTHING`, wsA); err != nil {
		t.Fatal(err)
	}

	m := NewMcpServer(pool)
	ws := wsA
	ctx := context.Background()

	call := func(name string, args map[string]any) map[string]any {
		resp := m.Dispatch(ctx, ws, map[string]any{
			"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args},
		})
		if resp["error"] != nil {
			t.Fatalf("%s error: %v", name, resp["error"])
		}
		return resp
	}

	// tools/list includes the write tools
	resp := m.Dispatch(ctx, ws, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	var list struct {
		Result struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	b, _ := json.Marshal(resp)
	_ = json.Unmarshal(b, &list)
	names := map[string]bool{}
	for _, tl := range list.Result.Tools {
		n, _ := tl["name"].(string)
		names[n] = true
	}
	if !names["propose_task"] || !names["propose_time"] {
		t.Fatalf("write tools missing from tools/list: %v", names)
	}

	// --- propose_task: creates pending approval, writes nothing
	args := map[string]any{"project_id": projA, "title": "Agent task", "description": "from mcp"}
	resp = call("propose_task", args)
	var res struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	b, _ = json.Marshal(resp)
	_ = json.Unmarshal(b, &res)
	var prop struct {
		ApprovalID string `json:"approval_id"`
	}
	if len(res.Result.Content) == 0 || json.Unmarshal([]byte(res.Result.Content[0].Text), &prop) != nil {
		t.Fatalf("propose_task content: %v", res)
	}
	if prop.ApprovalID == "" {
		t.Fatal("propose_task returned no approval_id")
	}
	var n int
	if err := ap.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE title = 'Agent task'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("task written before approval: n=%d", n)
	}

	// --- duplicate pending: same title → tool error, no second row
	resp = m.Dispatch(ctx, ws, map[string]any{
		"jsonrpc": "2.0", "id": 3, "method": "tools/call",
		"params": map[string]any{"name": "propose_task", "arguments": args},
	})
	if resp["error"] == nil {
		t.Fatal("duplicate pending should error")
	}
	if err := ap.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE title = 'Task: Agent task' AND status = 'pending'`).
		Scan(&n); err != nil || n != 1 {
		t.Fatalf("duplicate created a second row: n=%d", n)
	}

	// --- staff approve executes the task payload
	code, out := h.doWS("POST", "/v1/approvals/"+prop.ApprovalID+"/decide",
		map[string]string{"decision": "approved", "comment": "go"}, ws)
	if code != 200 {
		t.Fatalf("decide code=%d out=%s", code, out)
	}
	var dres struct {
		Status   string `json:"status"`
		Executed string `json:"executed"`
	}
	if err := json.Unmarshal(out, &dres); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dres.Executed, "task ") {
		t.Fatalf("executed = %q", dres.Executed)
	}
	if err := ap.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE title = 'Agent task' AND deleted_at IS NULL`).
		Scan(&n); err != nil || n != 1 {
		t.Fatalf("task not created on approve: n=%d", n)
	}

	// --- propose_time → approve → draft time entry
	resp = call("propose_time", map[string]any{"project_id": projA, "minutes": 90, "note": "mcp"})
	b, _ = json.Marshal(resp)
	_ = json.Unmarshal(b, &res)
	if json.Unmarshal([]byte(res.Result.Content[0].Text), &prop) != nil || prop.ApprovalID == "" {
		t.Fatal("propose_time returned no approval_id")
	}
	code, out = h.doWS("POST", "/v1/approvals/"+prop.ApprovalID+"/decide",
		map[string]string{"decision": "approved"}, ws)
	if code != 200 {
		t.Fatalf("decide code=%d out=%s", code, out)
	}
	if err := ap.QueryRow(ctx, `SELECT count(*) FROM time_entries WHERE minutes = 90 AND status = 'draft'`).
		Scan(&n); err != nil || n != 1 {
		t.Fatalf("time entry not created: n=%d", n)
	}

	// --- propose + REJECT → nothing written
	resp = call("propose_task", map[string]any{"project_id": projA, "title": "Rejected task"})
	b, _ = json.Marshal(resp)
	_ = json.Unmarshal(b, &res)
	if json.Unmarshal([]byte(res.Result.Content[0].Text), &prop) != nil || prop.ApprovalID == "" {
		t.Fatal("propose returned no approval_id")
	}
	code, out = h.doWS("POST", "/v1/approvals/"+prop.ApprovalID+"/decide",
		map[string]string{"decision": "changes_requested"}, ws)
	if code != 200 {
		t.Fatalf("reject code=%d out=%s", code, out)
	}
	if err := ap.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE title = 'Rejected task'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rejected task was written: n=%d", n)
	}

	// --- member cannot decide (403): ravi is member of wsA
	if _, err := ap.Exec(sctx, `INSERT INTO users (id, email, display_name)
		VALUES ('bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb','ravi@acme.test','Ravi') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Exec(sctx, `INSERT INTO memberships (workspace_id, user_id, role)
		VALUES ($1, 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'member') ON CONFLICT DO NOTHING`, wsA); err != nil {
		t.Fatal(err)
	}
	_, _, raviTok := loginAs(h, "ravi@acme.test")
	resp = call("propose_task", map[string]any{"project_id": projA, "title": "Member gate"})
	b, _ = json.Marshal(resp)
	_ = json.Unmarshal(b, &res)
	if json.Unmarshal([]byte(res.Result.Content[0].Text), &prop) != nil || prop.ApprovalID == "" {
		t.Fatal("propose returned no approval_id")
	}
	if code, _ := h.doJWT("POST", "/v1/approvals/"+prop.ApprovalID+"/decide",
		map[string]string{"decision": "approved"}, raviTok); code != 403 {
		t.Fatalf("member decide = %d, want 403", code)
	}
	// decide it as admin so reruns don't trip the pending-dup guard
	if code, _ := h.doWS("POST", "/v1/approvals/"+prop.ApprovalID+"/decide",
		map[string]string{"decision": "approved"}, ws); code != 200 {
		t.Fatalf("admin cleanup decide = %d", code)
	}

	// double-decide on a fresh proposal: first succeeds, second 404s.
	resp = call("propose_task", map[string]any{"project_id": projA, "title": "Double decide"})
	b, _ = json.Marshal(resp)
	_ = json.Unmarshal(b, &res)
	if json.Unmarshal([]byte(res.Result.Content[0].Text), &prop) != nil || prop.ApprovalID == "" {
		t.Fatal("propose returned no approval_id")
	}
	code, _ = h.doWS("POST", "/v1/approvals/"+prop.ApprovalID+"/decide",
		map[string]string{"decision": "approved"}, ws)
	if code != 200 {
		t.Fatalf("first decide code=%d", code)
	}
	code, _ = h.doWS("POST", "/v1/approvals/"+prop.ApprovalID+"/decide",
		map[string]string{"decision": "approved"}, ws)
	if code == 200 {
		t.Fatal("double-decide should not succeed")
	}

	// --- validation: bad minutes
	if resp := m.Dispatch(ctx, ws, map[string]any{
		"jsonrpc": "2.0", "id": 9, "method": "tools/call",
		"params": map[string]any{"name": "propose_time", "arguments": map[string]any{"project_id": projA, "minutes": 9999}},
	}); resp["error"] == nil {
		t.Fatal("minutes=9999 should error")
	}

	// --- kill switch gates write tools too
	if _, err := ap.Exec(ctx, `INSERT INTO workspace_settings (workspace_id) VALUES ($1)
		ON CONFLICT (workspace_id) DO NOTHING`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Exec(ctx, `UPDATE workspace_settings SET agents_enabled = false WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if resp := m.Dispatch(ctx, ws, map[string]any{
		"jsonrpc": "2.0", "id": 10, "method": "tools/call",
		"params": map[string]any{"name": "propose_task", "arguments": map[string]any{"project_id": projA, "title": "Nope"}},
	}); resp["error"] == nil {
		t.Fatal("kill switch should block propose_task")
	}
}
