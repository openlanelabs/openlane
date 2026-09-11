package server

// Portal approvals v1 (issue #58, spec §7.4): staff request, customer
// Approve / Request-changes with a comment. The approval is a first-class
// row; the "milestone" it approves is a free-form title until milestones
// land in P1.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

type approvalOut struct {
	ID              string  `json:"id"`
	ProjectID       string  `json:"project_id"`
	Title           string  `json:"title"`
	Description     string  `json:"description,omitempty"`
	Status          string  `json:"status"`
	DecisionComment string  `json:"decision_comment,omitempty"`
	DecidedAt       *string `json:"decided_at,omitempty"`
	CreatedAt       string  `json:"created_at"`
}

// ---- staff ----

func (s *Server) createApproval(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if len(req.Title) < 1 || len(req.Title) > 200 {
		problem(w, http.StatusBadRequest, "title required (max 200 chars)")
		return
	}

	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	ctx := r.Context()

	var id string
	var created time.Time
	// project visibility via RLS: other workspaces' project ids 404
	err := tx.QueryRow(ctx, `
		INSERT INTO approvals (workspace_id, project_id, title, description, requested_by)
		SELECT NULLIF(current_setting('app.workspace_id', true), '')::uuid, p.id, $1, NULLIF($2,''),
		       NULLIF(current_setting('app.user_id', true), '')::uuid
		FROM projects p
		WHERE p.id = $3::uuid AND p.deleted_at IS NULL
		RETURNING id, created_at`,
		req.Title, req.Description, r.PathValue("id")).Scan(&id, &created)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "project not found")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'approval', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'approval.created', 'api')`,
		id); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.notifyEvent(ctx, workspaceFromCtx(ctx), "approval.requested",
		"🔔 Approval requested: "+req.Title, id)
	writeJSON(w, http.StatusCreated, approvalOut{
		ID: id, ProjectID: r.PathValue("id"), Title: req.Title,
		Description: req.Description, Status: "pending",
		CreatedAt: created.UTC().Format(time.RFC3339),
	})
}

func (s *Server) listProjectApprovals(w http.ResponseWriter, r *http.Request) {
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	ctx := r.Context()

	rows, err := tx.Query(ctx, `
		SELECT a.id, a.project_id, a.title, COALESCE(a.description,''), a.status,
		       COALESCE(a.decision_comment,''), a.decided_at, a.created_at
		FROM approvals a
		JOIN projects p ON p.id = a.project_id
		WHERE a.deleted_at IS NULL AND p.id = $1::uuid
		ORDER BY a.created_at DESC`, r.PathValue("id"))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []approvalOut{}
	for rows.Next() {
		var a approvalOut
		var decided *string
		var created time.Time
		if err := rows.Scan(&a.ID, &a.ProjectID, &a.Title, &a.Description, &a.Status,
			&a.DecisionComment, &decided, &created); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		a.DecidedAt = decided
		a.CreatedAt = created.UTC().Format(time.RFC3339)
		out = append(out, a)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) reopenApproval(w http.ResponseWriter, r *http.Request) {
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	ctx := r.Context()

	tag, err := tx.Exec(ctx, `
		UPDATE approvals SET status = 'pending', decided_by = NULL,
		       decision_comment = NULL, decided_at = NULL
		WHERE id = $1::uuid AND deleted_at IS NULL
		  AND status IN ('approved','changes_requested')`, r.PathValue("id"))
	if err != nil || tag.RowsAffected() == 0 {
		problem(w, http.StatusNotFound, "approval not found or not decided")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'approval', $1::uuid, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'approval.reopened', 'api')`,
		r.PathValue("id")); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- portal ----

func (s *Server) listPortalApprovals(ctx context.Context, tx pgx.Tx, w http.ResponseWriter, _ *http.Request) (int, error) {
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.project_id, a.title, COALESCE(a.description,'')
		FROM approvals a
		WHERE a.deleted_at IS NULL AND a.status = 'pending'
		ORDER BY a.created_at ASC`)
	if err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	defer rows.Close()
	out := []map[string]string{}
	for rows.Next() {
		var id, projectID, title, description string
		if err := rows.Scan(&id, &projectID, &title, &description); err != nil {
			return http.StatusInternalServerError, errQuiet
		}
		out = append(out, map[string]string{
			"id": id, "project_id": projectID, "title": title, "description": description,
		})
	}
	writeJSON(w, http.StatusOK, out)
	return 0, nil
}

func (s *Server) decidePortalApproval(ctx context.Context, tx pgx.Tx, w http.ResponseWriter, r *http.Request) (int, error) {
	var req struct {
		Decision string `json:"decision"`
		Comment  string `json:"comment"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return 0, nil
	}
	if req.Decision != "approved" && req.Decision != "changes_requested" {
		problem(w, http.StatusBadRequest, `decision must be "approved" or "changes_requested"`)
		return 0, nil
	}
	sess := sessionFromCtx(ctx)

	// pending-only, scoped by RLS to the link's projects; pk-probes of
	// decided or foreign approvals both look like 404 (no oracle)
	var id string
	err := tx.QueryRow(ctx, `
		UPDATE approvals SET status = $2, decided_by = $3::uuid,
		       decision_comment = NULLIF($4,''), decided_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL AND status = 'pending'
		RETURNING id`,
		r.PathValue("id"), req.Decision, sess.ContactID, req.Comment).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "approval not found")
			return 0, nil
		}
		return http.StatusInternalServerError, errQuiet
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		SELECT a.workspace_id, 'approval', a.id, 'contact', $2, 'approval.decided', 'portal'
		FROM approvals a WHERE a.id = $1`, id, sess.ContactID); err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	if err := tx.Commit(ctx); err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	verdict := "changes requested"
	if req.Decision == "approved" {
		verdict = "approved"
	}
	s.notifyEvent(ctx, sess.WorkspaceID, "approval.decided",
		"✍️ Customer "+verdict+": "+sess.ProjectName, id)
	writeJSON(w, http.StatusOK, map[string]string{"status": req.Decision, "id": id})
	return 0, nil
}
