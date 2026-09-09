//go:build integration

package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestTasksCRUDStateMachine(t *testing.T) {
	srv, _, _, _ := importTestStack(t)
	h := &httptestSrv{t: t, URL: srv.URL}

	// create a project to hold tasks
	_, body := h.do("POST", "/v1/projects", map[string]any{"customer_id": custID, "name": "Tasks Test Project"})
	var proj struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &proj)
	if proj.ID == "" {
		t.Fatalf("project create failed: %s", body)
	}

	// create task (defaults)
	code, body := h.do("POST", "/v1/projects/"+proj.ID+"/tasks", map[string]any{"title": "Plan kickoff"})
	if code != 201 {
		t.Fatalf("create task = %d: %s", code, body)
	}
	var task struct {
		ID          string  `json:"id"`
		Status      string  `json:"status"`
		OwnerType   string  `json:"owner_type"`
		Required    bool    `json:"required"`
		CustomerVis bool    `json:"customer_visible"`
		CompletedAt *string `json:"completed_at"`
	}
	json.Unmarshal(body, &task)
	if task.Status != "todo" || task.OwnerType != "internal" || !task.Required || task.CustomerVis {
		t.Fatalf("defaults wrong: %+v", task)
	}

	// invalid title
	if code, _ := h.do("POST", "/v1/projects/"+proj.ID+"/tasks", map[string]any{"title": ""}); code != 400 {
		t.Fatalf("empty title = %d", code)
	}
	// invalid status
	if code, _ := h.do("POST", "/v1/projects/"+proj.ID+"/tasks", map[string]any{"title": "x", "status": "wat"}); code != 400 {
		t.Fatalf("bad status = %d", code)
	}
	// foreign project
	if code, _ := h.do("POST", "/v1/projects/99999999-9999-9999-9999-999999999999/tasks", map[string]any{"title": "x"}); code != 404 {
		t.Fatalf("foreign project = %d", code)
	}

	// §8.1: todo→done invalid (must go through in_progress)
	if code, _ := h.do("PATCH", "/v1/tasks/"+task.ID, map[string]any{"status": "done"}); code != 400 {
		t.Fatalf("todo→done = %d, want 400", code)
	}
	// todo→in_progress ok
	if code, body := h.do("PATCH", "/v1/tasks/"+task.ID, map[string]any{"status": "in_progress"}); code != 200 {
		t.Fatalf("todo→in_progress = %d: %s", code, body)
	}
	// in_progress→review→done
	if code, _ := h.do("PATCH", "/v1/tasks/"+task.ID, map[string]any{"status": "review"}); code != 200 {
		t.Fatal("in_progress→review failed")
	}
	code, body = h.do("PATCH", "/v1/tasks/"+task.ID, map[string]any{"status": "done"})
	if code != 200 {
		t.Fatalf("review→done = %d: %s", code, body)
	}
	json.Unmarshal(body, &task)
	if task.CompletedAt == nil {
		t.Fatal("done must set completed_at")
	}
	// done→todo without reason = 400
	if code, _ := h.do("PATCH", "/v1/tasks/"+task.ID, map[string]any{"status": "todo"}); code != 400 {
		t.Fatalf("reopen without reason = %d, want 400", code)
	}
	// done→todo with reason = 200 + audit task.reopened
	if code, _ := h.do("PATCH", "/v1/tasks/"+task.ID, map[string]any{"status": "todo", "reopen_reason": "scope changed"}); code != 200 {
		t.Fatal("reopen with reason failed")
	}
	// completed_at cleared on reopen
	code, body = h.do("GET", "/v1/projects/"+proj.ID+"/tasks", nil)
	var list []map[string]any
	json.Unmarshal(body, &list)
	for _, lt := range list {
		if lt["id"] == task.ID && lt["completed_at"] != nil {
			t.Fatal("completed_at must clear on reopen")
		}
	}

	// soft delete → 404
	if code, _ := h.do("DELETE", "/v1/tasks/"+task.ID, nil); code != 204 {
		t.Fatalf("delete = %d", code)
	}
	if code, _ := h.do("PATCH", "/v1/tasks/"+task.ID, map[string]any{"title": "x"}); code != 404 {
		t.Fatalf("patch deleted = %d, want 404", code)
	}
}

func TestPortalStillSeesOnlyCustomerVisible(t *testing.T) {
	// staff writes a mix of tasks; the portal (token-scoped) must see only customer_visible ones.
	srv, _, adminPool, ctx := importTestStack(t)
	h := &httptestSrv{t: t, URL: srv.URL}

	_, body := h.do("POST", "/v1/projects", map[string]any{"customer_id": custID, "name": "Portal Visibility"})
	var proj struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &proj)

	h.do("POST", "/v1/projects/"+proj.ID+"/tasks", map[string]any{"title": "Visible one", "customer_visible": true})
	h.do("POST", "/v1/projects/"+proj.ID+"/tasks", map[string]any{"title": "Secret one", "customer_visible": false})

	// mint a portal link for the project (staff API)
	code, body := h.do("POST", "/v1/portal-links", map[string]any{"project_id": proj.ID, "contact_id": "44444444-4444-4444-4444-444444444444"})
	if code != 201 {
		t.Fatalf("portal link = %d: %s", code, body)
	}
	var link struct {
		Token string `json:"token"`
	}
	json.Unmarshal(body, &link)

	// portal list — internal task must be absent
	code, body = portalGetRaw(t, srv.URL, link.Token)
	if code != 200 {
		t.Fatalf("portal tasks = %d", code)
	}
	if containsStr(body, "Secret one") {
		t.Fatal("internal task leaked to portal")
	}
	if !containsStr(body, "Visible one") {
		t.Fatal("customer-visible task missing from portal")
	}
	_ = adminPool
	_ = ctx
}

func portalGetRaw(t *testing.T, base, token string) (int, []byte) {
	t.Helper()
	return httpGetRaw(t, base+"/v1/portal/"+token+"/tasks")
}

func httpGetRaw(t *testing.T, url string) (int, []byte) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	out, _ := io.ReadAll(res.Body)
	return res.StatusCode, out
}

func containsStr(b []byte, s string) bool {
	return bytes.Contains(b, []byte(s))
}
