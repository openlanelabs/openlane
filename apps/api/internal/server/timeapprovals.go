package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// Timesheet approvals (P1 §307): draft→submitted→approved/rejected,
// resubmit rejected. Approval chain owner→manager: approver must be
// manager-or-above and cannot approve their own entry.

func roleFromCtx(ctx context.Context) string {
	role, _ := ctx.Value(roleKey{}).(string)
	return role
}

func (s *Server) isManager(ctx context.Context) bool {
	switch roleFromCtx(ctx) {
	case "owner", "admin", "manager":
		return true
	}
	return false
}

// decideTime: shared submit/approve/reject engine. newStatus is the
// destination; reject requires reason.
func (s *Server) decideTime(w http.ResponseWriter, r *http.Request, newStatus string) {
	var req struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<10)).Decode(&req) // body optional

	entryID := r.PathValue("id")
	ctx := r.Context()
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

	var cur, authorID string
	err = tx.QueryRow(ctx, `SELECT status, user_id::text FROM time_entries WHERE id = $1::uuid AND deleted_at IS NULL`,
		entryID).Scan(&cur, &authorID)
	if errors.Is(err, pgx.ErrNoRows) {
		problem(w, http.StatusNotFound, "time entry not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	me := userFromCtx(ctx)
	switch newStatus {
	case "submitted":
		// draft→submitted (own entry), rejected→submitted (resubmit)
		// author check first: non-authors get no state information
		if me != authorID {
			problem(w, http.StatusForbidden, "only the author can submit their entry")
			return
		}
		if cur != "draft" && cur != "rejected" {
			problem(w, http.StatusBadRequest, "only draft or rejected entries can be submitted")
			return
		}
	case "approved", "rejected":
		if cur != "submitted" {
			problem(w, http.StatusBadRequest, "only submitted entries can be "+newStatus)
			return
		}
		if !s.isManager(ctx) {
			problem(w, http.StatusForbidden, "manager role required")
			return
		}
		if me == authorID {
			problem(w, http.StatusForbidden, "cannot approve your own time")
			return
		}
		if newStatus == "rejected" {
			if len(req.Reason) < 1 || len(req.Reason) > 500 {
				problem(w, http.StatusBadRequest, "reason is required (1-500 chars)")
				return
			}
		}
	default:
		problem(w, http.StatusBadRequest, "invalid status")
		return
	}

	if _, err := tx.Exec(ctx, `
		UPDATE time_entries SET status = $2,
		  reject_reason = CASE WHEN $2 = 'rejected' THEN $3 ELSE NULL END,
		  updated_at = now()
		WHERE id = $1::uuid`, entryID, newStatus, req.Reason); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	newValue, _ := json.Marshal(map[string]string{"status": newStatus, "reason": req.Reason})
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'time_entry', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, $2, 'api', $3)`,
		entryID, "time."+newStatus, newValue); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) submitTime(w http.ResponseWriter, r *http.Request)  { s.decideTime(w, r, "submitted") }
func (s *Server) approveTime(w http.ResponseWriter, r *http.Request) { s.decideTime(w, r, "approved") }
func (s *Server) rejectTime(w http.ResponseWriter, r *http.Request)  { s.decideTime(w, r, "rejected") }

// listPendingTime: GET /v1/time/pending — manager queue for this workspace.
func (s *Server) listPendingTime(w http.ResponseWriter, r *http.Request) {
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
		SELECT te.id, te.project_id::text, te.task_id::text, te.user_id::text, te.minutes,
		       te.started_at, te.ended_at, COALESCE(te.note, ''), te.status
		FROM time_entries te
		WHERE te.workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
		  AND te.status = 'submitted' AND te.deleted_at IS NULL
		ORDER BY te.started_at ASC LIMIT 200`)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	type pending struct {
		ID        string  `json:"id"`
		ProjectID string  `json:"project_id"`
		TaskID    *string `json:"task_id"`
		UserID    string  `json:"user_id"`
		Minutes   int     `json:"minutes"`
		StartedAt string  `json:"started_at"`
		EndedAt   string  `json:"ended_at"`
		Note      string  `json:"note"`
		Status    string  `json:"status"`
	}
	out := []pending{}
	for rows.Next() {
		var p pending
		var started, ended time.Time
		if err := rows.Scan(&p.ID, &p.ProjectID, &p.TaskID, &p.UserID, &p.Minutes,
			&started, &ended, &p.Note, &p.Status); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		p.StartedAt = started.UTC().Format(time.RFC3339)
		p.EndedAt = ended.UTC().Format(time.RFC3339)
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}
