//go:build integration

package server

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"testing"
)

// SCIM 2.0 provisioning (§406 Phase2): the IdP lifecycle. The
// deprovisioning half is the point — active:false revokes access
// NOW (membership deactivated_at; the four auth guards reject
// deactivated members).

func scimDo(h *httptestSrv, method, path, token string, body any) (int, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/scim+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestSCIMLifecycle(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	t.Setenv("OPENLANE_INTEGRATION_KEY", base64.StdEncoding.EncodeToString(key))

	srv, _, h, _ := timeTestStack(t)
	defer srv.Close()
	_, _, access := devLogin(h)

	// --- config: admin sets token ---
	code, body := h.doJWT("PUT", "/v1/scim", map[string]any{}, access)
	if code != http.StatusOK {
		t.Fatalf("scim put = %d %s", code, body)
	}
	var cfg struct {
		Token string `json:"token"`
	}
	json.Unmarshal(body, &cfg)
	if len(cfg.Token) < 69 { // scim_ + 64 hex
		t.Fatalf("token = %q", cfg.Token)
	}

	// --- auth enforced ---
	if code, _ := scimDo(h, "GET", "/scim/v2/ServiceProviderConfig", "", nil); code != http.StatusOK {
		t.Fatal("SPConfig must be public")
	}
	if code, _ := scimDo(h, "POST", "/scim/v2/Users", "wrong_token", nil); code != http.StatusUnauthorized {
		t.Fatal("bad token must 401")
	}
	if code, _ := scimDo(h, "POST", "/scim/v2/Users", "", nil); code != http.StatusUnauthorized {
		t.Fatal("missing token must 401")
	}

	// --- create ---
	createBody := map[string]any{
		"schemas":  []string{"urn:ietf:params:scim:schemas:core:2.0:User"},
		"userName": "newhire@acme.test",
		"active":   true,
		"name":     map[string]string{"givenName": "New", "familyName": "Hire"},
	}
	code, body = scimDo(h, "POST", "/scim/v2/Users", cfg.Token, createBody)
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var u struct {
		ID       string `json:"id"`
		UserName string `json:"userName"`
		Active   *bool  `json:"active"`
	}
	json.Unmarshal(body, &u)
	if u.ID == "" || u.UserName != "newhire@acme.test" || u.Active == nil || !*u.Active {
		t.Fatalf("created user = %s", body)
	}

	// --- duplicate → 409 with same SCIM id (IdP reconciliation) ---
	code, body = scimDo(h, "POST", "/scim/v2/Users", cfg.Token, createBody)
	if code != http.StatusConflict {
		t.Fatalf("dup create = %d (want 409)", code)
	}
	var u2 struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &u2)
	if u2.ID != u.ID {
		t.Fatalf("dup id drift: %s vs %s", u2.ID, u.ID)
	}

	// --- filter lookup ---
	code, body = scimDo(h, "GET", `/scim/v2/Users?filter=`+url.QueryEscape(`userName eq "newhire@acme.test"`), cfg.Token, nil)
	if code != http.StatusOK {
		t.Fatalf("filter = %d %s", code, body)
	}
	var u3 struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &u3)
	if u3.ID != u.ID {
		t.Fatalf("filter id drift: %s vs %s", u3.ID, u.ID)
	}
	// unsupported filter shape → 400
	if code, _ = scimDo(h, "GET", `/scim/v2/Users?filter=`+url.QueryEscape(`emails.value sw "x"`), cfg.Token, nil); code != http.StatusBadRequest {
		t.Fatal("unsupported filter must 400")
	}

	// --- deactivate (the breach preventer) ---
	patch := map[string]any{
		"schemas":    []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
		"Operations": []map[string]any{{"op": "replace", "path": "active", "value": false}},
	}
	code, body = scimDo(h, "PATCH", "/scim/v2/Users/"+u.ID, cfg.Token, patch)
	if code != http.StatusOK {
		t.Fatalf("patch off = %d %s", code, body)
	}
	var uOff struct {
		Active *bool `json:"active"`
	}
	json.Unmarshal(body, &uOff)
	if uOff.Active == nil || *uOff.Active {
		t.Fatalf("patched active = %s", body)
	}

	// deactivated member cannot magic-link in (the auth guard).
	// Request always 202s (no user enumeration) — the dev_token
	// must be ABSENT for a deactivated member.
	code, body = authJSON(t, h, "POST", "/v1/auth/magic-link",
		map[string]string{"email": "newhire@acme.test", "workspace_slug": "acme"}, nil)
	if code != http.StatusAccepted {
		t.Fatalf("magic-link = %d (must 202 regardless)", code)
	}
	var ml struct {
		DevToken string `json:"dev_token"`
	}
	json.Unmarshal(body, &ml)
	if ml.DevToken != "" {
		t.Fatal("deactivated member received a magic link — access revoked must mean revoked")
	}

	// --- reactivate restores prior role ---
	patch["Operations"].([]map[string]any)[0]["value"] = true
	code, body = scimDo(h, "PATCH", "/scim/v2/Users/"+u.ID, cfg.Token, patch)
	if code != http.StatusOK {
		t.Fatalf("patch on = %d %s", code, body)
	}
	var uOn struct {
		Active *bool `json:"active"`
	}
	json.Unmarshal(body, &uOn)
	if uOn.Active == nil || !*uOn.Active {
		t.Fatalf("reactivated = %s", body)
	}

	// --- delete → soft (no hard delete) ---
	if code, _ = scimDo(h, "DELETE", "/scim/v2/Users/"+u.ID, cfg.Token, nil); code != http.StatusNoContent {
		t.Fatal("delete must 204")
	}
	// still resolvable, now inactive
	code, body = scimDo(h, "GET", `/scim/v2/Users?filter=`+url.QueryEscape(`userName eq "newhire@acme.test"`), cfg.Token, nil)
	if code != http.StatusOK {
		t.Fatalf("post-delete filter = %d", code)
	}
	var u4 struct {
		Active *bool `json:"active"`
	}
	json.Unmarshal(body, &u4)
	if u4.Active != nil && *u4.Active {
		t.Fatal("deleted user must be inactive")
	}

	// --- unknown user 404s per SCIM error schema ---
	if code, body = scimDo(h, "PATCH", "/scim/v2/Users/99999999-9999-9999-9999-999999999999", cfg.Token, patch); code != http.StatusNotFound {
		t.Fatalf("unknown patch = %d %s", code, body)
	}
	if !bytes.Contains(body, []byte("urn:ietf:params:scim:api:messages:2.0:Error")) {
		t.Fatalf("error schema = %s", body)
	}

	// --- SPConfig declares honestly ---
	code, body = scimDo(h, "GET", "/scim/v2/ServiceProviderConfig", "", nil)
	if code != http.StatusOK || !bytes.Contains(body, []byte("oauthbearertoken")) {
		t.Fatalf("SPConfig = %d %s", code, body)
	}
}
