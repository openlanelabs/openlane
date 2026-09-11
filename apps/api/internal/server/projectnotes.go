package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Timestamped project notes (P1 §590, §516.8) — append-only feed with
// author + created_at per row. The competitor complaint was notes that
// don't show who/when; these do by construction.

// createProjectNote: POST /v1/projects/{id}/notes
func (s *Server) createProjectNote(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	req.Body = strings.TrimSpace(req.Body)
	if len(req.Body) < 1 || len(req.Body) > 2000 {
		problem(w, http.StatusBadRequest, "body must be 1-2000 chars")
		return
	}
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
	var n projectNoteOut
	var created time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO project_notes (workspace_id, project_id, author_user_id, body)
		SELECT NULLIF(current_setting('app.workspace_id', true), '')::uuid, p.id,
		       NULLIF(current_setting('app.user_id', true), '')::uuid, $2
		FROM projects p
		WHERE p.id = $1::uuid AND p.deleted_at IS NULL
		RETURNING id, project_id, COALESCE(author_user_id::text,''), body, created_at`,
		r.PathValue("id"), req.Body).Scan(&n.ID, &n.ProjectID, &n.Author, &n.Body, &created)
	n.CreatedAt = created.UTC().Format(time.RFC3339)
	if errors.Is(err, pgx.ErrNoRows) {
		problem(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'project_note', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'note.created', 'api', $2)`,
		n.ID, mustJSON(map[string]any{"project_id": n.ProjectID, "chars": len(n.Body)})); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, n)
}

// listProjectNotes: GET /v1/projects/{id}/notes — newest first.
func (s *Server) listProjectNotes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true)",
		workspaceFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows, err := tx.Query(ctx, `
		SELECT n.id::text, n.project_id::text, COALESCE(u.display_name, 'Unknown'), n.body, n.created_at
		FROM project_notes n
		LEFT JOIN users u ON u.id = n.author_user_id
		JOIN projects p ON p.id = n.project_id
		WHERE n.project_id = $1::uuid AND p.deleted_at IS NULL
		ORDER BY n.created_at DESC
		LIMIT 200`,
		r.PathValue("id"))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []projectNoteOut{}
	for rows.Next() {
		var n projectNoteOut
		var created time.Time
		if err := rows.Scan(&n.ID, &n.ProjectID, &n.Author, &n.Body, &created); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		n.CreatedAt = created.UTC().Format(time.RFC3339)
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

type projectNoteOut struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}
