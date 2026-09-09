//go:build integration

// Portal slice integration test: real HTTP against real Postgres.
// Proves the contract end-to-end: create link → session → tasks → complete
// → audit row exists → revocation → 410. IDOR assertions included.
//
// Env (docker compose / CI service):
//
//	TEST_DATABASE_URL=postgres://openlane:openlane@localhost:5432/openlane_test
//	START_POSTGRES=1   (compose up -d; suite manages schema itself)
//
// Run: go test -tags integration -run TestPortal ./internal/server/...
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	wsA       = "11111111-1111-1111-1111-111111111111"
	wsB       = "22222222-2222-2222-2222-222222222222"
	userID    = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	custID    = "33333333-3333-3333-3333-333333333333"
	contactID = "44444444-4444-4444-4444-444444444444"
	projA     = "55555555-5555-5555-5555-555555555555"
	projB     = "66666666-6666-6666-6666-666666666666"
	staffTok  = "test-staff-token"
)

func adminDSN(t *testing.T) string {
	t.Helper()
	// admin DSN = same host, role openlane (owner), for migrations+seed
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	return strings.Replace(strings.Replace(dsn, "openlane_app:openlane_app@", "openlane:openlane@", 1), "/openlane_app", "/openlane", 1)
}

func appDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	return dsn
}

func TestPortal(t *testing.T) {
	admin, app := adminDSN(t), appDSN(t)

	// ---- schema: reset + migrate + seed (owner DSN) ----
	ctx := context.Background()
	run := func(args ...string) string {
		cmd := exec.Command(args[0], args[1:]...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
		return ""
	}
	run("go", "run", "github.com/pressly/goose/v3/cmd/goose@v3.22.0", "-dir", "../../../../db/migrations", "postgres", admin, "reset")
	run("go", "run", "github.com/pressly/goose/v3/cmd/goose@v3.22.0", "-dir", "../../../../db/migrations", "postgres", admin, "up")
	adminPool, err := pgxpool.New(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer adminPool.Close()
	pgMust := func(q string, args ...any) {
		if _, err := adminPool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("seed %q: %v", q[:min(40, len(q))], err)
		}
	}
	pgMust(`ALTER ROLE openlane_app WITH PASSWORD 'openlane_app'`)
	pgMust(`INSERT INTO workspaces (id, name, slug) VALUES ($1,'Acme SI','acme'), ($2,'Beta Ltd','beta')`, wsA, wsB)
	pgMust(`INSERT INTO users (id, email, display_name) VALUES ($1,'asha@acme.test','Asha')`, userID)
	pgMust(`INSERT INTO customers (id, workspace_id, name) VALUES ($1, $2, 'Adobe')`, custID, wsA)
	pgMust(`INSERT INTO contacts (id, workspace_id, customer_id, email, display_name, is_portal_user)
		VALUES ($1, $2, $3, 'ravi@adobe.test', 'Ravi', true)`, contactID, wsA, custID)
	pgMust(`INSERT INTO projects (id, workspace_id, customer_id, name, status) VALUES
		($1, $2, $3, 'Adobe Onboarding', 'active'), ($4, $2, $3, 'Adobe Phase 2', 'active')`, projA, wsA, custID, projB)
	pgMust(`INSERT INTO tasks (workspace_id, project_id, title, owner_type, customer_visible, due_at) VALUES
		($1, $2, 'Upload employee CSV', 'customer', true, now() - interval '1 day'),
		($1, $2, 'Approve kickoff scope', 'customer', true, now() + interval '3 day'),
		($1, $2, 'Internal migration plan', 'internal', false, null),
		($1, $3, 'Phase 2 SSO setup', 'customer', true, null)`, wsA, projA, projB)

	mux, pool, err := New(ctx, app, staffTok)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// staff client
	staff := &reqc{t: t, base: srv.URL, auth: "Bearer " + staffTok, ws: wsA}
	_ = staff
	// portal base
	portalURL := func(token string) string { return srv.URL + "/v1/portal/" + token }

	// ---- 1. staff creates link for project A ----
	var link struct {
		ID        string `json:"id"`
		Token     string `json:"token"`
		ProjectID string `json:"project_id"`
		Status    string `json:"status"`
	}
	staffPost(t, srv.URL, "/v1/portal-links", staffTok, wsA, map[string]any{
		"project_id": projA, "contact_id": contactID,
	}, &link)
	if link.Token == "" || len(link.Token) < 43 {
		t.Fatalf("token not returned once: %+v", link)
	}

	// ---- 1b. staff link for project B (for IDOR) ----
	var linkB struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	staffPost(t, srv.URL, "/v1/portal-links", staffTok, wsA, map[string]any{
		"project_id": projB, "contact_id": contactID,
	}, &linkB)

	// ---- 2. portal session ----
	var sess portalSessionOut
	portalGet(t, portalURL(link.Token)+"/session", &sess)
	if sess.ProjectID != projA || sess.ProjectName != "Adobe Onboarding" || sess.ContactName != "Ravi" {
		t.Fatalf("session: %+v", sess)
	}

	// ---- 3. tasks: 2 visible (not internal, not project B) ----
	var tasks []portalTaskOut
	portalGet(t, portalURL(link.Token)+"/tasks", &tasks)
	if len(tasks) != 2 {
		t.Fatalf("expected 2 customer-visible tasks for projA, got %d: %+v", len(tasks), tasks)
	}
	var taskID string
	for _, tk := range tasks {
		if !tk.CustomerVisible || tk.ProjectID != projA {
			t.Fatalf("leak: %+v", tk)
		}
		taskID = tk.ID
	}

	// ---- 3b. IDOR: internal task invisible, other project invisible ----
	// (covered by count=2; explicit cross-project probe:)
	var tasksB []portalTaskOut
	portalGet(t, portalURL(linkB.Token)+"/tasks", &tasksB)
	if len(tasksB) != 1 {
		t.Fatalf("projB link should see 1 task, got %d", len(tasksB))
	}

	// ---- 4. complete ----
	var done portalTaskOut
	portalPost(t, portalURL(link.Token)+"/tasks/"+taskID+"/complete", &done)
	if done.Status != "done" || done.CompletedAt == nil {
		t.Fatalf("complete: %+v", done)
	}

	// ---- 4b. complete again = idempotent 200 ----
	var done2 portalTaskOut
	portalPost(t, portalURL(link.Token)+"/tasks/"+taskID+"/complete", &done2)
	if done2.Status != "done" {
		t.Fatalf("idempotent complete: %+v", done2)
	}

	// ---- 5. audit row exists (owner pool, no RLS games) ----
	var auditCount int
	if err := adminPool.QueryRow(ctx,
		`SELECT count(*) FROM audit_logs WHERE action='task.completed' AND source='portal'`).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != 1 {
		t.Fatalf("audit rows = %d, want exactly 1 (idempotent re-complete skips audit)", auditCount)
	}

	// ---- 6. revoke then 410 ----
	staffReq(t, srv.URL, "POST", "/v1/portal-links/"+link.ID+"/revoke", staffTok, wsA, nil, nil)
	code := portalStatus(t, "GET", portalURL(link.Token)+"/tasks", nil)
	if code != 410 {
		t.Fatalf("revoked link tasks = %d, want 410", code)
	}

	// ---- 7. session on revoked = 410 too ----
	code = portalStatus(t, "GET", portalURL(link.Token)+"/session", nil)
	if code != 410 {
		t.Fatalf("revoked session = %d, want 410", code)
	}

	// ---- 8. never-valid token = 401 ----
	code = portalStatus(t, "GET", srv.URL+"/v1/portal/"+strings.Repeat("x", 43)+"/tasks", nil)
	if code != 401 {
		t.Fatalf("bogus token = %d, want 401", code)
	}

	t.Log("portal slice: create → session → tasks (2) → complete (idempotent) → audit → revoke → 410 → 401 — all OK")
}

// ---- tiny helpers ----

type reqc struct {
	t    *testing.T
	base string
	auth string
	ws   string
}

func staffPost(t *testing.T, base, path, tok, ws string, body any, out any) {
	staffReq(t, base, "POST", path, tok, ws, body, out)
}

func staffReq(t *testing.T, base, method, path, tok, ws string, body, out any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, _ := http.NewRequest(method, base+path, &buf)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("X-Workspace-Id", ws)
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		t.Fatalf("%s %s: %d", method, path, res.StatusCode)
	}
	if out != nil {
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatalf("%s %s decode: %v", method, path, err)
		}
	}
}

func portalGet(t *testing.T, url string, out any) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("GET %s: %d", url, res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		t.Fatalf("GET %s decode: %v", url, err)
	}
}

func portalPost(t *testing.T, url string, out any) {
	t.Helper()
	res, err := http.Post(url, "application/json", bytes.NewReader([]byte("{}")))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("POST %s: %d", url, res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		t.Fatalf("POST %s decode: %v", url, err)
	}
}

func portalStatus(t *testing.T, method, url string, _ *bytes.Buffer) int {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	return res.StatusCode
}

var _ = fmt.Sprintf
var _ = time.Now
