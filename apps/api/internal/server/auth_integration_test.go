//go:build integration

package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func authTestStack(t *testing.T) (*httptestSrv, string) {
	t.Helper()
	srv, _, adminPool, ctx := importTestStack(t)
	_ = srv
	// seed a user + membership (owner) in wsA
	must := func(q string, args ...any) {
		if _, err := adminPool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	must(`INSERT INTO users (id, email, display_name) VALUES ($1, 'asha@acme.test', 'Asha Verma')`, userID)
	must(`INSERT INTO memberships (workspace_id, user_id, role) VALUES ($1, $2, 'admin')`, wsA, userID)
	h := &httptestSrv{t: t, URL: srv.URL}
	return h, srv.URL
}

var _ = (*pgxpool.Pool)(nil)

// rawPost/Get without auth headers for the public auth endpoints
func authJSON(t *testing.T, h *httptestSrv, method, path string, body any, hdr map[string]string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func TestMagicLinkAuthFlow(t *testing.T) {
	h, _ := authTestStack(t)

	// 1. request — unknown email still 202 (no oracle)
	code, _ := authJSON(t, h, "POST", "/v1/auth/magic-link", map[string]string{"email": "nobody@nowhere.test", "workspace_slug": "acme"}, nil)
	if code != 202 {
		t.Fatalf("unknown email = %d, want 202", code)
	}

	// 2. request — real member, dev token returned (OPENLANE_DEV_LOGIN=1 in test env? set via TestMain? For tests we read it from the response only if env is set — the test stack sets it)
	os.Setenv("OPENLANE_DEV_LOGIN", "1")
	code, body := authJSON(t, h, "POST", "/v1/auth/magic-link", map[string]string{"email": "asha@acme.test", "workspace_slug": "acme"}, nil)
	if code != 202 {
		t.Fatalf("magic-link = %d: %s", code, body)
	}
	var ml struct {
		DevToken string `json:"dev_token"`
	}
	json.Unmarshal(body, &ml)
	if ml.DevToken == "" {
		t.Fatal("dev_token missing (OPENLANE_DEV_LOGIN=1)")
	}

	// 3. consume — wrong/short token 401
	if code, _ := authGet(t, h, "/v1/auth/magic-link/consume?token=short"); code != 401 {
		t.Fatalf("short token = %d", code)
	}
	// consume — valid → tokens
	code, body = authGet(t, h, "/v1/auth/magic-link/consume?token="+ml.DevToken)
	if code != 200 {
		t.Fatalf("consume = %d: %s", code, body)
	}
	var sess struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		UserID       string `json:"user_id"`
		WorkspaceID  string `json:"workspace_id"`
		Role         string `json:"role"`
	}
	json.Unmarshal(body, &sess)
	if sess.AccessToken == "" || sess.RefreshToken == "" || sess.UserID != userID || sess.WorkspaceID != wsA || sess.Role != "admin" {
		t.Fatalf("session = %+v", sess)
	}
	// 4. single-use: consume again → 401
	if code, _ := authGet(t, h, "/v1/auth/magic-link/consume?token="+ml.DevToken); code != 401 {
		t.Fatalf("reuse = %d, want 401", code)
	}

	// 5. /auth/me with JWT
	authHdr := map[string]string{"Authorization": "Bearer " + sess.AccessToken}
	code, body = authJSON(t, h, "GET", "/v1/auth/me", nil, authHdr)
	if code != 200 {
		t.Fatalf("me = %d: %s", code, body)
	}
	var me struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	json.Unmarshal(body, &me)
	if me.Email != "asha@acme.test" || me.Role != "admin" {
		t.Fatalf("me = %+v", me)
	}
	// workspaces list
	code, body = authJSON(t, h, "GET", "/v1/auth/workspaces", nil, authHdr)
	if code != 200 || !strings.Contains(string(body), "acme") {
		t.Fatalf("workspaces = %d %s", code, body)
	}

	// 6. staff route with JWT (no X-Workspace-Id header — workspace comes from the token)
	code, body = authJSON(t, h, "GET", "/v1/projects", nil, authHdr)
	if code != 200 {
		t.Fatalf("projects via JWT = %d: %s", code, body)
	}

	// 7. JWT without membership workspace mismatch → e.g. tampered ws in token — covered by parse; here: another user's JWT can't see wsA (different user). Skip.

	// 8. refresh rotation
	code, body = authJSON(t, h, "POST", "/v1/auth/refresh", map[string]string{"refresh_token": sess.RefreshToken}, nil)
	if code != 200 {
		t.Fatalf("refresh = %d: %s", code, body)
	}
	var rot struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	json.Unmarshal(body, &rot)
	if rot.RefreshToken == "" || rot.RefreshToken == sess.RefreshToken {
		t.Fatal("rotation must issue a new refresh token")
	}
	// 9. REUSE of the OLD refresh → family revoked; the NEW one dies too
	if code, _ := authJSON(t, h, "POST", "/v1/auth/refresh", map[string]string{"refresh_token": sess.RefreshToken}, nil); code != 401 {
		t.Fatalf("old refresh reuse = %d, want 401", code)
	}
	if code, _ := authJSON(t, h, "POST", "/v1/auth/refresh", map[string]string{"refresh_token": rot.RefreshToken}, nil); code != 401 {
		t.Fatalf("family revocation = %d, want 401 (theft kills the whole family)", code)
	}
	_ = rot.AccessToken
}

func authGet(t *testing.T, h *httptestSrv, path string) (int, []byte) {
	return authJSON(t, h, "GET", path, nil, nil)
}
