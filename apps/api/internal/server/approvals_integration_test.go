//go:build integration

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

const approvalPortalToken = "approvals-portal-token-1234567890-abcdefghij"

func approvalsTestStack(t *testing.T) *httptestSrv {
	t.Helper()
	_, _, h := filesTestStack(t)
	admin := adminDSN(t)
	ap, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ap.Close() })
	if _, err := ap.Exec(context.Background(),
		`INSERT INTO portal_links (workspace_id, project_id, contact_id, token_hash, expires_at)
		 VALUES ($1, $2, $3, encode(sha256($4::bytea),'hex'), now() + interval '7 days')`,
		wsA, projA, contactID, approvalPortalToken); err != nil {
		t.Fatalf("link: %v", err)
	}
	return h
}

func (h *httptestSrv) doPortalJSON(method, path string, body any) (int, []byte) {
	h.t.Helper()
	real := strings.Replace(path, "/p/", "/v1/portal/", 1)
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.URL+real, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func TestApprovalsStaffFlow(t *testing.T) {
	h := approvalsTestStack(t)

	mk := func(title string) string {
		code, body := h.do("POST", "/v1/projects/"+projA+"/approvals", map[string]any{
			"title": title, "description": "please review",
		})
		if code != http.StatusCreated {
			t.Fatalf("create %q = %d %s", title, code, body)
		}
		var a approvalOut
		json.Unmarshal(body, &a)
		if a.Status != "pending" || a.Title != title {
			t.Fatalf("shape: %s", body)
		}
		return a.ID
	}
	id1 := mk("Kickoff scope sign-off")
	mk("Data migration plan sign-off")

	// staff list shows both
	code, body := h.do("GET", "/v1/projects/"+projA+"/approvals", nil)
	if code != 200 || strings.Count(string(body), `"title"`) != 2 {
		t.Fatalf("staff list = %d %s", code, body)
	}

	// ghost project 404 (RLS), empty title 400
	if code, _ := h.do("POST", "/v1/projects/99999999-9999-9999-9999-999999999999/approvals", map[string]any{"title": "x"}); code != 404 {
		t.Fatal("ghost project should 404")
	}
	if code, _ := h.do("POST", "/v1/projects/"+projA+"/approvals", map[string]any{"title": ""}); code != 400 {
		t.Fatal("empty title should 400")
	}

	// reopen on a pending approval → 404 (only decided can reopen)
	if code, _ := h.do("POST", "/v1/approvals/"+id1+"/reopen", nil); code != 404 {
		t.Fatalf("reopen pending = %d, want 404", code)
	}

	// audit approval.created x2 via admin pool
	admin := adminDSN(t)
	ap, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	defer ap.Close()
	var audit int
	if err := ap.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action='approval.created'`).Scan(&audit); err != nil || audit != 2 {
		t.Fatalf("audit created = %d %v", audit, err)
	}
}

func TestApprovalsPortalDecide(t *testing.T) {
	h := approvalsTestStack(t)
	admin := adminDSN(t)
	ap, _ := pgxpool.New(context.Background(), admin)
	defer ap.Close()

	// staff creates one approval
	code, body := h.do("POST", "/v1/projects/"+projA+"/approvals", map[string]any{
		"title": "Kickoff scope sign-off",
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var a approvalOut
	json.Unmarshal(body, &a)

	pURL := "/p/" + approvalPortalToken

	// portal sees the pending approval
	code, body = h.doPortal("GET", pURL+"/approvals")
	if code != 200 || !strings.Contains(string(body), "Kickoff scope sign-off") {
		t.Fatalf("portal list = %d %s", code, body)
	}

	// decide: approved + comment
	if code, body = h.doPortalJSON("POST", pURL+"/approvals/"+a.ID+"/decide",
		map[string]string{"decision": "approved", "comment": "looks good"}); code != 200 ||
		!strings.Contains(string(body), `"approved"`) {
		t.Fatalf("decide = %d %s", code, body)
	}

	// decided approval vanishes from the portal list
	code, body = h.doPortal("GET", pURL+"/approvals")
	if code != 200 || strings.Contains(string(body), "Kickoff scope sign-off") {
		t.Fatalf("decided still listed: %d %s", code, body)
	}

	// decide AGAIN → 404 (no oracle: pending-only predicate)
	if code, _ := h.doPortalJSON("POST", pURL+"/approvals/"+a.ID+"/decide",
		map[string]string{"decision": "approved"}); code != 404 {
		t.Fatal("double decide = 404 expected")
	}

	// bad decision value → 400
	code, body = h.do("POST", "/v1/projects/"+projA+"/approvals", map[string]any{"title": "second"})
	var b approvalOut
	json.Unmarshal(body, &b)
	if code, _ := h.doPortalJSON("POST", pURL+"/approvals/"+b.ID+"/decide",
		map[string]string{"decision": "maybe"}); code != 400 {
		t.Fatal("bad decision should 400")
	}

	// changes_requested with comment, then staff reopen → pending again
	if code, _ := h.doPortalJSON("POST", pURL+"/approvals/"+b.ID+"/decide",
		map[string]string{"decision": "changes_requested", "comment": "add more detail"}); code != 200 {
		t.Fatal("changes_requested failed")
	}
	if code, _ := h.do("POST", "/v1/approvals/"+b.ID+"/reopen", nil); code != 204 {
		t.Fatal("reopen failed")
	}
	// visible to portal again
	code, body = h.doPortal("GET", pURL+"/approvals")
	if code != 200 || !strings.Contains(string(body), `"second"`) {
		t.Fatalf("reopen should re-list: %d %s", code, body)
	}

	// ghost portal token → 401
	if code, _ := h.doPortal("GET", "/p/"+"x"+strings.Repeat("y", 50)+"/approvals"); code != 401 {
		t.Fatal("bad portal token should 401")
	}

	// audit: decided (contact actor) + reopened
	var decided, reopened int
	ap.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action='approval.decided' AND actor_type='contact'`).Scan(&decided)
	ap.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action='approval.reopened'`).Scan(&reopened)
	if decided != 2 || reopened != 1 {
		t.Fatalf("audits decided=%d reopened=%d", decided, reopened)
	}
}
