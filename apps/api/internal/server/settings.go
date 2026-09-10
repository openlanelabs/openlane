package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

func (s *Server) putSlackSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		WebhookURL          string `json:"webhook_url"`
		NotifyTaskCompleted *bool  `json:"notify_task_completed"`
		NotifyProjectCreate *bool  `json:"notify_project_created"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if !slackAllowed(strings.TrimSpace(req.WebhookURL)) {
		problem(w, http.StatusBadRequest, "webhook_url must be a hooks.slack.com or *.slack.com/services/ https URL")
		return
	}
	nt := true
	if req.NotifyTaskCompleted != nil {
		nt = *req.NotifyTaskCompleted
	}
	np := true
	if req.NotifyProjectCreate != nil {
		np = *req.NotifyProjectCreate
	}

	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	ctx := r.Context()
	if _, err := tx.Exec(ctx, `
		INSERT INTO workspace_settings (workspace_id, slack_webhook_url, notify_task_completed, notify_project_created)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, $2, $3)
		ON CONFLICT (workspace_id) DO UPDATE SET
		  slack_webhook_url = $1, notify_task_completed = $2, notify_project_created = $3, updated_at = now()`,
		req.WebhookURL, nt, np); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getSlackSettings(w http.ResponseWriter, r *http.Request) {
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	var configured bool
	var nt, np bool
	// URL is deliberately NOT returned — configured flag only
	err := tx.QueryRow(r.Context(), `
		SELECT slack_webhook_url IS NOT NULL AND slack_webhook_url <> '', notify_task_completed, notify_project_created
		FROM workspace_settings
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid`).
		Scan(&configured, &nt, &np)
	if err == pgx.ErrNoRows {
		writeJSON(w, http.StatusOK, map[string]any{"configured": false, "notify_task_completed": true, "notify_project_created": true})
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"configured": configured, "notify_task_completed": nt, "notify_project_created": np})
}

func (s *Server) deleteSlackSettings(w http.ResponseWriter, r *http.Request) {
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	if _, err := tx.Exec(r.Context(), `
		DELETE FROM workspace_settings
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid`); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
