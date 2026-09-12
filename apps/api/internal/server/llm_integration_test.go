//go:build integration

package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLLMRouter: config CRUD + provider drivers via httptest stubs +
// narration degrade + cost metering.
func TestLLMRouter(t *testing.T) {
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

	// provider stub: speaks openai + anthropic + ollama schemas
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.HasSuffix(r.URL.Path, "/chat/completions") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{"message": map[string]string{"content": "openai says do the migration first"}}},
				"usage":   map[string]int{"prompt_tokens": 120, "completion_tokens": 30},
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v1/messages") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"content": []map[string]any{{"text": "anthropic says prioritize the silent customer"}},
				"usage":   map[string]int{"input_tokens": 200, "output_tokens": 50},
			})
			return
		}
		if strings.HasSuffix(r.URL.Path, "/api/chat") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message": map[string]string{"content": "ollama local says ship it"},
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
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

	// member can't PUT
	if code, _ := h.doJWT("PUT", "/v1/agents/llm", map[string]any{"provider": "openai"}, raviTok); code != http.StatusForbidden {
		t.Fatalf("member put llm = %d, want 403", code)
	}

	put := func(body map[string]any) (int, []byte) {
		return h.doJWT("PUT", "/v1/agents/llm", body, ashaTok)
	}
	// bad provider, bad url, missing key
	if code, _ := put(map[string]any{"provider": "grok", "base_url": "http://127.0.0.1:1", "cheap_model": "c", "smart_model": "s", "api_key": "k"}); code != http.StatusBadRequest {
		t.Fatalf("bad provider = %d", code)
	}
	if code, _ := put(map[string]any{"provider": "openai", "base_url": "ftp://x", "cheap_model": "c", "smart_model": "s", "api_key": "k"}); code != http.StatusBadRequest {
		t.Fatalf("bad url = %d", code)
	}
	if code, b := put(map[string]any{"provider": "openai", "base_url": "http://127.0.0.1:1", "cheap_model": "c", "smart_model": "s", "api_key": ""}); code != http.StatusBadRequest {
		t.Fatalf("missing key = %d %s", code, b)
	}
	// member GET ok (configured:false)
	code, body := h.doJWT("GET", "/v1/agents/llm", nil, raviTok)
	if code != 200 || !strings.Contains(string(body), `"configured":false`) {
		t.Fatalf("member get llm = %d %s", code, body)
	}

	// PUT openai config against stub
	if code, b := put(map[string]any{"provider": "openai", "base_url": stub.URL, "cheap_model": "gpt-cheap", "smart_model": "gpt-smart", "api_key": "sk-test"}); code != http.StatusOK {
		t.Fatalf("put openai = %d %s", code, b)
	}
	// GET: configured, key NOT present
	code, body = h.doJWT("GET", "/v1/agents/llm", nil, ashaTok)
	if code != 200 || !strings.Contains(string(body), `"configured":true`) || strings.Contains(string(body), "sk-test") {
		t.Fatalf("get llm = %d %s", code, body)
	}
	// key round-trip: stored sealed, not plaintext
	var sealed string
	if err := adminPool.QueryRow(ctx,
		`SELECT api_key_sealed->>'key_enc' FROM llm_configs WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`).Scan(&sealed); err != nil || sealed == "" {
		t.Fatalf("sealed key missing: %v", err)
	}
	if raw, _ := base64.StdEncoding.DecodeString(sealed); strings.Contains(string(raw), "sk-test") {
		t.Fatal("key stored plaintext")
	}

	// narration: seed one stale approval signal first
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO projects (id, workspace_id, customer_id, name, status) VALUES
		  ('77777777-7777-7777-7777-777777777777', '11111111-1111-1111-1111-111111111111',
		   '33333333-3333-3333-3333-333333333333', 'LLM Test', 'active') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO approvals (workspace_id, project_id, title, status, created_at) VALUES
		  ('11111111-1111-1111-1111-111111111111', '77777777-7777-7777-7777-777777777777',
		   'SOW', 'pending', now() - interval '10 days')`); err != nil {
		t.Fatal(err)
	}
	code, body = h.doJWT("GET", "/v1/agents/signals?narrate=1", nil, ashaTok)
	if code != 200 {
		t.Fatalf("narrated signals = %d %s", code, body)
	}
	if !strings.Contains(string(body), "openai says do the migration first") {
		t.Fatalf("narrative missing: %s", body)
	}
	// cost + model recorded on the narrate agent_run
	var model string
	var cost int
	if err := adminPool.QueryRow(ctx,
		`SELECT model, cost_cents FROM agent_runs WHERE agent='signals' AND input_ref='signals:narrate' ORDER BY created_at DESC LIMIT 1`).
		Scan(&model, &cost); err != nil {
		t.Fatalf("narrate agent_run missing: %v", err)
	}
	if model != "gpt-cheap" {
		t.Fatalf("narrate model = %q, want gpt-cheap (cheap task class)", model)
	}
	if cost == 0 {
		t.Fatal("narrate cost = 0; openai usage should meter")
	}

	// anthropic driver
	if code, _ := put(map[string]any{"provider": "anthropic", "base_url": stub.URL, "cheap_model": "haiku", "smart_model": "sonnet", "api_key": "ak-test"}); code != http.StatusOK {
		t.Fatalf("put anthropic = %d", code)
	}
	if code, body = h.doJWT("GET", "/v1/agents/signals?narrate=1", nil, ashaTok); code != 200 || !strings.Contains(string(body), "anthropic says prioritize the silent customer") {
		t.Fatalf("anthropic narration = %d %s", code, body)
	}

	// ollama driver (no key required)
	if code, _ := put(map[string]any{"provider": "ollama", "base_url": stub.URL, "cheap_model": "llama3", "smart_model": "llama3", "api_key": ""}); code != http.StatusOK {
		t.Fatalf("put ollama = %d", code)
	}
	if code, body = h.doJWT("GET", "/v1/agents/signals?narrate=1", nil, ashaTok); code != 200 || !strings.Contains(string(body), "ollama local says ship it") {
		t.Fatalf("ollama narration = %d %s", code, body)
	}

	// host allowlist: evil.example accepted for storage but dial is guarded
	if code, _ := put(map[string]any{"provider": "openai", "base_url": "http://evil.example", "cheap_model": "c", "smart_model": "s", "api_key": "k"}); code != http.StatusOK {
		t.Fatalf("put should accept config (stored), dial is guarded")
	}
	if code, body = h.doJWT("GET", "/v1/agents/signals?narrate=1", nil, ashaTok); code != 200 || strings.Contains(string(body), "ship it") {
		t.Fatalf("blocked-host narration = %d %s (must degrade, no provider text)", code, body)
	}

	// delete config → degrade
	if _, err := adminPool.Exec(ctx, `DELETE FROM llm_configs WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`); err != nil {
		t.Fatal(err)
	}
	if code, body = h.doJWT("GET", "/v1/agents/signals?narrate=1", nil, ashaTok); code != 200 || strings.Contains(string(body), "ship it") {
		t.Fatalf("post-delete narration = %d %s (must degrade silently)", code, body)
	}
}
