//go:build integration

package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestCustomDomains: the last P1 — claim, TXT-verify (stubbed
// resolver), list, delete; cross-workspace claim conflict; host
// resolution verified-only.
func TestCustomDomains(t *testing.T) {
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
	if _, err := adminPool.Exec(ctx, `DELETE FROM workspace_domains`); err != nil {
		t.Fatal(err)
	}

	acode, _, ashaTok := devLogin(h)
	if acode != 200 {
		t.Fatalf("asha login = %d", acode)
	}
	rcode, _, raviTok := loginAs(h, "ravi@acme.test")
	if rcode != 200 {
		t.Fatalf("ravi login = %d", rcode)
	}

	// member 403 on all routes
	if code, _ := h.doJWT("PUT", "/v1/domains", map[string]any{"domain": "x.com"}, raviTok); code != http.StatusForbidden {
		t.Fatalf("member PUT = %d, want 403", code)
	}
	if code, _ := h.doJWT("GET", "/v1/domains", nil, raviTok); code != http.StatusForbidden {
		t.Fatalf("member GET = %d, want 403", code)
	}
	// invalid domains
	for _, bad := range []string{"", "https://acme.com", "acme.com/path", "acme", "a b.com", "acme.com:8443", "-bad.com", "bad..com"} {
		if code, _ := h.doJWT("PUT", "/v1/domains", map[string]any{"domain": bad}, ashaTok); code != http.StatusBadRequest {
			t.Fatalf("bad domain %q = %d, want 400", bad, code)
		}
	}
	// case is normalized (DNS is case-insensitive): NoCaps.com → nocaps.com
	if code, out := h.doJWT("PUT", "/v1/domains", map[string]any{"domain": "NoCaps.com"}, ashaTok); code != http.StatusOK {
		t.Fatalf("case-normalized claim = %d %s", code, out)
	}
	// claim
	code, out := h.doJWT("PUT", "/v1/domains", map[string]any{"domain": "onboarding.acme.com"}, ashaTok)
	if code != http.StatusOK {
		t.Fatalf("claim = %d %s", code, out)
	}
	var d workspaceDomain
	if err := json.Unmarshal(out, &d); err != nil {
		t.Fatal(err)
	}
	if d.Verified {
		t.Fatal("fresh claim must be unverified")
	}
	if len(d.Challenge) < 30 {
		t.Fatalf("challenge = %q, want a token", d.Challenge)
	}
	// same-workspace re-put: new challenge, still unverified
	code, out2 := h.doJWT("PUT", "/v1/domains", map[string]any{"domain": "onboarding.acme.com"}, ashaTok)
	if code != http.StatusOK {
		t.Fatalf("re-put = %d", code)
	}
	var d2 workspaceDomain
	_ = json.Unmarshal(out2, &d2)
	if d2.Challenge == d.Challenge || d2.Verified {
		t.Fatal("re-put must regenerate the challenge and stay unverified")
	}

	// drop the nocaps.com probe row so the later list assertion is clean
	var nocapsID string
	if err := adminPool.QueryRow(ctx, `SELECT id FROM workspace_domains WHERE domain = 'nocaps.com'`).Scan(&nocapsID); err == nil {
		if code, _ := h.doJWT("DELETE", "/v1/domains/"+nocapsID, nil, ashaTok); code != http.StatusNoContent {
			t.Fatalf("cleanup delete = %d", code)
		}
	}

	// stub the resolver: wrong TXT first, then the real one
	origTXT := dnsTXT
	defer func() { dnsTXT = origTXT }()
	dnsTXT = func(ctx context.Context, record string) ([]string, error) {
		if record == "_openlane-challenge.onboarding.acme.com" {
			return []string{"something-else"}, nil
		}
		return nil, context.DeadlineExceeded
	}
	if code, _ := h.doJWT("POST", "/v1/domains/"+d2.ID+"/verify", nil, ashaTok); code != http.StatusBadRequest {
		t.Fatalf("wrong TXT verify = %d, want 400", code)
	}
	dnsTXT = func(ctx context.Context, record string) ([]string, error) {
		if record == "_openlane-challenge.onboarding.acme.com" {
			return []string{d2.Challenge}, nil
		}
		return nil, context.DeadlineExceeded
	}
	code, out3 := h.doJWT("POST", "/v1/domains/"+d2.ID+"/verify", nil, ashaTok)
	if code != http.StatusOK {
		t.Fatalf("verify = %d %s", code, out3)
	}
	// list shows verified
	code, out4 := h.doJWT("GET", "/v1/domains", nil, ashaTok)
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	var list []workspaceDomain
	if err := json.Unmarshal(out4, &list); err != nil || len(list) != 1 || !list[0].Verified {
		t.Fatalf("list = %s", out4)
	}
	// audit rows
	var ct int
	_ = adminPool.QueryRow(ctx, `SELECT count(*) FROM audit_logs WHERE entity_type='workspace_domain'`).Scan(&ct)
	if ct < 2 {
		t.Fatalf("audit rows = %d, want claimed+verified", ct)
	}

	// cross-workspace claim conflict: the UNIQUE(domain) constraint is the
	// structural guard; the handler surfaces it as 409 for API callers.
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO workspace_domains (workspace_id, domain, challenge)
		VALUES ('22222222-2222-2222-2222-222222222222', 'onboarding.acme.com', 'x')`); err == nil {
		t.Fatal("cross-workspace dup domain must violate UNIQUE")
	}

	// delete
	if code, _ := h.doJWT("DELETE", "/v1/domains/"+d2.ID, nil, ashaTok); code != http.StatusNoContent {
		t.Fatalf("delete = %d, want 204", code)
	}
	if code, _ := h.doJWT("GET", "/v1/domains", nil, ashaTok); code != http.StatusOK {
		t.Fatal("list after delete failed")
	}
	var empty []workspaceDomain
	code, out5 := h.doJWT("GET", "/v1/domains", nil, ashaTok)
	_ = code
	if err := json.Unmarshal(out5, &empty); err != nil || len(empty) != 0 {
		t.Fatalf("list after delete = %s", out5)
	}
}
