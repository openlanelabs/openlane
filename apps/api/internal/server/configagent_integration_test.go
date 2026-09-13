//go:build integration

package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestConfigAgent: §15.3 — form response → validated setup sheet →
// human approve → deterministic task/budget creation.
func TestConfigAgent(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	t.Setenv("OPENLANE_INTEGRATION_KEY", base64.StdEncoding.EncodeToString(key))

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
	const projA = "55555555-5555-5555-5555-555555555555"

	// form + response for the order
	var formID, respID string
	if err := adminPool.QueryRow(ctx, `
		INSERT INTO forms (workspace_id, project_id, title, fields)
		VALUES ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555', 'Onboarding order',
		        '[{"key":"svc","label":"Which services?","type":"text","required":true},
		          {"key":"goLive","label":"Target go-live","type":"date","required":true}]'::jsonb)
		RETURNING id`).Scan(&formID); err != nil {
		t.Fatal(err)
	}
	if err := adminPool.QueryRow(ctx, `
		INSERT INTO form_responses (workspace_id, form_id, answers)
		VALUES ('11111111-1111-1111-1111-111111111111', $1::uuid,
		        '{"svc":"Data migration + weekly reporting","goLive":"2026-10-01"}'::jsonb)
		RETURNING id`, formID).Scan(&respID); err != nil {
		t.Fatal(err)
	}

	// stub LLM: proposes a good sheet + ONE invalid task (empty title) + one
	// out-of-range due_days — both must be stripped by validation.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		if !strings.Contains(body.Messages[len(body.Messages)-1].Content, "Data migration") {
			w.WriteHeader(500)
			return
		}
		out := `{"customer_goals":"Migrate data and get weekly reporting by October",
		         "tasks":[{"title":"Scoping workshop","owner_type":"internal","due_days":3,"customer_visible":false},
		                  {"title":"","owner_type":"internal","due_days":3,"customer_visible":false},
		                  {"title":"Data migration run","owner_type":"internal","due_days":999,"customer_visible":false},
		                  {"title":"Weekly report setup","owner_type":"internal","due_days":10,"customer_visible":true}],
		         "budget_hours":120,"notes":"Budget set from migration scope"}`
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": out}}},
			"usage":   map[string]int{"prompt_tokens": 500, "completion_tokens": 200},
		})
	}))
	defer stub.Close()

	acode, _, ashaTok := devLogin(h)
	if acode != 200 {
		t.Fatalf("asha login = %d", acode)
	}
	rcode, _, raviTok := loginAs(h, "ravi@acme.test")
	if rcode != 200 {
		t.Fatalf("ravi login = %d", rcode)
	}

	ask := map[string]any{"project_id": projA, "form_response_id": respID}

	// member 403 + no-LLM 400
	if code, _ := h.doJWT("POST", "/v1/agents/config/sheet", ask, raviTok); code != http.StatusForbidden {
		t.Fatalf("member sheet = %d, want 403", code)
	}
	if code, _ := h.doJWT("POST", "/v1/agents/config/sheet", ask, ashaTok); code != http.StatusBadRequest {
		t.Fatalf("no-config sheet = %d, want 400", code)
	}
	if code, _ := h.doJWT("PUT", "/v1/agents/llm", map[string]any{
		"provider": "openai", "base_url": stub.URL,
		"cheap_model": "mini", "smart_model": "big", "api_key": "sk-test",
	}, ashaTok); code != http.StatusOK {
		t.Fatal("put llm failed")
	}
	// unknown response → 404 (RLS scope)
	bad := map[string]any{"project_id": projA, "form_response_id": "99999999-9999-9999-9999-999999999999"}
	if code, _ := h.doJWT("POST", "/v1/agents/config/sheet", bad, ashaTok); code != http.StatusNotFound {
		t.Fatalf("unknown response = %d, want 404", code)
	}

	code, out := h.doJWT("POST", "/v1/agents/config/sheet", ask, ashaTok)
	if code != http.StatusOK {
		t.Fatalf("sheet = %d %s", code, out)
	}
	var res struct {
		ConfigSheetID string `json:"config_sheet_id"`
		Sheet         struct {
			CustomerGoals string            `json:"customer_goals"`
			Tasks         []configSheetTask `json:"tasks"`
			BudgetHours   *int              `json:"budget_hours"`
		} `json:"sheet"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.Model != "big" {
		t.Fatalf("model = %q, want smart", res.Model)
	}
	// validation stripped the empty-title + 999-day tasks; kept the 2 good ones
	if len(res.Sheet.Tasks) != 2 {
		t.Fatalf("tasks = %+v, want 2 (invalid stripped)", res.Sheet.Tasks)
	}
	if res.Sheet.Tasks[0].Title != "Scoping workshop" {
		t.Fatalf("task0 = %+v", res.Sheet.Tasks[0])
	}
	if res.Sheet.BudgetHours == nil || *res.Sheet.BudgetHours != 120 {
		t.Fatalf("budget = %v, want 120", res.Sheet.BudgetHours)
	}

	// approve → deterministic execution: 2 tasks + budget set
	code, out = h.doJWT("POST", "/v1/agents/config/"+res.ConfigSheetID+"/approve", nil, ashaTok)
	if code != http.StatusOK {
		t.Fatalf("approve = %d %s", code, out)
	}
	var appr struct {
		Status string `json:"status"`
		Refs   struct {
			TaskIDs []string `json:"task_ids"`
		} `json:"refs"`
	}
	if err := json.Unmarshal(out, &appr); err != nil {
		t.Fatal(err)
	}
	if appr.Status != "executed" || len(appr.Refs.TaskIDs) != 2 {
		t.Fatalf("approve = %+v", appr)
	}
	// tasks + budget actually landed
	var taskCt int
	var newBudget *int
	if err := adminPool.QueryRow(ctx,
		`SELECT count(*) FROM tasks WHERE id = ANY($1::uuid[])`,
		appr.Refs.TaskIDs).Scan(&taskCt); err != nil || taskCt != 2 {
		t.Fatalf("created tasks = %d err %v", taskCt, err)
	}
	if err := adminPool.QueryRow(ctx,
		`SELECT budget_hours FROM projects WHERE id = '55555555-5555-5555-5555-555555555555'`).
		Scan(&newBudget); err != nil || newBudget == nil || *newBudget != 120 {
		t.Fatalf("budget = %v err %v", newBudget, err)
	}
	// re-approve 404 (state machine)
	if code, _ := h.doJWT("POST", "/v1/agents/config/"+res.ConfigSheetID+"/approve", nil, ashaTok); code != http.StatusNotFound {
		t.Fatalf("re-approve = %d, want 404", code)
	}
	// audit + metered
	var auditCt, runCt int
	_ = adminPool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE entity_type='config_sheet' AND action='config.executed'`).Scan(&auditCt)
	_ = adminPool.QueryRow(ctx,
		`SELECT count(*) FROM agent_runs WHERE agent='workforce' AND model='big'`).Scan(&runCt)
	if auditCt < 1 || runCt < 1 {
		t.Fatalf("audit=%d runs=%d", auditCt, runCt)
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
	if code, _ := h.doJWT("POST", "/v1/agents/config/sheet", ask, ashaTok); code != http.StatusServiceUnavailable {
		t.Fatalf("kill switch sheet = %d, want 503", code)
	}
	if _, err := adminPool.Exec(ctx, `UPDATE workspace_settings SET agents_enabled = true
		WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`); err != nil {
		t.Fatal(err)
	}
}
