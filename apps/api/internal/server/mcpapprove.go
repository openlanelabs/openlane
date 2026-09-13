package server

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Staff decide for agent-proposed approvals (MCP write tools, §347):
// POST /v1/approvals/{id}/decide — manager+. On "approved", if the
// approval carries an agent-proposal payload (description prefix
// "agent-proposal:"), the payload executes INSIDE the decide tx:
// kind=task → INSERT tasks; kind=time → INSERT time_entries (draft).
// Rejection = no write. The decision + executed refs are recorded in
// decision_comment; audit + notify follow the house patterns.

func (s *Server) decideStaffApproval(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	var req struct {
		Decision string `json:"decision"`
		Comment  string `json:"comment"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.Decision != "approved" && req.Decision != "changes_requested" {
		problem(w, http.StatusBadRequest, `decision must be "approved" or "changes_requested"`)
		return
	}

	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// fetch + decide atomically: pending-only, RLS-scoped; foreign or
	// decided rows both 404 (no oracle)
	var id, title, desc string
	err := tx.QueryRow(ctx, `
		UPDATE approvals SET status = $2, decided_by = NULLIF(current_setting('app.user_id', true), '')::uuid,
		       decision_comment = NULLIF($3,''), decided_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL AND status = 'pending'
		RETURNING id, title, description`,
		r.PathValue("id"), req.Decision, req.Comment).Scan(&id, &title, &desc)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "approval not found")
			return
		}
		log.Printf("decideStaff fetch: %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	// agent-proposal payload? execute on approval, inside the same tx
	executed := ""
	if req.Decision == "approved" && strings.HasPrefix(desc, "agent-proposal:") {
		parts := strings.SplitN(strings.TrimPrefix(desc, "agent-proposal:"), ":", 2)
		if len(parts) == 2 {
			var p map[string]any
			if json.Unmarshal([]byte(parts[1]), &p) == nil {
				switch parts[0] {
				case "task":
					ref, err := execAgentTask(ctx, tx, p)
					if err != nil {
						problem(w, http.StatusInternalServerError, "payload execution failed: "+err.Error())
						return
					}
					executed = "task " + ref
				case "time":
					ref, err := execAgentTime(ctx, tx, p)
					if err != nil {
						problem(w, http.StatusInternalServerError, "payload execution failed: "+err.Error())
						return
					}
					executed = "time_entry " + ref
				}
			}
		}
	}
	if executed != "" {
		if _, err := tx.Exec(ctx, `UPDATE approvals SET decision_comment =
			concat_ws(' · ', NULLIF($2,''), 'executed: '||$3) WHERE id = $1`, id, req.Comment, executed); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		SELECT a.workspace_id, 'approval', a.id, 'user', NULLIF(current_setting('app.user_id', true), '')::uuid,
		       'approval.decided', 'api'
		FROM approvals a WHERE a.id = $1`, id); err != nil {
		log.Printf("decideStaff audit: %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		log.Printf("decideStaff commit: %v", err)
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.notifyEvent(ctx, workspaceFromCtx(ctx), "approval.decided",
		"👤 Manager "+req.Decision+": "+title, id)
	writeJSON(w, http.StatusOK, map[string]string{"status": req.Decision, "id": id, "executed": executed})
}

// execAgentTask: create the proposed task. RLS scopes project_id;
// a cross-tenant or deleted project id → no rows → error → tx rolls
// back (the approval stays pending for retry).
func execAgentTask(ctx context.Context, tx pgx.Tx, p map[string]any) (string, error) {
	projID, _ := p["project_id"].(string)
	title, _ := p["title"].(string)
	desc, _ := p["description"].(string)
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO tasks (workspace_id, project_id, title, description_md, owner_type)
		SELECT NULLIF(current_setting('app.workspace_id', true), '')::uuid, pr.id, $2, NULLIF($3,''), 'agent'
		FROM projects pr WHERE pr.id = $1::uuid AND pr.deleted_at IS NULL
		RETURNING id::text`, projID, title, desc).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errors.New("project not found (deleted or wrong workspace) — approval left pending")
	}
	return id, err
}

// execAgentTime: log the proposed time entry as draft (flows through
// the §308 time-approval queue like any other entry).
func execAgentTime(ctx context.Context, tx pgx.Tx, p map[string]any) (string, error) {
	projID, _ := p["project_id"].(string)
	mins, _ := p["minutes"].(float64)
	note, _ := p["note"].(string)
	var id string
	err := tx.QueryRow(ctx, `
		INSERT INTO time_entries (workspace_id, project_id, user_id, started_at, ended_at, minutes, note, status)
		SELECT NULLIF(current_setting('app.workspace_id', true), '')::uuid, pr.id,
		       COALESCE(NULLIF(current_setting('app.user_id', true), '')::uuid, own.user_id), now() - ($2::int * interval '1 min'), now(), $2::int, NULLIF($3,''), 'draft'
		FROM projects pr
		CROSS JOIN LATERAL (SELECT m.user_id FROM memberships m
			WHERE m.workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
			  AND m.role = 'owner' ORDER BY m.created_at LIMIT 1) own
		WHERE pr.id = $1::uuid AND pr.deleted_at IS NULL
		RETURNING id::text`, projID, int(mins), note).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errors.New("project not found (deleted or wrong workspace) — approval left pending")
	}
	return id, err
}
