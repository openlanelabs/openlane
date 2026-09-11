//go:build integration

package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestRequiredFieldsGate: tasks.required_fields block work start until
// filled (§516.8 — the Rocketlane gap).
func TestRequiredFieldsGate(t *testing.T) {
	_, _, h := filesTestStack(t)

	// task with required description + due_at, neither set
	var task struct {
		ID string `json:"id"`
	}
	if code, body := h.do("POST", "/v1/projects/"+projA+"/tasks", map[string]any{
		"title": "Gated task", "owner_type": "internal",
		"required_fields": []string{"description", "due_at"},
	}); code != http.StatusCreated {
		t.Fatalf("create = %d %s", code, body)
	} else if err := json.Unmarshal(body, &task); err != nil {
		t.Fatal(err)
	}

	// start → 400 naming both
	if code, body := h.do("PATCH", "/v1/tasks/"+task.ID, map[string]any{
		"status": "in_progress",
	}); code != http.StatusBadRequest || !strings.Contains(string(body), "description") || !strings.Contains(string(body), "due_at") {
		t.Fatalf("gate = %d %s", code, body)
	}

	// same PATCH carries the fields → passes
	if code, body := h.do("PATCH", "/v1/tasks/"+task.ID, map[string]any{
		"status":         "in_progress",
		"description_md": "now filled",
		"due_at":         "2026-10-01T00:00:00Z",
	}); code != http.StatusOK {
		t.Fatalf("gate with fields = %d %s", code, body)
	}

	// unknown keys dropped on write
	if code, body := h.do("POST", "/v1/projects/"+projA+"/tasks", map[string]any{
		"title": "Unknown keys", "owner_type": "internal",
		"required_fields": []string{"description", "bogus"},
	}); code != http.StatusCreated {
		t.Fatalf("create2 = %d %s", code, body)
	} else {
		var out struct {
			RequiredFields []string `json:"required_fields"`
		}
		_ = json.Unmarshal(body, &out)
		if len(out.RequiredFields) != 1 || out.RequiredFields[0] != "description" {
			t.Fatalf("sanitize = %v", out.RequiredFields)
		}
	}

	// clearing required_fields via PATCH re-allows unguarded transitions
	if code, body := h.do("PATCH", "/v1/tasks/"+task.ID, map[string]any{
		"required_fields": []string{},
	}); code != http.StatusOK {
		t.Fatalf("clear = %d %s", code, body)
	}
}

// TestProjectNotes: timestamped append-only feed (§516.8).
func TestProjectNotes(t *testing.T) {
	_, _, h := filesTestStack(t)

	// 404 on foreign project
	if code, _ := h.do("POST", "/v1/projects/99999999-9999-9999-9999-999999999999/notes", map[string]any{
		"body": "x",
	}); code != http.StatusNotFound {
		t.Fatalf("foreign = %d", code)
	}

	// create two notes
	if code, body := h.do("POST", "/v1/projects/"+projA+"/notes", map[string]any{
		"body": "Kickoff done",
	}); code != http.StatusCreated {
		t.Fatalf("note1 = %d %s", code, body)
	}
	if code, _ := h.do("POST", "/v1/projects/"+projA+"/notes", map[string]any{
		"body": "Sent SOW draft",
	}); code != http.StatusCreated {
		t.Fatalf("note2 = %d", code)
	}

	// list: newest first, author + timestamp present
	if code, body := h.do("GET", "/v1/projects/"+projA+"/notes", nil); code != http.StatusOK {
		t.Fatalf("list = %d %s", code, body)
	} else {
		var out []struct {
			Author    string `json:"author"`
			Body      string `json:"body"`
			CreatedAt string `json:"created_at"`
		}
		if err := json.Unmarshal(body, &out); err != nil || len(out) != 2 {
			t.Fatalf("notes = %s", body)
		}
		if out[0].Body != "Sent SOW draft" || out[1].Body != "Kickoff done" {
			t.Fatalf("order = %v", out)
		}
		if out[0].Author == "" || out[0].CreatedAt == "" {
			t.Fatalf("timestamped note incomplete: %+v", out[0])
		}
	}

	// body bounds
	if code, _ := h.do("POST", "/v1/projects/"+projA+"/notes", map[string]any{
		"body": strings.Repeat("x", 2001),
	}); code != http.StatusBadRequest {
		t.Fatalf("too long = %d", code)
	}
}
