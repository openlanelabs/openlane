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

// TestPortalBranding: §499 — theme persistence, hex validation, portal
// session carries the theme, host resolution verified-only.
func TestPortalBranding(t *testing.T) {
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
	if _, err := adminPool.Exec(ctx, `DELETE FROM workspace_themes; DELETE FROM workspace_domains`); err != nil {
		t.Fatal(err)
	}
	// contact + portal link for the session test (contact_id NOT NULL)
	if _, err := adminPool.Exec(ctx, `
		INSERT INTO contacts (id, workspace_id, customer_id, email, display_name, is_portal_user)
		VALUES ('44444444-4444-4444-4444-444444444444', '11111111-1111-1111-1111-111111111111',
		        '33333333-3333-3333-3333-333333333333', 'c@acme.test', 'C', true)
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	var linkToken string
	if err := adminPool.QueryRow(ctx, `
		INSERT INTO portal_links (workspace_id, project_id, contact_id, token_hash, expires_at)
		VALUES ('11111111-1111-1111-1111-111111111111', '55555555-5555-5555-5555-555555555555',
		        '44444444-4444-4444-4444-444444444444',
		        encode(sha256('brand-token-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'::bytea), 'hex'), now() + interval '7 days')
		RETURNING 'brand-token-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'`).Scan(&linkToken); err != nil {
		t.Fatal(err)
	}

	acode, _, ashaTok := devLogin(h)
	if acode != 200 {
		t.Fatalf("asha login = %d", acode)
	}

	// portal session BEFORE theme: no theme key
	code, out := h.do("GET", "/v1/portal/"+linkToken+"/session", nil)
	if code != http.StatusOK {
		t.Fatalf("session = %d %s", code, out)
	}
	var rawSess map[string]json.RawMessage
	if err := json.Unmarshal(out, &rawSess); err != nil {
		t.Fatal(err)
	}
	if _, has := rawSess["theme"]; has {
		t.Fatalf("unthemed session must omit theme, got %s", rawSess["theme"])
	}

	// member 403 on PUT
	if code, _ := h.doJWT("PUT", "/v1/theme", map[string]any{"brand_color": "#2563eb"},
		loginToken(t, h)); code != http.StatusForbidden {
		t.Fatalf("member theme PUT = %d, want 403", code)
	}
	// bad hex
	if code, _ := h.doJWT("PUT", "/v1/theme", map[string]any{"brand_color": "blue"}, ashaTok); code != http.StatusBadRequest {
		t.Fatalf("bad hex = %d, want 400", code)
	}
	// save theme
	if code, _ := h.doJWT("PUT", "/v1/theme", map[string]any{
		"logo_url": "https://acme.test/logo.png", "brand_color": "#2563EB", "no_branding": true,
	}, ashaTok); code != http.StatusOK {
		t.Fatalf("theme PUT = %d", code)
	}
	// GET round-trip (member-readable)
	rcode, _, raviTok := loginAs(h, "ravi@acme.test")
	if rcode != 200 {
		t.Fatal("ravi login failed")
	}
	code, out = h.doJWT("GET", "/v1/theme", nil, raviTok)
	if code != http.StatusOK {
		t.Fatalf("member theme GET = %d", code)
	}
	var th portalTheme
	if err := json.Unmarshal(out, &th); err != nil {
		t.Fatal(err)
	}
	if th.BrandColor == nil || *th.BrandColor != "#2563EB" || !th.NoBranding {
		t.Fatalf("theme = %+v", th)
	}

	// portal session NOW carries the theme
	code, out = h.do("GET", "/v1/portal/"+linkToken+"/session", nil)
	if code != http.StatusOK {
		t.Fatalf("session 2 = %d", code)
	}
	sess := map[string]any{}
	if err := json.Unmarshal(out, &sess); err != nil {
		t.Fatal(err)
	}
	themeMap, _ := sess["theme"].(map[string]any)
	if themeMap == nil || themeMap["brand_color"] != "#2563EB" || themeMap["no_branding"] != true {
		t.Fatalf("session theme = %v", sess["theme"])
	}

	// host resolution: unverified domain → null; verified → ws + theme
	dom := "brand.acme.test"
	if code, _ := h.doJWT("PUT", "/v1/domains", map[string]any{"domain": dom}, ashaTok); code != http.StatusOK {
		t.Fatalf("claim = %d", code)
	}
	code, out = h.do("GET", "/v1/portal/host?host="+dom, nil)
	if code != http.StatusOK {
		t.Fatalf("host = %d", code)
	}
	var hr map[string]any
	_ = json.Unmarshal(out, &hr)
	if hr["workspace_id"] != nil {
		t.Fatalf("unverified host resolved: %v", hr)
	}
	// verify it (direct DB update — DNS is stubbed elsewhere)
	if _, err := adminPool.Exec(ctx,
		`UPDATE workspace_domains SET verified = true WHERE domain = $1`, dom); err != nil {
		t.Fatal(err)
	}
	code, out = h.do("GET", "/v1/portal/host?host="+dom+":3000", nil) // port stripped
	if code != http.StatusOK {
		t.Fatalf("host 2 = %d", code)
	}
	hr = map[string]any{}
	_ = json.Unmarshal(out, &hr)
	if hr["workspace_id"] != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("verified host = %v", hr)
	}
	tm, _ := hr["theme"].(map[string]any)
	if tm == nil || tm["brand_color"] != "#2563EB" {
		t.Fatalf("host theme = %v", hr["theme"])
	}
	// unknown host → null (§229 fallback: caller serves standard portal)
	code, out = h.do("GET", "/v1/portal/host?host=nope.example.org", nil)
	_ = code
	hr = map[string]any{}
	_ = json.Unmarshal(out, &hr)
	if hr["workspace_id"] != nil {
		t.Fatalf("unknown host resolved: %v", hr)
	}
}

func loginToken(t *testing.T, h *httptestSrv) string {
	t.Helper()
	code, _, tok := loginAs(h, "ravi@acme.test")
	if code != 200 {
		t.Fatalf("loginAs = %d", code)
	}
	return tok
}
