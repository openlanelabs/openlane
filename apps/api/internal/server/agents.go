package server

import (
	"encoding/json"
	"net/http"
)

// Agent runs audit surface (P2 §15): billing transparency — we show
// cost, unlike Nitro black-box. Manager+ for reads (flagging members'
// missing days is management telemetry); admin for the kill switch.

type agentRunJSON struct {
	ID         string  `json:"id"`
	Agent      string  `json:"agent"`
	Status     string  `json:"status"`
	Model      string  `json:"model"`
	CostCents  int     `json:"cost_cents"`
	InputRef   string  `json:"input_ref"`
	OutputRef  string  `json:"output_ref"`
	Error      string  `json:"error"`
	ApprovedBy *string `json:"approved_by"`
	StartedAt  string  `json:"started_at"`
	FinishedAt *string `json:"finished_at"`
}

func (s *Server) listAgentRuns(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true), set_config('app.user_id', $2, true)",
		workspaceFromCtx(ctx), userFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows, err := tx.Query(ctx, `
		SELECT id::text, agent, status, model, cost_cents, input_ref, output_ref, error,
		       approved_by::text, started_at::text, finished_at::text
		FROM agent_runs
		ORDER BY created_at DESC
		LIMIT 100`)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []agentRunJSON{}
	for rows.Next() {
		var run agentRunJSON
		var approvedBy *string
		if err := rows.Scan(&run.ID, &run.Agent, &run.Status, &run.Model, &run.CostCents,
			&run.InputRef, &run.OutputRef, &run.Error, &approvedBy, &run.StartedAt, &run.FinishedAt); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		if approvedBy != nil && *approvedBy != "" {
			run.ApprovedBy = approvedBy
		}
		out = append(out, run)
	}
	if err := rows.Err(); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if out == nil {
		out = []agentRunJSON{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}

func (s *Server) getAgentsStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true), set_config('app.user_id', $2, true)",
		workspaceFromCtx(ctx), userFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	var enabled bool
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE((SELECT agents_enabled FROM workspace_settings
		 WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid), true)`).
		Scan(&enabled); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": enabled,
		"agents":  []string{"guardian", "mcp"}, // the v1 roster
	})
}

func (s *Server) putAgentsStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if roleFromCtx(ctx) != "admin" && roleFromCtx(ctx) != "owner" {
		problem(w, http.StatusForbidden, "admin role required")
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil || req.Enabled == nil {
		problem(w, http.StatusBadRequest, "enabled (boolean) required")
		return
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true), set_config('app.user_id', $2, true)",
		workspaceFromCtx(ctx), userFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workspace_settings (workspace_id) VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid)
		ON CONFLICT (workspace_id) DO NOTHING`); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workspace_settings SET agents_enabled = $1
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid`,
		*req.Enabled); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	newValue, _ := json.Marshal(map[string]bool{"enabled": *req.Enabled})
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'workspace',
		        NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'agents.kill_switch', 'api', $1)`,
		newValue); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"enabled": *req.Enabled})
}
