//go:build integration

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestAutomationsCRUD(t *testing.T) {
	_, _, h := filesTestStack(t)

	// invalid: bad trigger, bad action
	if code, _ := h.do("POST", "/v1/automations", map[string]any{
		"name": "x", "trigger_event": "moon.full", "action": map[string]any{"type": "slack_message"},
	}); code != 400 {
		t.Fatal("bad trigger should 400")
	}
	if code, _ := h.do("POST", "/v1/automations", map[string]any{
		"name": "x", "trigger_event": "task.completed", "action": map[string]any{"type": "email"},
	}); code != 400 {
		t.Fatal("bad action should 400")
	}
	// create_task needs uuid project + title
	if code, _ := h.do("POST", "/v1/automations", map[string]any{
		"name": "x", "trigger_event": "task.completed",
		"action": map[string]any{"type": "create_task", "project_id": "nope", "title": "t"},
	}); code != 400 {
		t.Fatal("bad project_id should 400")
	}

	// create two
	mk := func(name string) string {
		code, body := h.do("POST", "/v1/automations", map[string]any{
			"name": name, "trigger_event": "task.completed",
			"action": map[string]any{"type": "slack_message"},
		})
		if code != http.StatusCreated {
			t.Fatalf("create %q = %d %s", name, code, body)
		}
		var a automationOut
		json.Unmarshal(body, &a)
		return a.ID
	}
	id1 := mk("Rule A")
	id2 := mk("Rule B")

	// list shows both
	code, body := h.do("GET", "/v1/automations", nil)
	if code != 200 || strings.Count(string(body), `"name"`) != 2 {
		t.Fatalf("list = %d %s", code, body)
	}
	// deactivate A
	if code, _ := h.do("PATCH", "/v1/automations/"+id1, map[string]any{"is_active": false}); code != 200 {
		t.Fatal("patch failed")
	}
	// delete B
	if code, _ := h.do("DELETE", "/v1/automations/"+id2, nil); code != 204 {
		t.Fatal("delete failed")
	}
	// list: A remains (inactive), B gone
	code, body = h.do("GET", "/v1/automations", nil)
	if code != 200 || !strings.Contains(string(body), "Rule A") ||
		strings.Contains(string(body), "Rule B") {
		t.Fatalf("after toggle+delete: %s", body)
	}
	// ghost patch → 404
	if code, _ := h.do("PATCH", "/v1/automations/99999999-9999-9999-9999-999999999999",
		map[string]any{"is_active": true}); code != 404 {
		t.Fatal("ghost patch should 404")
	}
}

func TestAutomationsEngine(t *testing.T) {
	h, _ := riverTestStack(t)
	admin := adminDSN(t)
	ap, _ := pgxpool.New(context.Background(), admin)
	defer ap.Close()

	// rule: on task.completed in projA → create a follow-up task
	code, body := h.do("POST", "/v1/automations", map[string]any{
		"name": "Follow-up after completion", "trigger_event": "task.completed",
		"condition": map[string]any{"project_id": projA},
		"action": map[string]any{
			"type": "create_task", "project_id": projA,
			"title": "Post-completion check", "customer_visible": true,
		},
	})
	if code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	}
	var rule struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &rule)

	// create a task in projA and complete it via staff
	code, body = h.do("POST", "/v1/projects/"+projA+"/tasks", map[string]any{
		"title": "Deploy to prod", "owner_type": "internal", "customer_visible": true,
	})
	if code != 201 {
		t.Fatalf("task = %d %s", code, body)
	}
	var task struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &task)
	complete := func(id string) {
		t.Helper()
		if code, body := h.do("PATCH", "/v1/tasks/"+id, map[string]any{"status": "in_progress"}); code != 200 {
			t.Fatalf("start = %d %s", code, body)
		}
		if code, body := h.do("PATCH", "/v1/tasks/"+id, map[string]any{"status": "done"}); code != 200 {
			t.Fatalf("done = %d %s", code, body)
		}
	}
	complete(task.ID)

	// engine ran synchronously (fireAutomations is in-process): follow-up
	// task exists + run row recorded
	var follow int
	ap.QueryRow(context.Background(),
		`SELECT count(*) FROM tasks WHERE title = 'Post-completion check'`).Scan(&follow)
	if follow != 1 {
		t.Fatal("automation did not create the follow-up task")
	}
	code, body = h.do("GET", "/v1/automations/"+rule.ID+"/runs", nil)
	if code != 200 || !strings.Contains(string(body), `"completed"`) {
		t.Fatalf("runs = %d %s", code, body)
	}

	// condition filter: complete a task in projB → no run, no task.
	// projB not seeded in filesTestStack? use ghost: create task under projA,
	// but with condition on a DIFFERENT project id... simpler: seed a second
	// project via admin and complete a task there:
	ap.Exec(context.Background(), `SELECT set_config('app.workspace_id', $1, false)`, wsA)
	var projX string
	ap.QueryRow(context.Background(), `INSERT INTO projects (workspace_id, customer_id, name, status)
		VALUES ($1, $2, 'Condition target', 'active') RETURNING id::text`, wsA, custID).Scan(&projX)
	ap.Exec(context.Background(), `SELECT set_config('app.workspace_id', '', false)`)
	code, body = h.do("POST", "/v1/projects/"+projX+"/tasks", map[string]any{
		"title": "Other project task", "owner_type": "internal",
	})
	if code != 201 {
		t.Fatalf("task2 = %d %s", code, body)
	}
	var task2 struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &task2)
	complete(task2.ID)

	var runs int
	ap.QueryRow(context.Background(),
		`SELECT count(*) FROM automation_runs WHERE automation_id = $1 AND status='completed'`,
		rule.ID).Scan(&runs)
	if runs != 1 { // only the projA completion ran; the projX one skipped
		t.Fatalf("runs completed = %d, want 1", runs)
	}
	var skipped int
	ap.QueryRow(context.Background(),
		`SELECT count(*) FROM automation_runs WHERE automation_id = $1 AND status='skipped'`,
		rule.ID).Scan(&skipped)
	if skipped != 1 {
		t.Fatalf("runs skipped = %d, want 1", skipped)
	}
	var follow2 int
	ap.QueryRow(context.Background(),
		`SELECT count(*) FROM tasks WHERE title = 'Post-completion check'`).Scan(&follow2)
	if follow2 != 1 {
		t.Fatalf("follow-up should not duplicate: %d", follow2)
	}

	// deactivate → completing another projA task produces no new run
	h.do("PATCH", "/v1/automations/"+rule.ID, map[string]any{"is_active": false})
	code, body = h.do("POST", "/v1/projects/"+projA+"/tasks", map[string]any{
		"title": "Another", "owner_type": "internal",
	})
	var task3 struct {
		ID string `json:"id"`
	}
	json.Unmarshal(body, &task3)
	complete(task3.ID)
	ap.QueryRow(context.Background(),
		`SELECT count(*) FROM automation_runs WHERE automation_id = $1`, rule.ID).Scan(&runs)
	if runs != 2 {
		t.Fatalf("inactive rule should not run: %d", runs)
	}
}
