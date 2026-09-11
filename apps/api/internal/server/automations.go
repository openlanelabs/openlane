package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// automations.go — §408 v1: when/condition/action rules, unlimited
// runs, run history. The engine hooks notifyEvent: every event the API
// already emits is a trigger; the two P0 actions are slack_message
// (workspace settings webhook) and create_task.

var validTriggers = map[string]bool{
	"task.completed": true, "approval.requested": true, "approval.decided": true,
	"csat.submitted": true, "project.created": true, "project.created_from_template": true,
}

type automationOut struct {
	ID           string          `json:"id"`
	Name         string          `json:"name"`
	TriggerEvent string          `json:"trigger_event"`
	Condition    json.RawMessage `json:"condition"`
	Action       json.RawMessage `json:"action"`
	IsActive     bool            `json:"is_active"`
	CreatedAt    string          `json:"created_at"`
}

func validateAutomation(name, trigger string, condition, action map[string]any) string {
	if name == "" || len(name) > 255 {
		return "name must be 1-255 chars"
	}
	if !validTriggers[trigger] {
		return "unknown trigger_event"
	}
	for k := range condition {
		if k != "project_id" {
			return "condition supports at most project_id"
		}
		if pid, ok := condition[k].(string); !ok || len(pid) != 36 {
			return "condition.project_id must be a uuid"
		}
	}
	act, _ := action["type"].(string)
	switch act {
	case "slack_message":
		// target is the workspace settings webhook — no URL stored in
		// the rule (SSRF surface stays the allowlisted one)
		return ""
	case "create_task":
		pid, _ := action["project_id"].(string)
		title, _ := action["title"].(string)
		if len(pid) != 36 {
			return "create_task requires project_id (uuid)"
		}
		if title == "" || len(title) > 255 {
			return "create_task requires title (1-255)"
		}
		return ""
	default:
		return "action.type must be slack_message or create_task"
	}
}

func scanAutomation(row pgx.Row) (automationOut, error) {
	var a automationOut
	var created time.Time
	err := row.Scan(&a.ID, &a.Name, &a.TriggerEvent, &a.Condition, &a.Action, &a.IsActive, &created)
	a.CreatedAt = created.UTC().Format(time.RFC3339)
	return a, err
}

// POST /v1/automations
func (s *Server) createAutomation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         string         `json:"name"`
		TriggerEvent string         `json:"trigger_event"`
		Condition    map[string]any `json:"condition"`
		Action       map[string]any `json:"action"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.Condition == nil {
		req.Condition = map[string]any{}
	}
	if msg := validateAutomation(req.Name, req.TriggerEvent, req.Condition, req.Action); msg != "" {
		problem(w, http.StatusBadRequest, msg)
		return
	}
	ctx := r.Context()
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	a, err := scanAutomation(tx.QueryRow(ctx, `
		INSERT INTO automations (workspace_id, name, trigger_event, condition, action, created_by)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, $2, $3, $4,
		        NULLIF(current_setting('app.user_id', true), '')::uuid)
		RETURNING id, name, trigger_event, condition, action, is_active, created_at`,
		req.Name, req.TriggerEvent, mustJSON(req.Condition), mustJSON(req.Action)))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'automation', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'automation.created', 'api')`, a.ID); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, a)
}

// GET /v1/automations
func (s *Server) listAutomations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT id, name, trigger_event, condition, action, is_active, created_at
		FROM automations WHERE deleted_at IS NULL
		ORDER BY created_at DESC`)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []automationOut{}
	for rows.Next() {
		a, err := scanAutomationRow(rows)
		if err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		out = append(out, a)
	}
	writeJSON(w, http.StatusOK, out)
}

func scanAutomationRow(rows pgx.Rows) (automationOut, error) {
	var a automationOut
	var created time.Time
	err := rows.Scan(&a.ID, &a.Name, &a.TriggerEvent, &a.Condition, &a.Action, &a.IsActive, &created)
	a.CreatedAt = created.UTC().Format(time.RFC3339)
	return a, err
}

// PATCH /v1/automations/{id} — is_active toggle (v1: the only edit)
func (s *Server) patchAutomation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IsActive *bool `json:"is_active"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&req); err != nil || req.IsActive == nil {
		problem(w, http.StatusBadRequest, `{"is_active": true|false} required`)
		return
	}
	ctx := r.Context()
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id string
	err := tx.QueryRow(ctx, `
		UPDATE automations SET is_active = $2 WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING id`, r.PathValue("id"), *req.IsActive).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "automation not found")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "is_active": *req.IsActive})
}

// DELETE /v1/automations/{id} — soft delete
func (s *Server) deleteAutomation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id string
	err := tx.QueryRow(ctx, `
		UPDATE automations SET deleted_at = now() WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING id`, r.PathValue("id")).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "automation not found")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GET /v1/automations/{id}/runs — run history (§408)
func (s *Server) listAutomationRuns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT id::text, event, COALESCE(event_ref,''), status, COALESCE(detail,''), created_at
		FROM automation_runs
		WHERE automation_id = $1::uuid
		ORDER BY created_at DESC LIMIT 100`, r.PathValue("id"))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	type runOut struct {
		ID        string `json:"id"`
		Event     string `json:"event"`
		EventRef  string `json:"event_ref,omitempty"`
		Status    string `json:"status"`
		Detail    string `json:"detail,omitempty"`
		CreatedAt string `json:"created_at"`
	}
	out := []runOut{}
	for rows.Next() {
		var rn runOut
		var created time.Time
		if err := rows.Scan(&rn.ID, &rn.Event, &rn.EventRef, &rn.Status, &rn.Detail, &created); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		rn.CreatedAt = created.UTC().Format(time.RFC3339)
		out = append(out, rn)
	}
	writeJSON(w, http.StatusOK, out)
}

// fireAutomations: the engine. Runs every active rule whose trigger
// matches; condition.project_id filters to that project when the event
// carries a ref. Each execution records a run row (completed/failed/
// skipped) — unlimited runs, full history.
func (s *Server) fireAutomations(ctx context.Context, workspaceID, event string, ref ...string) {
	if workspaceID == "" {
		return
	}
	eventRef := ""
	if len(ref) > 0 {
		eventRef = ref[0]
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, false), set_config('app.portal_token_hash', '', false)", workspaceID); err != nil {
		return
	}
	rows, err := conn.Query(ctx, `
		SELECT id::text, condition, action FROM automations
		WHERE trigger_event = $1 AND is_active AND deleted_at IS NULL`, event)
	if err != nil {
		return
	}
	type rule struct {
		id        string
		condition map[string]any
		action    map[string]any
	}
	var rules []rule
	for rows.Next() {
		var r rule
		if err := rows.Scan(&r.id, &r.condition, &r.action); err != nil {
			rows.Close()
			return
		}
		rules = append(rules, r)
	}
	rows.Close()
	for _, r := range rules {
		s.runAutomation(ctx, conn, workspaceID, event, eventRef, r.id, r.condition, r.action)
	}
}

func (s *Server) runAutomation(ctx context.Context, conn *pgxpool.Conn, workspaceID, event, eventRef, id string, condition, action map[string]any) {
	record := func(status, detail string) {
		_, _ = conn.Exec(ctx, `
			INSERT INTO automation_runs (workspace_id, automation_id, event, event_ref, status, detail)
			VALUES ($1::uuid, $2::uuid, $3, NULLIF($4,''), $5, NULLIF($6,''))`,
			workspaceID, id, event, eventRef, status, detail)
	}
	// condition: project_id must equal the event's project (eventRef is
	// the entity id — the engine resolves its project via SQL when the
	// condition needs it)
	if pid, ok := condition["project_id"].(string); ok && pid != "" {
		if eventRef == "" {
			record("skipped", "condition needs project_id but event carries no ref")
			return
		}
		var match bool
		if err := conn.QueryRow(ctx, `
			SELECT COALESCE(
			  (SELECT t.project_id::text = $2 FROM tasks t WHERE t.id = $1::uuid), false)
			OR COALESCE(
			  (SELECT a.project_id::text = $2 FROM approvals a WHERE a.id = $1::uuid), false)
			OR COALESCE(
			  (SELECT p.id::text = $2 FROM projects p WHERE p.id = $1::uuid), false)`,
			eventRef, pid).Scan(&match); err != nil || !match {
			record("skipped", "event project does not match condition")
			return
		}
	}
	switch action["type"] {
	case "slack_message":
		var url string
		if err := conn.QueryRow(ctx, `
			SELECT slack_webhook_url FROM workspace_settings
			WHERE workspace_id = $1 AND slack_webhook_url <> ''`, workspaceID).Scan(&url); err != nil {
			record("failed", "no slack webhook configured")
			return
		}
		if err := s.enqueueSlackEvent(ctx, workspaceID, url, "🤖 "+event); err != nil {
			record("failed", "enqueue: "+err.Error())
			return
		}
		record("completed", "slack_message queued")
	case "create_task":
		pid, _ := action["project_id"].(string)
		title, _ := action["title"].(string)
		vis, _ := action["customer_visible"].(bool)
		var taskID string
		err := func() error {
			tx, err := conn.Begin(ctx)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx,
				"SELECT set_config('app.workspace_id', $1, true)", workspaceID); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `
				INSERT INTO tasks (workspace_id, project_id, title, owner_type, customer_visible)
				SELECT p.workspace_id, p.id, $2, 'internal', $3
				FROM projects p WHERE p.id = $1::uuid AND p.deleted_at IS NULL
				RETURNING id::text`, pid, title, vis).Scan(&taskID); err != nil {
				return err
			}
			return tx.Commit(ctx)
		}()
		if err != nil {
			record("failed", "create_task: "+err.Error())
			return
		}
		record("completed", "task "+taskID+" created")
	default:
		record("failed", "unknown action type")
	}
}
