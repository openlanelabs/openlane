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

// TestDocAgent: draft generation w/ citation contract — uncited
// paragraphs flagged, PII masked before the provider sees sources,
// draft saved as versioned doc, cost metered, kill switch, roles.
func TestDocAgent(t *testing.T) {
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
	// project to draft against (filesTestStack seeds projA 5555 in wsA)
	const projA = "55555555-5555-5555-5555-555555555555"

	// LLM stub: captures the prompt (to assert PII masking), returns a
	// draft with one cited + one uncited paragraph.
	var capturedPrompt string
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		for _, m := range body.Messages {
			capturedPrompt += m.Content + "\n"
		}
		draft := "# SOW\n\n## Scope\n\nWe will build the integration platform. [source call1]\n\n" +
			"The platform will support unlimited users at no extra charge.\n\n" +
			"## Timeline\n\nKickoff next week. [source call1]"
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": draft}}},
			"usage":   map[string]int{"prompt_tokens": 400, "completion_tokens": 120},
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

	body := map[string]any{
		"project_id":  projA,
		"title":       "Acme Integration SOW",
		"template_md": "# SOW\n\n## Scope\n\n## Timeline\n",
		"sources": []map[string]string{
			{"label": "call1", "text": "Customer wants the integration built by Q3. Contact john.doe@corp.example or +1 415 555 0100."},
		},
	}

	// member 403
	if code, _ := h.doJWT("POST", "/v1/agents/doc/draft", body, raviTok); code != http.StatusForbidden {
		t.Fatalf("member draft = %d, want 403", code)
	}
	// no llm configured → 400 (explicit, not silent)
	if code, b := h.doJWT("POST", "/v1/agents/doc/draft", body, ashaTok); code != http.StatusBadRequest {
		t.Fatalf("no-config draft = %d %s, want 400", code, b)
	}

	// configure the stub as openai-compatible
	if code, _ := h.doJWT("PUT", "/v1/agents/llm", map[string]any{
		"provider": "openai", "base_url": stub.URL,
		"cheap_model": "mini", "smart_model": "big", "api_key": "sk-test",
	}, ashaTok); code != http.StatusOK {
		t.Fatalf("put llm = %d", code)
	}

	// unknown project → 404 (must come AFTER config so 400 doesn't mask it)
	bad := map[string]any{"project_id": "99999999-9999-9999-9999-999999999999",
		"title": "X", "template_md": "T", "sources": body["sources"]}
	if code, _ := h.doJWT("POST", "/v1/agents/doc/draft", bad, ashaTok); code != http.StatusNotFound {
		t.Fatalf("unknown project = %d, want 404", code)
	}

	code, out := h.doJWT("POST", "/v1/agents/doc/draft", body, ashaTok)
	if code != http.StatusOK {
		t.Fatalf("draft = %d %s", code, out)
	}
	var res struct {
		DocID     string        `json:"doc_id"`
		Uncited   []uncitedSpan `json:"uncited"`
		Model     string        `json:"model"`
		CostCents int           `json:"cost_cents"`
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	// smart task class
	if res.Model != "big" {
		t.Fatalf("draft model = %q, want big (smart class)", res.Model)
	}
	if res.CostCents == 0 {
		t.Fatal("cost not metered")
	}
	// the uncited paragraph is flagged (the unlimited-users hallucination)
	if len(res.Uncited) != 1 || !strings.Contains(res.Uncited[0].Text, "unlimited users") {
		t.Fatalf("uncited = %+v, want the unlimited-users paragraph", res.Uncited)
	}
	// PII masked before provider
	if strings.Contains(capturedPrompt, "john.doe@corp.example") || strings.Contains(capturedPrompt, "+1 415 555 0100") {
		t.Fatal("PII leaked to provider in prompt")
	}
	if !strings.Contains(capturedPrompt, "[email-redacted]") || !strings.Contains(capturedPrompt, "[phone-redacted]") {
		t.Fatal("masked markers missing in prompt")
	}
	// draft saved: doc + version 1 + customer_visible false + agent_runs row
	var vis bool
	var content string
	if err := adminPool.QueryRow(ctx,
		`SELECT d.customer_visible, v.content_md FROM docs d JOIN doc_versions v ON v.doc_id = d.id
		 WHERE d.id = $1::uuid AND v.version = 1`, res.DocID).Scan(&vis, &content); err != nil {
		t.Fatalf("doc not saved: %v", err)
	}
	if vis {
		t.Fatal("draft must not be customer_visible")
	}
	if !strings.Contains(content, "integration platform") {
		t.Fatal("draft content not versioned")
	}
	var runModel string
	if err := adminPool.QueryRow(ctx,
		"SELECT model FROM agent_runs WHERE agent='doc' AND output_ref = $1", "doc:"+res.DocID).
		Scan(&runModel); err != nil || runModel != "big" {
		t.Fatalf("doc agent_run = %q err %v", runModel, err)
	}

	// kill switch → 503
	if _, err := adminPool.Exec(ctx, `INSERT INTO workspace_settings (workspace_id)
		VALUES ('11111111-1111-1111-1111-111111111111') ON CONFLICT (workspace_id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `UPDATE workspace_settings SET agents_enabled = false
		WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.doJWT("POST", "/v1/agents/doc/draft", body, ashaTok); code != http.StatusServiceUnavailable {
		t.Fatalf("kill switch draft = %d, want 503", code)
	}
	if _, err := adminPool.Exec(ctx, `UPDATE workspace_settings SET agents_enabled = true
		WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`); err != nil {
		t.Fatal(err)
	}
}
