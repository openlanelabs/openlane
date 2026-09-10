//go:build integration

package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSlackNotifications(t *testing.T) {
	var mu sync.Mutex
	var received []string
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var msg struct{ Text string `json:"text"` }
		json.Unmarshal(b, &msg)
		mu.Lock()
		received = append(received, msg.Text)
		mu.Unlock()
		w.WriteHeader(200)
	}))
	defer sink.Close()

	srv, _, adminPool, ctx := importTestStack(t)
	h := &httptestSrv{t: t, URL: srv.URL}

	// allowlist bypass for the httptest sink (CI-only env, mirrors OPENLANE_SLACK_ALLOW_ANY)
	t.Setenv("OPENLANE_SLACK_ALLOW_ANY", "1")

	// configure the webhook
	code, _ := h.do("PUT", "/v1/settings/slack", map[string]any{"webhook_url": sink.URL})
	if code != 204 {
		t.Fatalf("put settings = %d", code)
	}
	// status shows configured, URL never echoed
	code, body := h.do("GET", "/v1/settings/slack", nil)
	if code != 200 || !strings.Contains(string(body), `"configured":true`) || strings.Contains(string(body), "127.0.0.1") {
		t.Fatalf("get settings = %d %s", code, body)
	}
	// non-slack URL rejected even with allowlist off
	t.Setenv("OPENLANE_SLACK_ALLOW_ANY", "")
	if code, _ := h.do("PUT", "/v1/settings/slack", map[string]any{"webhook_url": "https://evil.example.com/hook"}); code != 400 {
		t.Fatalf("SSRF guard = %d, want 400", code)
	}
	t.Setenv("OPENLANE_SLACK_ALLOW_ANY", "1")

	// create a project → project.created fires
	_, body = h.do("POST", "/v1/projects", map[string]any{"customer_id": custID, "name": "Slack Notify Project", "status": "draft"})
	var proj struct{ ID string `json:"id"` }
	json.Unmarshal(body, &proj)
	if proj.ID == "" {
		t.Fatalf("project create failed: %s", body)
	}

	// complete a task via the STAFF path with done transition (todo→done is invalid; use in_progress→done)
	h.do("POST", "/v1/projects/"+proj.ID+"/tasks", map[string]any{"title": "Slack task"})
	var tasks []map[string]any
	_, body = h.do("GET", "/v1/projects/"+proj.ID+"/tasks", nil)
	json.Unmarshal(body, &tasks)
	taskID := tasks[0]["id"].(string)
	h.do("PATCH", "/v1/tasks/"+taskID, map[string]any{"status": "in_progress"})
	if code, _ := h.do("PATCH", "/v1/tasks/"+taskID, map[string]any{"status": "done"}); code != 200 {
		t.Fatal("task not completable")
	}

	// wait for async deliveries
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	var gotProject, gotTask bool
	for _, m := range received {
		if strings.Contains(m, "Slack Notify Project") && strings.Contains(m, "Project created") {
			gotProject = true
		}
		if strings.Contains(m, "Slack task") {
			gotTask = true
		}
	}
	if !gotProject {
		t.Fatalf("project.created not delivered: %v", received)
	}
	if !gotTask {
		t.Fatalf("task.completed not delivered: %v", received)
	}
	_ = adminPool
	_ = ctx
}
