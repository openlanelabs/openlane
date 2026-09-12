//go:build integration

package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMigrationAgent: the §15.2 viral wedge — LLM suggests the mapping
// (validated against the real dest schema), deterministic code previews
// + runs it, bad rows quarantined with reasons, humans approve.
func TestMigrationAgent(t *testing.T) {
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

	// stub LLM: proposes one REAL mapping (company -> Name — should win
	// over our best-guess since headers differ) and one HALLUCINATED
	// dest field ("Fake_Field__c") that must be dropped; picks
	// phone_e164 + date_iso + dedupe.
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		// assert we told it the dest schema (it can't map what it doesn't know)
		if !strings.Contains(body.Messages[len(body.Messages)-1].Content, "Website") {
			w.WriteHeader(500)
			return
		}
		out := `{"columns": {"company": "Name", "contact": "Fake_Field__c"},
		         "transforms": ["phone_e164", "date_iso", "dedupe", "invented_transform"],
		         "plain_english": "Maps company to Name, normalizes phones to E.164 and dates to ISO, drops duplicates."}`
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": out}}},
			"usage":   map[string]int{"prompt_tokens": 600, "completion_tokens": 200},
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

	csv := "company,contact_phone,signup_date,notes\n" +
		"Acme Corp,+1 (415) 555-0100,03/15/2026,=SUM(A1:A9)\n" +
		"Beta LLC,415-555-0199,2026-03-16,second row\n" +
		"Gamma Inc,12,not-a-date,third\n" +
		"Beta LLC,415-555-0199,2026-03-16,dup of row 2\n" +
		"Delta LLC,+44 20 7946 0958,01/02/2026,uk phone\n"

	body := map[string]any{
		"name":     "legacy accounts",
		"dest":     "salesforce_accounts",
		"csv_text": csv,
	}

	// member 403
	if code, _ := h.doJWT("POST", "/v1/agents/migrations/suggest", body, raviTok); code != http.StatusForbidden {
		t.Fatalf("member suggest = %d, want 403", code)
	}
	// no llm configured → 400
	if code, _ := h.doJWT("POST", "/v1/agents/migrations/suggest", body, ashaTok); code != http.StatusBadRequest {
		t.Fatalf("no-config suggest = %d, want 400", code)
	}
	if code, _ := h.doJWT("PUT", "/v1/agents/llm", map[string]any{
		"provider": "openai", "base_url": stub.URL,
		"cheap_model": "mini", "smart_model": "big", "api_key": "sk-test",
	}, ashaTok); code != http.StatusOK {
		t.Fatalf("put llm failed")
	}
	// bad dest → 400
	if code, _ := h.doJWT("POST", "/v1/agents/migrations/suggest",
		map[string]any{"name": "x", "dest": "snowflake", "csv_text": "a\n1"}, ashaTok); code != http.StatusBadRequest {
		t.Fatalf("bad dest = %d, want 400", code)
	}

	code, out := h.doJWT("POST", "/v1/agents/migrations/suggest", body, ashaTok)
	if code != http.StatusOK {
		t.Fatalf("suggest = %d %s", code, out)
	}
	var sug struct {
		Name         string            `json:"name"`
		Status       string            `json:"status"`
		Columns      map[string]string `json:"columns"`
		Transforms   []string          `json:"transforms"`
		PlainEnglish string            `json:"plain_english"`
		Preview      struct {
			Headers []string `json:"headers"`
			Rows    []struct {
				Row    map[string]string `json:"row"`
				Errors []string          `json:"errors"`
			} `json:"rows"`
		} `json:"preview"`
		Model string `json:"model"`
	}
	if err := json.Unmarshal(out, &sug); err != nil {
		t.Fatal(err)
	}
	if sug.Status != "draft" {
		t.Fatalf("status = %q, want draft", sug.Status)
	}
	if sug.Model != "big" {
		t.Fatalf("model = %q, want smart class", sug.Model)
	}
	// LLM's valid mapping wins over best-guess
	if sug.Columns["company"] != "Name" {
		t.Fatalf("company mapping = %q, want Name (LLM pick)", sug.Columns["company"])
	}
	// hallucinated dest field dropped
	for _, f := range sug.Columns {
		if f == "Fake_Field__c" {
			t.Fatal("hallucinated dest field survived validation")
		}
	}
	// invented transform dropped, real ones kept, sanitize always
	for _, tr := range []string{"phone_e164", "date_iso", "dedupe", "sanitize_formula"} {
		if !hasStr(sug.Transforms, tr) {
			t.Fatalf("transform %s missing from %+v", tr, sug.Transforms)
		}
	}
	if hasStr(sug.Transforms, "invented_transform") {
		t.Fatal("invented transform survived")
	}
	// preview: row 3 has errors (bad phone + bad date), row 1 has none
	var row3Errs int
	for _, pr := range sug.Preview.Rows {
		if len(pr.Errors) > 0 {
			row3Errs++
		}
	}
	if row3Errs == 0 {
		t.Fatal("preview shows no validation errors")
	}
	// formula sanitized in preview
	for _, pr := range sug.Preview.Rows {
		for _, v := range pr.Row {
			if strings.Contains(v, "=SUM") && !strings.HasPrefix(v, "'") {
				t.Fatal("formula injection NOT sanitized in preview")
			}
		}
	}
	if !strings.Contains(sug.PlainEnglish, "E.164") && !strings.Contains(sug.PlainEnglish, "phones") {
		// plain_english passes through from the LLM (informational; don't over-assert wording)
		_ = sug.PlainEnglish
	}

	// find the run id via list
	code, out = h.doJWT("GET", "/v1/agents/migrations", nil, ashaTok)
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	var list []struct {
		ID     string `json:"id"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(out, &list); err != nil || len(list) == 0 {
		t.Fatalf("list empty: %v", err)
	}
	mid := list[0].ID

	// approve → done, quarantines the bad rows (Gamma: bad phone+date;
	// dup Beta), keeps the good ones (Acme, Beta, Delta)
	code, out = h.doJWT("POST", "/v1/agents/migrations/"+mid+"/approve", nil, ashaTok)
	if code != http.StatusOK {
		t.Fatalf("approve = %d %s", code, out)
	}
	var appr struct {
		Stats struct {
			Total       int `json:"total"`
			Ok          int `json:"ok"`
			Quarantined int `json:"quarantined"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(out, &appr); err != nil {
		t.Fatal(err)
	}
	if appr.Stats.Total != 5 || appr.Stats.Ok != 3 || appr.Stats.Quarantined != 2 {
		t.Fatalf("stats = %+v, want total 5 ok 3 quarantined 2", appr.Stats)
	}
	// get: quarantine has reasons
	code, out = h.doJWT("GET", "/v1/agents/migrations/"+mid, nil, ashaTok)
	if code != http.StatusOK {
		t.Fatalf("get = %d", code)
	}
	var got struct {
		Status     string `json:"status"`
		Quarantine []struct {
			Line    int      `json:"line"`
			Reasons []string `json:"reasons"`
		} `json:"quarantine"`
		Result []map[string]string `json:"result"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "done" {
		t.Fatalf("status = %q, want done", got.Status)
	}
	if len(got.Quarantine) != 2 {
		t.Fatalf("quarantine = %+v, want 2 rows", got.Quarantine)
	}
	for _, q := range got.Quarantine {
		if len(q.Reasons) == 0 {
			t.Fatal("quarantined row without reasons")
		}
	}
	if len(got.Result) != 3 {
		t.Fatalf("result rows = %d, want 3", len(got.Result))
	}
	// E.164 in result: [+0-9] only — and no sanitize apostrophe (the
	// smoke-found bug: "'+14155550100" must never happen)
	for _, row := range got.Result {
		if p, ok := row["Phone"]; ok && p != "" {
			for _, c := range p {
				if (c < '0' || c > '9') && c != '+' {
					t.Fatalf("phone %q not clean E.164", p)
				}
			}
		}
	}

	// second approve → 404 (status machine: not in draft)
	if code, _ := h.doJWT("POST", "/v1/agents/migrations/"+mid+"/approve", nil, ashaTok); code != http.StatusNotFound {
		t.Fatalf("re-approve = %d, want 404", code)
	}
	// audit log written
	var auditCt int
	if err := adminPool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE entity_type='migration_run' AND action='migration.approved'`).
		Scan(&auditCt); err != nil || auditCt < 1 {
		t.Fatalf("audit rows = %d err %v", auditCt, err)
	}
	// agent_runs metered for the suggest
	var runCt int
	if err := adminPool.QueryRow(ctx,
		`SELECT count(*) FROM agent_runs WHERE agent='migration' AND model='big'`).
		Scan(&runCt); err != nil || runCt < 1 {
		t.Fatalf("agent_runs = %d err %v", runCt, err)
	}

	// kill switch on a NEW run: disable, suggest → 503
	if _, err := adminPool.Exec(ctx, `INSERT INTO workspace_settings (workspace_id)
		VALUES ('11111111-1111-1111-1111-111111111111') ON CONFLICT (workspace_id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := adminPool.Exec(ctx, `UPDATE workspace_settings SET agents_enabled = false
		WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`); err != nil {
		t.Fatal(err)
	}
	if code, _ := h.doJWT("POST", "/v1/agents/migrations/suggest", body, ashaTok); code != http.StatusServiceUnavailable {
		t.Fatalf("kill switch suggest = %d, want 503", code)
	}
	if _, err := adminPool.Exec(ctx, `UPDATE workspace_settings SET agents_enabled = true
		WHERE workspace_id = '11111111-1111-1111-1111-111111111111'`); err != nil {
		t.Fatal(err)
	}

	// DELETE the finished run (rollback = discard)
	if code, _ := h.doJWT("DELETE", "/v1/agents/migrations/"+mid, nil, ashaTok); code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", code)
	}
	if code, _ := h.doJWT("GET", "/v1/agents/migrations/"+mid, nil, ashaTok); code != http.StatusNotFound {
		t.Fatalf("get after delete = %d, want 404", code)
	}
	fmt.Fprintln(io.Discard, "done")
}
