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

// TestAnalystAgent: NL → curated query. The LLM never writes SQL —
// it picks from the catalog; slots are validated server-side; the
// narration must echo row numbers; cross-tenant params rejected by
// RLS scoping; everything metered.
func TestAnalystAgent(t *testing.T) {
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
	// data for the queries: an overdue customer-visible task + time logged
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO tasks (workspace_id, project_id, title, owner_type, customer_visible, due_at, status)
		SELECT '11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555',
		       'Overdue thing', 'customer', true, now() - interval '2 days', 'todo'`); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO time_entries (workspace_id, project_id, user_id, started_at, ended_at, minutes)
		VALUES ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555',
		        'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', now() - interval '3 hours', now() - interval '1 hour', 120)`); err != nil {
		t.Fatal(err)
	}

	// stub LLM: first call = smart pick (hours_by_person), second =
	// cheap narration. A MALICIOUS pick is tested separately below.
	call := 0
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call++
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = raw
		if call == 1 {
			out := `{"query": "hours_by_person", "params": {"window_days": "30"}}`
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]string{"content": out}}},
				"usage":   map[string]int{"prompt_tokens": 300, "completion_tokens": 40},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "Asha logged 120 minutes."}}},
			"usage":   map[string]int{"prompt_tokens": 200, "completion_tokens": 30},
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

	ask := map[string]any{"question": "who logged the most hours this month?"}

	// member 403
	if code, _ := h.doJWT("POST", "/v1/agents/analyst/query", ask, raviTok); code != http.StatusForbidden {
		t.Fatalf("member analyst = %d, want 403", code)
	}
	// no llm → 400
	if code, _ := h.doJWT("POST", "/v1/agents/analyst/query", ask, ashaTok); code != http.StatusBadRequest {
		t.Fatalf("no-config analyst = %d, want 400", code)
	}
	if code, _ := h.doJWT("PUT", "/v1/agents/llm", map[string]any{
		"provider": "openai", "base_url": stub.URL,
		"cheap_model": "mini", "smart_model": "big", "api_key": "sk-test",
	}, ashaTok); code != http.StatusOK {
		t.Fatal("put llm failed")
	}

	// happy path
	code, out := h.doJWT("POST", "/v1/agents/analyst/query", ask, ashaTok)
	if code != http.StatusOK {
		t.Fatalf("analyst = %d %s", code, out)
	}
	var res struct {
		Query     string           `json:"query"`
		Columns   []string         `json:"columns"`
		Rows      []map[string]any `json:"rows"`
		Narrative string           `json:"narrative"`
		Model     string           `json:"model"`
		CostCents int              `json:"cost_cents"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.Query != "hours_by_person" {
		t.Fatalf("query = %q, want hours_by_person", res.Query)
	}
	if len(res.Rows) == 0 {
		t.Fatal("no rows — hours_by_person should find the seeded 120m entry")
	}
	if !strings.Contains(res.Narrative, "120") {
		t.Fatalf("narrative %q doesn't echo the row numbers", res.Narrative)
	}
	if res.CostCents == 0 {
		t.Fatal("cost not metered")
	}
	// the rows are workspace-scoped: no Beta ws rows (RLS)
	for _, row := range res.Rows {
		if v, _ := row["email"].(string); strings.HasPrefix(v, "beta@") {
			t.Fatal("cross-workspace row leaked")
		}
	}
	// metered
	var runCt int
	if err := adminPool.QueryRow(ctx,
		`SELECT count(*) FROM agent_runs WHERE agent='analyst'`).Scan(&runCt); err != nil || runCt < 1 {
		t.Fatalf("agent_runs = %d err %v", runCt, err)
	}

	// malicious pick: unknown query → 400 with hint
	evil1 := `{"query": "DROP TABLE users; --", "params": {}}`
	call = 0
	stub2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = raw
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": evil1}}},
			"usage":   map[string]int{"prompt_tokens": 100, "completion_tokens": 10},
		})
	}))
	defer stub2.Close()
	if code, _ := h.doJWT("PUT", "/v1/agents/llm", map[string]any{
		"provider": "openai", "base_url": stub2.URL,
		"cheap_model": "mini", "smart_model": "big", "api_key": "sk-test",
	}, ashaTok); code != http.StatusOK {
		t.Fatal("repoint llm failed")
	}
	code, out = h.doJWT("POST", "/v1/agents/analyst/query", ask, ashaTok)
	if code != http.StatusBadRequest {
		t.Fatalf("sql-injection pick = %d, want 400", code)
	}

	// malicious slot value: window_days = "30; DROP TABLE users"
	evil2 := `{"query": "hours_by_person", "params": {"window_days": "30; DROP TABLE users"}}`
	stub3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = raw
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": evil2}}},
			"usage":   map[string]int{"prompt_tokens": 100, "completion_tokens": 10},
		})
	}))
	defer stub3.Close()
	if code, _ := h.doJWT("PUT", "/v1/agents/llm", map[string]any{
		"provider": "openai", "base_url": stub3.URL,
		"cheap_model": "mini", "smart_model": "big", "api_key": "sk-test",
	}, ashaTok); code != http.StatusOK {
		t.Fatal("repoint llm 3 failed")
	}
	code, _ = h.doJWT("POST", "/v1/agents/analyst/query", ask, ashaTok)
	if code != http.StatusBadRequest {
		t.Fatalf("slot injection = %d, want 400", code)
	}
	// users table survived
	var n int
	if err := adminPool.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&n); err != nil || n == 0 {
		t.Fatal("users table damaged by injection attempt")
	}

	// null query (no catalog match) → 400 body with query:null + hint
	stub4 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		_ = raw
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": `{"query": null}`}}},
			"usage":   map[string]int{"prompt_tokens": 100, "completion_tokens": 10},
		})
	}))
	defer stub4.Close()
	if code, _ := h.doJWT("PUT", "/v1/agents/llm", map[string]any{
		"provider": "openai", "base_url": stub4.URL,
		"cheap_model": "mini", "smart_model": "big", "api_key": "sk-test",
	}, ashaTok); code != http.StatusOK {
		t.Fatal("repoint llm 4 failed")
	}
	code, out = h.doJWT("POST", "/v1/agents/analyst/query",
		map[string]any{"question": "what is the meaning of life?"}, ashaTok)
	if code != http.StatusBadRequest {
		t.Fatalf("null query = %d, want 400", code)
	}
	if !strings.Contains(string(out), "revenue_by_customer") {
		t.Fatal("hint missing from null-query response")
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
	if code, _ := h.doJWT("POST", "/v1/agents/analyst/query", ask, ashaTok); code != http.StatusServiceUnavailable {
		t.Fatalf("kill switch analyst = %d, want 503", code)
	}
	if _, err := adminPool.Exec(ctx, `UPDATE workspace_settings SET agents_enabled = true
		WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`); err != nil {
		t.Fatal(err)
	}
}
