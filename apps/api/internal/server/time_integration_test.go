//go:build integration

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func timeTestStack(t *testing.T) (*httptest.Server, *pgxpool.Pool, *httptestSrv, string) {
	t.Helper()
	srv, pool, h := filesTestStack(t)
	// a real member (JWT path): user + membership, magic-link dev login
	admin := adminDSN(t)
	ap, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	if _, err := ap.Exec(context.Background(),
		`INSERT INTO users (id, email, display_name) VALUES ($1, 'asha@acme.test', 'Asha') ON CONFLICT (id) DO NOTHING`, userID); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Exec(context.Background(),
		`INSERT INTO memberships (workspace_id, user_id, role) VALUES ($1, $2, 'admin') ON CONFLICT DO NOTHING`, wsA, userID); err != nil {
		t.Fatal(err)
	}
	code, body := h.do("POST", "/v1/projects/"+projA+"/tasks", map[string]any{
		"title": "Time task", "owner_type": "internal", "customer_visible": false,
	})
	if code != 201 {
		t.Fatalf("task create = %d %s", code, body)
	}
	var task struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &task)
	return srv, pool, h, task.ID
}

func rfc(off time.Duration) string { return time.Now().Add(off).UTC().Format(time.RFC3339) }

// devLogin: magic-link + consume, returns the access token for a seeded member.
func devLogin(h *httptestSrv) (int, []byte, string) {
	os.Setenv("OPENLANE_DEV_LOGIN", "1")
	code, body := authJSON(h.t, h, "POST", "/v1/auth/magic-link",
		map[string]string{"email": "asha@acme.test", "workspace_slug": "acme"}, nil)
	if code != 202 {
		return code, body, ""
	}
	var ml struct {
		DevToken string `json:"dev_token"`
	}
	json.Unmarshal(body, &ml)
	if ml.DevToken == "" {
		return 0, body, ""
	}
	code, body = authGet(h.t, h, "/v1/auth/magic-link/consume?token="+ml.DevToken)
	if code != 200 {
		return code, body, ""
	}
	var tok struct {
		Access string `json:"access_token"`
	}
	json.Unmarshal(body, &tok)
	return 200, body, tok.Access
}

// doJWT: staff request with a bearer access token.
func (h *httptestSrv) doJWT(method, path string, body any, access string) (int, []byte) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.URL+path, rd)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func TestTimePolicies(t *testing.T) {
	_, _, h, taskID := timeTestStack(t)
	_, _, access := devLogin(h)
	if access == "" {
		t.Fatal("no access token")
	}
	logT := func(body map[string]any) (int, []byte) {
		return h.doJWT("POST", "/v1/tasks/"+taskID+"/time", body, access)
	}

	// valid 1h entry (JWT user)
	code, body := logT(map[string]any{
		"started_at": rfc(-2 * time.Hour), "ended_at": rfc(-1 * time.Hour), "note": "prep",
	})
	if code != 201 {
		t.Fatalf("valid log = %d %s", code, body)
	}
	var e timeEntryOut
	json.Unmarshal(body, &e)
	if e.Minutes != 60 || e.Status != "draft" || e.TaskID != taskID {
		t.Fatalf("entry shape: %s", body)
	}

	// anonymous (static) identity cannot log time
	if code, _ := h.do("POST", "/v1/tasks/"+taskID+"/time", map[string]any{
		"started_at": rfc(-1 * time.Hour), "ended_at": rfc(-30 * time.Minute),
	}); code != 400 {
		t.Fatalf("static identity log = %d, want 400", code)
	}

	// >12h rejected
	if code, _ := logT(map[string]any{
		"started_at": rfc(-13 * time.Hour), "ended_at": rfc(-30 * time.Minute),
	}); code != 400 {
		t.Fatalf("13h = %d, want 400", code)
	}
	// future rejected
	if code, _ := logT(map[string]any{
		"started_at": rfc(-30 * time.Minute), "ended_at": rfc(2 * time.Hour),
	}); code != 400 {
		t.Fatalf("future = %d, want 400", code)
	}
	// >7d back rejected
	if code, _ := logT(map[string]any{
		"started_at": rfc(-8 * 24 * time.Hour), "ended_at": rfc(-8*24*time.Hour + time.Hour),
	}); code != 400 {
		t.Fatalf("backdated = %d, want 400", code)
	}
	// zero interval rejected
	if code, _ := logT(map[string]any{
		"started_at": rfc(-1 * time.Hour), "ended_at": rfc(-1 * time.Hour),
	}); code != 400 {
		t.Fatalf("zero = %d, want 400", code)
	}
	// garbage timestamp rejected
	if code, _ := logT(map[string]any{
		"started_at": "yesterday", "ended_at": rfc(-1 * time.Hour),
	}); code != 400 {
		t.Fatalf("garbage ts = %d, want 400", code)
	}

	// audit row via admin pool (app role can't see it without ctx — RLS)
	admin := adminDSN(t)
	ap, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	var audit int
	if err := ap.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action='time.logged'`).Scan(&audit); err != nil || audit != 1 {
		t.Fatalf("audit time.logged = %d %v", audit, err)
	}
}

func TestTimeScopes(t *testing.T) {
	_, _, h, _ := timeTestStack(t)
	_, _, access := devLogin(h)

	// project-level (taskless) entry as the JWT user
	code, body := h.doJWT("POST", "/v1/projects/"+projA+"/time", map[string]any{
		"started_at": rfc(-3 * time.Hour), "ended_at": rfc(-2 * time.Hour), "note": "planning",
	}, access)
	if code != 201 {
		t.Fatalf("project log = %d %s", code, body)
	}
	var e timeEntryOut
	json.Unmarshal(body, &e)
	if e.TaskID != "" || e.ProjectID != projA {
		t.Fatalf("taskless shape: %s", body)
	}

	// nonexistent (other-tenant) task 404 — no oracle
	ghost := "99999999-9999-9999-9999-999999999999"
	if code, _ := h.doJWT("POST", "/v1/tasks/"+ghost+"/time", map[string]any{
		"started_at": rfc(-1 * time.Hour), "ended_at": rfc(-30 * time.Minute),
	}, access); code != 404 {
		t.Fatalf("ghost task = %d, want 404", code)
	}

	// project list shows the entry (static staff can read)
	code, body = h.do("GET", "/v1/projects/"+projA+"/time", nil)
	if code != 200 || strings.Count(string(body), `"minutes"`) != 1 {
		t.Fatalf("project list = %d %s", code, body)
	}

	// me/time as the JWT user: exactly their entries (the project log)
	code, body = h.doJWT("GET", "/v1/me/time", nil, access)
	if code != 200 || strings.Count(string(body), `"minutes"`) != 1 {
		t.Fatalf("me/time = %d %s", code, body)
	}
}
