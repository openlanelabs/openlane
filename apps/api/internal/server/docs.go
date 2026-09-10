package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

// docs.go — §9/§266 v1: versioned markdown. Create = doc row + version 1
// in one tx; edit = append a version row and bump latest_version (never
// overwrite — spec: version history). Portal reads customer_visible docs.

type docOut struct {
	ID            string `json:"id"`
	ProjectID     string `json:"project_id"`
	Title         string `json:"title"`
	CustomerVis   bool   `json:"customer_visible"`
	LatestVersion int    `json:"latest_version"`
	UpdatedAt     string `json:"updated_at"`
}

type docVersionOut struct {
	Version   int    `json:"version"`
	ContentMD string `json:"content_md"`
	CreatedBy string `json:"created_by,omitempty"`
	CreatedAt string `json:"created_at"`
}

const maxDocBytes = 200_000 // ponytail: 200KB per version; chunked storage when Yjs lands

func validateDoc(title string, contentLen int) string {
	if title == "" || len(title) > 255 {
		return "title must be 1-255 chars"
	}
	if contentLen > maxDocBytes {
		return "document too large"
	}
	return ""
}

// POST /v1/projects/{id}/docs — staff create (v1)
func (s *Server) createDoc(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title       string `json:"title"`
		ContentMD   string `json:"content_md"`
		CustomerVis bool   `json:"customer_visible"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 210<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if msg := validateDoc(req.Title, len(req.ContentMD)); msg != "" {
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

	// INSERT..SELECT via projects for the RLS 404-on-ghost-project shape
	var d docOut
	var created time.Time
	err := tx.QueryRow(ctx, `
		INSERT INTO docs (workspace_id, project_id, title, customer_visible, latest_version, created_by)
		SELECT p.workspace_id, p.id, $2, $3, 1, NULLIF(current_setting('app.user_id', true), '')::uuid
		FROM projects p
		WHERE p.id = $1::uuid AND p.deleted_at IS NULL
		RETURNING id, project_id, title, customer_visible, latest_version, created_at`,
		r.PathValue("id"), req.Title, req.CustomerVis).Scan(
		&d.ID, &d.ProjectID, &d.Title, &d.CustomerVis, &d.LatestVersion, &created)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "project not found")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO doc_versions (workspace_id, doc_id, version, content_md, created_by)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, 1, $2,
		        NULLIF(current_setting('app.user_id', true), '')::uuid)`,
		d.ID, req.ContentMD); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'doc', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'doc.created', 'api')`,
		d.ID); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	d.UpdatedAt = created.UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusCreated, d)
}

// GET /v1/projects/{id}/docs — staff list (latest version metadata)
func (s *Server) listProjectDocs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT d.id, d.project_id, d.title, d.customer_visible, d.latest_version, d.updated_at
		FROM docs d
		WHERE d.project_id = $1::uuid AND d.deleted_at IS NULL
		ORDER BY d.updated_at DESC`,
		r.PathValue("id"))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []docOut{}
	for rows.Next() {
		var d docOut
		var updated time.Time
		if err := rows.Scan(&d.ID, &d.ProjectID, &d.Title, &d.CustomerVis, &d.LatestVersion, &updated); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		d.UpdatedAt = updated.UTC().Format(time.RFC3339)
		out = append(out, d)
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /v1/docs/{id} — staff current content (latest version)
func (s *Server) getDoc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var d docOut
	var updated time.Time
	var content string
	err := tx.QueryRow(ctx, `
		SELECT d.id, d.project_id, d.title, d.customer_visible, d.latest_version, d.updated_at,
		       v.content_md
		FROM docs d
		JOIN LATERAL (
		    SELECT content_md FROM doc_versions
		    WHERE doc_id = d.id ORDER BY version DESC LIMIT 1
		) v ON true
		WHERE d.id = $1::uuid AND d.deleted_at IS NULL`,
		r.PathValue("id")).Scan(&d.ID, &d.ProjectID, &d.Title, &d.CustomerVis, &d.LatestVersion, &updated, &content)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "doc not found")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	d.UpdatedAt = updated.UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, map[string]any{
		"id": d.ID, "project_id": d.ProjectID, "title": d.Title,
		"customer_visible": d.CustomerVis, "latest_version": d.LatestVersion,
		"updated_at": d.UpdatedAt, "content_md": content,
	})
}

// PUT /v1/docs/{id} — staff edit: append version row + bump latest
func (s *Server) updateDoc(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title       *string `json:"title"`
		ContentMD   *string `json:"content_md"`
		CustomerVis *bool   `json:"customer_visible"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 210<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.ContentMD == nil && req.Title == nil && req.CustomerVis == nil {
		problem(w, http.StatusBadRequest, "nothing to update")
		return
	}
	ctx := r.Context()
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// one UPDATE: bump latest_version, flip flags; RLS scopes the row
	var d docOut
	var updated time.Time
	err := tx.QueryRow(ctx, `
		UPDATE docs SET
		    title = COALESCE($2, title),
		    customer_visible = COALESCE($3, customer_visible),
		    latest_version = latest_version + 1,
		    updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING id, project_id, title, customer_visible, latest_version, updated_at`,
		r.PathValue("id"), req.Title, req.CustomerVis).Scan(
		&d.ID, &d.ProjectID, &d.Title, &d.CustomerVis, &d.LatestVersion, &updated)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "doc not found")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	// append the version row: new content, or carry forward previous
	if _, err := tx.Exec(ctx, `
		INSERT INTO doc_versions (workspace_id, doc_id, version, content_md, created_by)
		SELECT workspace_id, $1, $2, COALESCE($3, (
		    SELECT content_md FROM doc_versions
		    WHERE doc_id = $1 ORDER BY version DESC LIMIT 1
		)), NULLIF(current_setting('app.user_id', true), '')::uuid
		FROM docs WHERE id = $1`,
		d.ID, d.LatestVersion, req.ContentMD); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, new_value, source)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'doc', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'doc.updated',
		        jsonb_build_object('version', $2::int), 'api')`,
		d.ID, d.LatestVersion); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	d.UpdatedAt = updated.UTC().Format(time.RFC3339)
	writeJSON(w, http.StatusOK, d)
}

// GET /v1/docs/{id}/versions — staff version history (content list)
func (s *Server) listDocVersions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT version, COALESCE(content_md, ''), COALESCE(created_by::text, ''), created_at
		FROM doc_versions
		WHERE doc_id = $1::uuid
		ORDER BY version DESC`,
		r.PathValue("id"))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []docVersionOut{}
	for rows.Next() {
		var v docVersionOut
		var created time.Time
		if err := rows.Scan(&v.Version, &v.ContentMD, &v.CreatedBy, &created); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		v.CreatedAt = created.UTC().Format(time.RFC3339)
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /v1/portal/{token}/docs — customer-visible docs (latest content)
func (s *Server) listPortalDocs(ctx context.Context, tx pgx.Tx, w http.ResponseWriter, _ *http.Request) (int, error) {
	rows, err := tx.Query(ctx, `
		SELECT d.id, d.title, d.updated_at, v.content_md
		FROM docs d
		JOIN LATERAL (
		    SELECT content_md FROM doc_versions
		    WHERE doc_id = d.id ORDER BY version DESC LIMIT 1
		) v ON true
		WHERE d.customer_visible AND d.deleted_at IS NULL
		ORDER BY d.updated_at DESC`)
	if err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	defer rows.Close()
	type portalDoc struct {
		ID        string `json:"id"`
		Title     string `json:"title"`
		ContentMD string `json:"content_md"`
		UpdatedAt string `json:"updated_at"`
	}
	out := []portalDoc{}
	for rows.Next() {
		var pd portalDoc
		var updated time.Time
		if err := rows.Scan(&pd.ID, &pd.Title, &updated, &pd.ContentMD); err != nil {
			return http.StatusInternalServerError, errQuiet
		}
		pd.UpdatedAt = updated.UTC().Format(time.RFC3339)
		out = append(out, pd)
	}
	writeJSON(w, http.StatusOK, out)
	return 0, nil
}
