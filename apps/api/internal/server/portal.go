// Portal slice handlers (issue #22). Contracts: packages/contracts/openapi.yaml.
//
// Authorization is inverted from most CRUD apps: handlers do NOT check
// permissions. They set the RLS scope (workspace or token hash) and issue
// plain SQL; Postgres row-level security is the authorization layer
// (ADR-0002). A buggy handler can at worst mangle a response, never leak
// a cross-tenant row.
package server

import (
	"context"
	"crypto/rand"

	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// errQuiet means "response already written or plain 4xx" — see portal().
var errQuiet = errors.New("quiet")

// ---- DTOs ----

type portalLinkOut struct {
	ID        string     `json:"id"`
	ProjectID string     `json:"project_id"`
	ContactID string     `json:"contact_id"`
	Status    string     `json:"status"`
	ExpiresAt time.Time  `json:"expires_at"`
	LastUsed  *time.Time `json:"last_used_at"`
	CreatedAt time.Time  `json:"created_at"`
}

type portalTaskOut struct {
	ID              string     `json:"id"`
	ProjectID       string     `json:"project_id"`
	Title           string     `json:"title"`
	Status          string     `json:"status"`
	CustomerVisible bool       `json:"customer_visible"`
	DueAt           *time.Time `json:"due_at"`
	CompletedAt     *time.Time `json:"completed_at"`
}

type portalSessionOut struct {
	ProjectID    string    `json:"project_id"`
	ProjectName  string    `json:"project_name"`
	CustomerName string    `json:"customer_name"`
	ContactID    string    `json:"contact_id"`
	ContactName  string    `json:"contact_name"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func scanLink(scan func(...any) error) (portalLinkOut, error) {
	var l portalLinkOut
	var last pgtype.Timestamptz
	if err := scan(&l.ID, &l.ProjectID, &l.ContactID, &l.Status, &l.ExpiresAt, &last, &l.CreatedAt); err != nil {
		return l, err
	}
	if last.Valid {
		t := last.Time
		l.LastUsed = &t
	}
	return l, nil
}

func scanTask(scan func(...any) error) (portalTaskOut, error) {
	var t portalTaskOut
	var due, done pgtype.Timestamptz
	if err := scan(&t.ID, &t.ProjectID, &t.Title, &t.Status, &t.CustomerVisible, &due, &done); err != nil {
		return t, err
	}
	if due.Valid {
		tv := due.Time
		t.DueAt = &tv
	}
	if done.Valid {
		tv := done.Time
		t.CompletedAt = &tv
	}
	return t, nil
}

// ---- staff endpoints ----

func (s *Server) createPortalLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID     string `json:"project_id"`
		ContactID     string `json:"contact_id"`
		ExpiresInDays *int   `json:"expires_in_days"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.ProjectID == "" || req.ContactID == "" {
		problem(w, http.StatusBadRequest, "project_id and contact_id are required")
		return
	}
	days := 7
	if req.ExpiresInDays != nil {
		if *req.ExpiresInDays < 1 || *req.ExpiresInDays > 30 {
			problem(w, http.StatusBadRequest, "expires_in_days must be 1-30")
			return
		}
		days = *req.ExpiresInDays
	}

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

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	link, err := scanLink(func(dest ...any) error {
		return tx.QueryRow(ctx, `
			INSERT INTO portal_links (workspace_id, project_id, contact_id, token_hash, expires_at, created_by)
			SELECT p.workspace_id, p.id, $2, $3, now() + make_interval(days => $4), NULL
			FROM projects p
			WHERE p.id = $1 AND p.deleted_at IS NULL
			RETURNING id, project_id, contact_id, status, expires_at, last_used_at, created_at`,
			req.ProjectID, req.ContactID, hashToken(token), days).Scan(dest...)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		problem(w, http.StatusNotFound, "project not found in this workspace")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, struct {
		portalLinkOut
		Token string `json:"token"`
	}{link, token})
}

func (s *Server) listPortalLinks(w http.ResponseWriter, r *http.Request) {
	projectID := r.URL.Query().Get("project_id")
	if projectID == "" {
		problem(w, http.StatusBadRequest, "project_id query param required")
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
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true)",
		workspaceFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows, err := tx.Query(ctx, `
		SELECT id, project_id, contact_id, status, expires_at, last_used_at, created_at
		FROM portal_links
		WHERE project_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC`, projectID)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	links := []portalLinkOut{}
	for rows.Next() {
		l, err := scanLink(rows.Scan)
		if err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		links = append(links, l)
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, links)
}

func (s *Server) revokePortalLink(w http.ResponseWriter, r *http.Request) {
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
	tag, err := tx.Exec(ctx, `
		UPDATE portal_links SET status = 'revoked', updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL AND status = 'active'`, r.PathValue("id"))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if tag.RowsAffected() == 0 {
		problem(w, http.StatusNotFound, "portal link not found in this workspace")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- portal endpoints ----
//
// portal() (server.go) has already: hashed the token, opened the tx, and
// set app.portal_token_hash (+ cleared workspace scope). Below, plain SQL.

func (s *Server) getPortalSession(ctx context.Context, tx pgx.Tx, w http.ResponseWriter, r *http.Request) (int, error) {
	out := sessionFromCtx(ctx)
	th := hashToken(r.PathValue("token"))
	if _, err := tx.Exec(ctx, `UPDATE portal_links SET last_used_at = now() WHERE token_hash = $1`, th); err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	if err := tx.Commit(ctx); err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	writeJSON(w, http.StatusOK, out)
	return 0, nil
}

func (s *Server) listPortalTasks(ctx context.Context, tx pgx.Tx, w http.ResponseWriter, r *http.Request) (int, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, project_id, title, status, customer_visible, due_at, completed_at
		FROM tasks
		ORDER BY due_at NULLS LAST, created_at`)
	if err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	defer rows.Close()
	tasks := []portalTaskOut{}
	for rows.Next() {
		t, err := scanTask(rows.Scan)
		if err != nil {
			return http.StatusInternalServerError, errQuiet
		}
		tasks = append(tasks, t)
	}
	if err := tx.Commit(ctx); err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	writeJSON(w, http.StatusOK, tasks)
	return 0, nil
}

func (s *Server) completePortalTask(ctx context.Context, tx pgx.Tx, w http.ResponseWriter, r *http.Request) (int, error) {
	contactID := sessionFromCtx(ctx).ContactID

	taskID := r.PathValue("task_id")
	t, err := scanTask(func(dest ...any) error {
		return tx.QueryRow(ctx, `
			UPDATE tasks SET status = 'done', completed_at = now()
			WHERE id = $1 AND customer_visible AND deleted_at IS NULL AND status <> 'done'
			RETURNING id, project_id, title, status, customer_visible, due_at, completed_at`,
			taskID).Scan(dest...)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		// Idempotent re-complete within scope → 200; everything else → 404 (no oracle).
		// Fetch BEFORE commit — the tx (and its RLS scope) is closed after.
		t2, err2 := scanTask(func(dest ...any) error {
			return tx.QueryRow(ctx, `
				SELECT id, project_id, title, status, customer_visible, due_at, completed_at
				FROM tasks WHERE id = $1 AND status = 'done' AND customer_visible AND deleted_at IS NULL`,
				taskID).Scan(dest...)
		})
		if errors.Is(err2, pgx.ErrNoRows) {
			return http.StatusNotFound, errQuiet
		}
		if err2 != nil {
			return http.StatusInternalServerError, errQuiet
		}
		if err3 := tx.Commit(ctx); err3 != nil {
			return http.StatusInternalServerError, errQuiet
		}
		writeJSON(w, http.StatusOK, t2)
		return 0, nil
	}
	if err != nil {
		return http.StatusInternalServerError, errQuiet
	}

	// append-only audit trail; RLS INSERT-only policy vets the workspace
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, new_value, source)
		SELECT t.workspace_id, 'task', t.id, 'contact', $2, 'task.completed', jsonb_build_object('status', 'done'), 'portal'
		FROM tasks t WHERE t.id = $1`, taskID, contactID); err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	if err := tx.Commit(ctx); err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	writeJSON(w, http.StatusOK, t)
	return 0, nil
}
