//go:build integration

package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func contains(b []byte, sub string) bool { return bytes.Contains(b, []byte(sub)) }

// jiraTestStack: river stack (fresh DB + integration key + river schema)
// + a task on projA + jira configured with a local stub server as
// instance_url.
type jiraStub struct {
	srv        *httptest.Server
	issueGet   atomic.Int32
	transGet   atomic.Int32
	transPost  atomic.Int32
	lastTrans  atomic.Value // string
	issuePaths map[string]bool
}

func jiraTestStack(t *testing.T) (*httptestSrv, *jiraStub, string, string) {
	t.Helper()
	_, _, h := filesTestStack(t)
	admin := adminDSN(t)
	ap, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ap.Close() })
	if err := EnsureRiver(context.Background(), ap); err != nil {
		t.Fatalf("EnsureRiver: %v", err)
	}
	if _, err := ap.Exec(context.Background(), "DELETE FROM river_job"); err != nil {
		t.Fatalf("river_job cleanup: %v", err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	t.Setenv("OPENLANE_INTEGRATION_KEY", base64.StdEncoding.EncodeToString(key))

	// a task to link
	if code, body := h.do("POST", "/v1/projects/"+projA+"/tasks", map[string]any{
		"title": "Jira task", "owner_type": "internal",
	}); code != http.StatusCreated {
		t.Fatalf("create task = %d %s", code, body)
	}
	var items []struct {
		ID string `json:"id"`
	}
	if code, body := h.do("GET", "/v1/projects/"+projA+"/tasks", nil); code != http.StatusOK {
		t.Fatalf("list tasks = %d %s", code, body)
	} else if err := json.Unmarshal(body, &items); err != nil || len(items) == 0 {
		t.Fatalf("no tasks: %s", body)
	}
	taskID := items[0].ID

	// local jira: issue fetch + transitions list/post
	stub := &jiraStub{}
	stub.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/transitions") && r.Method == "GET":
			stub.transGet.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"transitions": []map[string]any{
				{"id": "11", "name": "To Do"},
				{"id": "31", "name": "In Progress"},
				{"id": "41", "name": "Done"},
			}})
		case r.Method == "GET" && len(r.URL.Path) > len("/rest/api/3/issue/"):
			stub.issueGet.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "10001", "key": "ACME-7"})
		case r.Method == "POST":
			stub.transPost.Add(1)
			var b struct {
				Transition struct {
					ID string `json:"id"`
				} `json:"transition"`
			}
			_ = json.NewDecoder(r.Body).Decode(&b)
			stub.lastTrans.Store(b.Transition.ID)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(func() { stub.srv.Close() })

	// configure jira: stub as instance + PAT
	secret := "jira-webhook-secret-key"
	if code, body := h.do("PUT", "/v1/integrations/jira", map[string]any{
		"instance_url":   stub.srv.URL,
		"webhook_secret": secret,
		"pat":            "stub-pat-token",
	}); code != http.StatusOK {
		t.Fatalf("put jira settings = %d %s", code, body)
	}
	return h, stub, taskID, secret
}

func TestJiraSettingsAndLink(t *testing.T) {
	h, stub, taskID, _ := jiraTestStack(t)

	// GET: configured, no secret/PAT echo
	code, body := h.do("GET", "/v1/integrations/jira", nil)
	if code != http.StatusOK || !contains(body, `"configured":true`) || contains(body, "stub-pat") {
		t.Fatalf("get settings = %d %s", code, body)
	}

	// link the task (validated against the stub → issue_get hit)
	if code, body := h.do("POST", "/v1/tasks/"+taskID+"/link", map[string]any{
		"issue_key": "ACME-7",
	}); code != http.StatusCreated {
		t.Fatalf("link = %d %s", code, body)
	}
	if stub.issueGet.Load() != 1 {
		t.Fatalf("expected 1 issue GET, got %d", stub.issueGet.Load())
	}

	// relink: upsert OK
	if code, _ := h.do("POST", "/v1/tasks/"+taskID+"/link", map[string]any{
		"issue_key": "ACME-7",
	}); code != http.StatusCreated {
		t.Fatalf("relink = %d", code)
	}

	// unknown task → 404
	if code, _ := h.do("POST", "/v1/tasks/99999999-9999-9999-9999-999999999999/link", map[string]any{
		"issue_key": "ACME-7",
	}); code != http.StatusNotFound {
		t.Fatalf("link unknown task = %d", code)
	}

	// unlink → 204, second unlink → 404
	if code, _ := h.do("DELETE", "/v1/tasks/"+taskID+"/link", nil); code != http.StatusNoContent {
		t.Fatalf("unlink = %d", code)
	}
	if code, _ := h.do("DELETE", "/v1/tasks/"+taskID+"/link", nil); code != http.StatusNotFound {
		t.Fatalf("second unlink = %d", code)
	}
}

func TestJiraWebhookSyncsStatusAndComments(t *testing.T) {
	h, _, taskID, secret := jiraTestStack(t)

	if code, _ := h.do("POST", "/v1/tasks/"+taskID+"/link", map[string]any{
		"issue_key": "ACME-7",
	}); code != http.StatusCreated {
		t.Fatalf("link = %d", code)
	}

	post := func(ev map[string]any) (int, []byte) {
		raw, _ := json.Marshal(ev)
		req, _ := jsonBodyRequest(t, h, "POST", "/v1/integrations/jira/webhook?ws=acme", raw)
		mac := hmacSHA256([]byte(secret), raw)
		req.Header.Set("X-OpenLane-Signature", base64.StdEncoding.EncodeToString(mac))
		return doRequest(t, req)
	}

	// status: In Progress → task in_progress
	code, body := post(map[string]any{
		"webhookEvent": "jira:issue_updated",
		"issue": map[string]any{
			"key": "ACME-7",
			"fields": map[string]any{
				"status": map[string]any{"name": "In Progress"},
			},
		},
	})
	if code != http.StatusOK || !contains(body, "synced") {
		t.Fatalf("status sync = %d %s", code, body)
	}
	var task struct {
		Status string `json:"status"`
	}
	if code, b2 := h.do("GET", "/v1/projects/"+projA+"/tasks", nil); code != http.StatusOK {
		t.Fatalf("list = %d %s", code, b2)
	} else {
		var out []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		if err := json.Unmarshal(b2, &out); err != nil {
			t.Fatal(err)
		}
		for _, it := range out {
			if it.ID == taskID {
				task.Status = it.Status
			}
		}
	}
	if task.Status != "in_progress" {
		t.Fatalf("task status = %q, want in_progress", task.Status)
	}

	// illegal jira status transition (done from in_progress is legal; test
	// that a Done event lands, then a bogus unknown status is ignored)
	if code, _ := post(map[string]any{
		"webhookEvent": "jira:issue_updated",
		"issue": map[string]any{
			"key": "ACME-7",
			"fields": map[string]any{
				"status": map[string]any{"name": "Done"},
			},
		},
	}); code != http.StatusOK {
		t.Fatalf("done sync = %d", code)
	}
	if code, _ := post(map[string]any{
		"webhookEvent": "jira:issue_updated",
		"issue": map[string]any{
			"key": "ACME-7",
			"fields": map[string]any{
				"status": map[string]any{"name": "Gibberish"},
			},
		},
	}); code != http.StatusOK {
		t.Fatalf("unknown status = %d", code)
	}

	// comment: ingested as a task message
	code, body = post(map[string]any{
		"webhookEvent": "comment_created",
		"issue":        map[string]any{"key": "ACME-7"},
		"comment": map[string]any{
			"id":   "99001",
			"body": "Deployed to staging",
			"author": map[string]any{
				"emailAddress": "asha@acme.test",
				"displayName":  "Asha",
			},
		},
	})
	if code != http.StatusOK || !contains(body, "synced") {
		t.Fatalf("comment = %d %s", code, body)
	}
	// task messages? listed via staff endpoint
	if code, body := h.do("GET", "/v1/tasks/"+taskID+"/messages", nil); code != http.StatusOK || !contains(body, "Deployed to staging") {
		t.Fatalf("messages = %d %s", code, body)
	}

	// duplicate comment: no second insert
	post(map[string]any{
		"webhookEvent": "comment_created",
		"issue":        map[string]any{"key": "ACME-7"},
		"comment": map[string]any{
			"id":     "99001",
			"body":   "Deployed to staging",
			"author": map[string]any{"emailAddress": "asha@acme.test"},
		},
	})
	if code, bodyMsg := h.do("GET", "/v1/tasks/"+taskID+"/messages", nil); code != http.StatusOK {
		t.Fatalf("messages 2 = %d %s", code, bodyMsg)
	} else {
		var out []json.RawMessage
		_ = json.Unmarshal(bodyMsg, &out)
		if len(out) != 1 {
			t.Fatalf("dedupe failed: %d messages", len(out))
		}
	}

	// unlinked key: ignored, no oracle
	if code, body := post(map[string]any{
		"webhookEvent": "jira:issue_updated",
		"issue": map[string]any{
			"key":    "OTHER-1",
			"fields": map[string]any{"status": map[string]any{"name": "Done"}},
		},
	}); code != http.StatusOK || !contains(body, "no link") {
		t.Fatalf("unlinked = %d %s", code, body)
	}

	// bad signature → 401
	raw, _ := json.Marshal(map[string]any{"webhookEvent": "jira:issue_updated"})
	req, _ := jsonBodyRequest(t, h, "POST", "/v1/integrations/jira/webhook?ws=acme", raw)
	req.Header.Set("X-OpenLane-Signature", "bad")
	if code, _ := doRequest(t, req); code != http.StatusUnauthorized {
		t.Fatalf("bad sig = %d", code)
	}
}

func TestJiraOutboundPush(t *testing.T) {
	h, stub, taskID, _ := jiraTestStack(t)

	if code, _ := h.do("POST", "/v1/tasks/"+taskID+"/link", map[string]any{
		"issue_key": "ACME-7",
	}); code != http.StatusCreated {
		t.Fatalf("link = %d", code)
	}

	// PATCH status → in_progress: should enqueue jira_status_push
	if code, body := h.do("PATCH", "/v1/tasks/"+taskID, map[string]any{
		"status": "in_progress",
	}); code != http.StatusOK {
		t.Fatalf("patch = %d %s", code, body)
	}

	// drain the queue by running the worker logic inline: fetch the job args
	// and call pushJiraStatus directly (the worker's Work is in the worker
	// app; the API-side push fn is the same logic).
	admin := adminDSN(t)
	ap, err := pgxpool.New(context.Background(), admin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ap.Close() })
	var ws, issueKey string
	var args []byte
	if err := ap.QueryRow(context.Background(), `
		SELECT args::jsonb->>'workspace_id', args::jsonb->>'issue_key', args::jsonb::text
		FROM river_job WHERE kind = 'jira_status_push' LIMIT 1`).Scan(&ws, &issueKey, &args); err != nil {
		t.Fatalf("no jira_status_push job queued: %v", err)
	}
	var st struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(args, &st); err != nil || st.Status != "in_progress" {
		t.Fatalf("job args = %s", args)
	}

	// execute the push (mirror of the worker's HTTP path)
	s := &Server{pool: ap}
	if err := s.pushJiraStatus(context.Background(), ws, taskID, issueKey, st.Status); err != nil {
		t.Fatalf("push: %v", err)
	}
	if stub.transPost.Load() != 1 || stub.lastTrans.Load() != "31" {
		t.Fatalf("transition calls = %d last = %v", stub.transPost.Load(), stub.lastTrans.Load())
	}
	// audit + last_synced_at written
	var n int
	if err := ap.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_logs WHERE action = 'task.jira_sync_out'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("sync_out audit = %d err %v", n, err)
	}

	// loop guard: push again with no fresh local change → no-op
	if err := s.pushJiraStatus(context.Background(), ws, taskID, issueKey, st.Status); err != nil {
		t.Fatalf("push2: %v", err)
	}
	if stub.transPost.Load() != 1 {
		t.Fatalf("loop guard failed: %d calls", stub.transPost.Load())
	}
}
