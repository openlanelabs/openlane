//go:build integration

package server

import (
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// loginAs: dev magic-link login for an arbitrary seeded email.
func loginAs(h *httptestSrv, email string) (int, []byte, string) {
	os.Setenv("OPENLANE_DEV_LOGIN", "1")
	code, body := authJSON(h.t, h, "POST", "/v1/auth/magic-link",
		map[string]string{"email": email, "workspace_slug": "acme"}, nil)
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

// timeApprovalStack: two members — asha (manager-role admin) + ravi
// (member) — with one logged entry each.
func timeApprovalStack(t *testing.T) (*httptestSrv, string, string, string, string) {
	t.Helper()
	srv, _, h, taskID := timeTestStack(t)
	t.Cleanup(func() { srv.Close() })
	_, _, asha := devLogin(h)

	// second member logs their own entry
	admin := adminDSN(t)
	ap, err := pgxpool.New(t.Context(), admin)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	if _, err := ap.Exec(t.Context(), `INSERT INTO users (id, email, display_name)
		VALUES ('bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'ravi@acme.test', 'Ravi') ON CONFLICT DO NOTHING`); err != nil {
		t.Fatal(err)
	}
	if _, err := ap.Exec(t.Context(), `INSERT INTO memberships (workspace_id, user_id, role)
		VALUES ($1, 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', 'member') ON CONFLICT DO NOTHING`, wsA); err != nil {
		t.Fatal(err)
	}
	// ravi's entry (author = ravi)
	st, en := fixedSpan(120, 4)
	var raviEntry string
	if err := ap.QueryRow(t.Context(), `INSERT INTO time_entries (workspace_id, project_id, task_id, user_id, started_at, ended_at, minutes)
		VALUES ($1, $2, $3, 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb', $4, $5, 120) RETURNING id::text`,
		wsA, projA, taskID, st, en).Scan(&raviEntry); err != nil {
		t.Fatal(err)
	}

	// ravi's JWT: magic-link flow with ravi's email
	rcode, rbody, ravi := loginAs(h, "ravi@acme.test")
	if rcode != 200 || ravi == "" {
		t.Fatalf("ravi login = %d %s", rcode, rbody)
	}

	// asha's own entry (author = asha aaaa)
	st2, en2 := fixedSpan(60, 5)
	if code, body := h.doJWT("POST", "/v1/tasks/"+taskID+"/time", map[string]any{
		"started_at": st2, "ended_at": en2,
	}, asha); code != http.StatusCreated {
		t.Fatalf("asha log = %d %s", code, body)
	}
	// asha's entry id: list my time
	code, body := h.doJWT("GET", "/v1/me/time", nil, asha)
	if code != http.StatusOK {
		t.Fatalf("me/time = %d %s", code, body)
	}
	var mine []struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &mine)
	if len(mine) == 0 {
		t.Fatal("asha has no entries")
	}
	return h, asha, ravi, mine[0].ID, raviEntry
}

func TestTimeApprovalFlow(t *testing.T) {
	h, asha, ravi, _, raviEntry := timeApprovalStack(t)

	// manager queue is empty (nothing submitted)
	code, body := h.doJWT("GET", "/v1/time/pending", nil, asha)
	if code != http.StatusOK {
		t.Fatalf("pending = %d %s", code, body)
	}
	var pending []map[string]any
	json.Unmarshal(body, &pending)
	if len(pending) != 0 {
		t.Fatalf("pending should be empty: %s", body)
	}

	// ravi submits
	if code, body = h.doJWT("POST", "/v1/time/"+raviEntry+"/submit", nil, ravi); code != http.StatusNoContent {
		t.Fatalf("submit = %d %s", code, body)
	}
	// double submit → 400
	if code, _ = h.doJWT("POST", "/v1/time/"+raviEntry+"/submit", nil, ravi); code != http.StatusBadRequest {
		t.Fatalf("double submit = %d", code)
	}

	// appears in manager queue
	code, body = h.doJWT("GET", "/v1/time/pending", nil, asha)
	json.Unmarshal(body, &pending)
	if len(pending) != 1 || pending[0]["id"] != raviEntry {
		t.Fatalf("queue = %s", body)
	}

	// ravi (member) cannot approve
	if code, _ = h.doJWT("POST", "/v1/time/"+raviEntry+"/approve", nil, ravi); code != http.StatusForbidden {
		t.Fatalf("member approve = %d (want 403)", code)
	}
	// member cannot see the queue
	if code, _ = h.doJWT("GET", "/v1/time/pending", nil, ravi); code != http.StatusForbidden {
		t.Fatalf("member queue = %d (want 403)", code)
	}
	// ravi cannot submit asha's entries (author check via entry path)
	if code, _ = h.doJWT("POST", "/v1/time/"+raviEntry+"/submit", nil, asha); code != http.StatusForbidden {
		t.Fatalf("asha submits ravi's = %d (want 403)", code)
	}

	// manager rejects without reason → 400
	if code, _ = h.doJWT("POST", "/v1/time/"+raviEntry+"/reject", map[string]any{}, asha); code != http.StatusBadRequest {
		t.Fatalf("reject no reason = %d (want 400)", code)
	}
	// reject with reason
	if code, _ = h.doJWT("POST", "/v1/time/"+raviEntry+"/reject", map[string]any{"reason": "missing ticket ref"}, asha); code != http.StatusNoContent {
		t.Fatalf("reject = %d", code)
	}

	// resubmit allowed from rejected
	if code, _ = h.doJWT("POST", "/v1/time/"+raviEntry+"/submit", nil, ravi); code != http.StatusNoContent {
		t.Fatalf("resubmit = %d", code)
	}

	// approve
	if code, _ = h.doJWT("POST", "/v1/time/"+raviEntry+"/approve", nil, asha); code != http.StatusNoContent {
		t.Fatalf("approve = %d", code)
	}
	// approved is terminal for the flow
	if code, _ = h.doJWT("POST", "/v1/time/"+raviEntry+"/submit", nil, ravi); code != http.StatusBadRequest {
		t.Fatalf("submit approved = %d (want 400)", code)
	}
	if code, _ = h.doJWT("POST", "/v1/time/"+raviEntry+"/reject", map[string]any{"reason": "x"}, asha); code != http.StatusBadRequest {
		t.Fatalf("reject approved = %d (want 400)", code)
	}

	// queue drained
	code, body = h.doJWT("GET", "/v1/time/pending", nil, asha)
	json.Unmarshal(body, &pending)
	if len(pending) != 0 {
		t.Fatalf("queue after approve = %s", body)
	}

	// cross-ws: unknown id → 404
	if code, _ = h.doJWT("POST", "/v1/time/99999999-9999-9999-9999-999999999999/approve", nil, asha); code != http.StatusNotFound {
		t.Fatalf("foreign entry = %d (want 404)", code)
	}
}

func TestTimeSelfApprovalBlocked(t *testing.T) {
	h, asha, _, ashaEntry, _ := timeApprovalStack(t)

	// asha submits her own entry
	if code, _ := h.doJWT("POST", "/v1/time/"+ashaEntry+"/submit", nil, asha); code != http.StatusNoContent {
		t.Fatalf("self submit = %d", code)
	}
	// asha (admin) tries to approve her own → 403
	if code, _ := h.doJWT("POST", "/v1/time/"+ashaEntry+"/approve", nil, asha); code != http.StatusForbidden {
		t.Fatalf("self approve = %d (want 403)", code)
	}
}
