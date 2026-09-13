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

// TestAssistantAgent: the final Nitro agent — QBR deck grounded in
// real project numbers, uncited fabrication flagged, draft internal.
func TestAssistantAgent(t *testing.T) {
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
	// grounding data: person cost 200, rate 100, 600m approved on projA
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO people (workspace_id, user_id, name, role, cost_rate, capacity_hrs, active)
		VALUES ('11111111-1111-1111-1111-111111111111', 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa',
			'Asha', 'admin', 200, 40, true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO rate_cards (workspace_id, name, is_active)
		VALUES ('11111111-1111-1111-1111-111111111111', 'Asst Default', true)`); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO rate_card_rates (workspace_id, rate_card_id, role, hourly_rate)
		SELECT '11111111-1111-1111-1111-111111111111', rc.id, 'admin', 100
		FROM rate_cards rc WHERE rc.workspace_id = '11111111-1111-1111-1111-111111111111'
		  AND rc.name = 'Asst Default'`); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO time_entries (workspace_id, project_id, user_id, started_at, ended_at, minutes, status)
		VALUES ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555',
			'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa', now() - interval '10 hours', now() - interval '9 hours',
			600, 'approved')`); err != nil {
		t.Fatal(err)
	}

	// stub: asserts grounding is in the prompt; returns a deck with one
	// grounded paragraph + one FABRICATED number paragraph (uncited).
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		prompt := body.Messages[len(body.Messages)-1].Content
		if !strings.Contains(prompt, `"Billed":1000`) && !strings.Contains(prompt, `"billed":1000`) {
			w.WriteHeader(500)
			return
		}
		deck := "## Executive Summary\n\nProject billed 1000 against 2000 cost so far. [source grounding]\n\n" +
			"NPS jumped to 92 this quarter and CSAT is the best in company history.\n\n" +
			"## Budget & Margin\n\nMargin is deeply negative; rebalance rates. [source grounding]\n\n" +
			"## Talk Track\n\n- Lead with the delivery recovery. [source grounding]"
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": deck}}},
			"usage":   map[string]int{"prompt_tokens": 800, "completion_tokens": 300},
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

	ask := map[string]any{"project_id": "55555555-5555-5555-5555-555555555555", "kind": "qbr"}

	// member 403, no-LLM 400, bad kind 400, unknown project 404
	if code, _ := h.doJWT("POST", "/v1/agents/assistant/deck", ask, raviTok); code != http.StatusForbidden {
		t.Fatalf("member deck = %d, want 403", code)
	}
	if code, _ := h.doJWT("POST", "/v1/agents/assistant/deck", ask, ashaTok); code != http.StatusBadRequest {
		t.Fatalf("no-config deck = %d, want 400", code)
	}
	if code, _ := h.doJWT("POST", "/v1/agents/assistant/deck", map[string]any{
		"project_id": "55555555-5555-5555-5555-555555555555", "kind": "party"}, ashaTok); code != http.StatusBadRequest {
		t.Fatalf("bad kind = %d, want 400", code)
	}
	if code, _ := h.doJWT("PUT", "/v1/agents/llm", map[string]any{
		"provider": "openai", "base_url": stub.URL,
		"cheap_model": "mini", "smart_model": "big", "api_key": "sk-test",
	}, ashaTok); code != http.StatusOK {
		t.Fatal("put llm failed")
	}
	if code, _ := h.doJWT("POST", "/v1/agents/assistant/deck", map[string]any{
		"project_id": "99999999-9999-9999-9999-999999999999", "kind": "qbr"}, ashaTok); code != http.StatusNotFound {
		t.Fatalf("unknown project = %d, want 404", code)
	}

	code, out := h.doJWT("POST", "/v1/agents/assistant/deck", ask, ashaTok)
	if code != http.StatusOK {
		t.Fatalf("deck = %d %s", code, out)
	}
	var res struct {
		DocID     string        `json:"doc_id"`
		Uncited   []uncitedSpan `json:"uncited"`
		Grounding deckGrounding `json:"grounding"`
		Model     string        `json:"model"`
		CostCents int           `json:"cost_cents"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if res.Model != "big" {
		t.Fatalf("model = %q, want smart", res.Model)
	}
	if res.CostCents == 0 {
		t.Fatal("not metered")
	}
	// grounding echoed + real
	if res.Grounding.Billed != 1000 || res.Grounding.Cost != 2000 {
		t.Fatalf("grounding = %+v, want billed 1000 cost 2000", res.Grounding)
	}
	if res.Grounding.MarginPct == nil || *res.Grounding.MarginPct != -100 {
		t.Fatalf("margin = %v, want -100", res.Grounding.MarginPct)
	}
	// fabricated paragraph flagged
	if len(res.Uncited) != 1 || !strings.Contains(res.Uncited[0].Text, "NPS") {
		t.Fatalf("uncited = %+v, want the NPS fabrication", res.Uncited)
	}
	// deck saved internal + versioned
	var vis bool
	var content string
	if err := adminPool.QueryRow(ctx,
		`SELECT d.customer_visible, v.content_md FROM docs d JOIN doc_versions v ON v.doc_id = d.id
		 WHERE d.id = $1::uuid AND v.version = 1`, res.DocID).Scan(&vis, &content); err != nil {
		t.Fatalf("doc not saved: %v", err)
	}
	if vis {
		t.Fatal("deck must not be customer_visible")
	}
	if !strings.Contains(content, "Talk Track") {
		t.Fatal("deck content missing talk track")
	}
	var title string
	if err := adminPool.QueryRow(ctx, `SELECT title FROM docs WHERE id = $1::uuid`, res.DocID).
		Scan(&title); err != nil || !strings.Contains(title, "QBR") {
		t.Fatalf("title = %q err %v", title, err)
	}
	// metered as assistant
	var runCt int
	if err := adminPool.QueryRow(ctx,
		`SELECT count(*) FROM agent_runs WHERE agent='assistant' AND model='big'`).
		Scan(&runCt); err != nil || runCt < 1 {
		t.Fatalf("assistant runs = %d err %v", runCt, err)
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
	if code, _ := h.doJWT("POST", "/v1/agents/assistant/deck", ask, ashaTok); code != http.StatusServiceUnavailable {
		t.Fatalf("kill switch deck = %d, want 503", code)
	}
	if _, err := adminPool.Exec(ctx, `UPDATE workspace_settings SET agents_enabled = true
		WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`); err != nil {
		t.Fatal(err)
	}
}
