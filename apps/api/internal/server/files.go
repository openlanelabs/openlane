package server

// Project files v1 (issue #50, spec §9): metadata rows in Postgres behind
// RLS; bytes in VaultS3 behind presigned URLs. The API never proxies file
// bytes. Object keys are server-generated {ws}/{project}/{uuid} so a
// client file name can never reach storage paths (spec §17 traversal
// edge).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type fileOut struct {
	ID          string `json:"id"`
	ProjectID   string `json:"project_id"`
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
	Status      string `json:"status"`
	CustomerVis bool   `json:"customer_visible"`
	CreatedAt   string `json:"created_at"`
	UploadURL   string `json:"upload_url,omitempty"` // presigned PUT, only on create
	DownloadURL string `json:"url,omitempty"`        // fresh presigned GET, only on /url
}

var fileNameRe = regexp.MustCompile(`^[\w][\w .()\-']{0,254}$`)
var contentTypeAllow = map[string]bool{
	"application/pdf": true, "application/vnd.openxmlformats-officedocument.wordprocessingml.document": true,
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         true,
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": true,
	"text/csv": true, "text/plain": true, "text/markdown": true,
	"image/png": true, "image/jpeg": true, "application/zip": true,
}

const maxFileSize = 200 << 20 // spec: P0 cap 200MB (multipart+resume later)

// sanitizeFileName: strip path separators + control chars, collapse
// repeats of allowed set, cap at 255 (spec edge: `../../../etc/passwd`).
func sanitizeFileName(in string) string {
	in = strings.ReplaceAll(in, "\\", "/")
	if i := strings.LastIndex(in, "/"); i >= 0 {
		in = in[i+1:]
	}
	var b strings.Builder
	for _, r := range in {
		if r < 32 || r == 127 {
			continue
		}
		b.WriteRune(r)
	}
	name := strings.TrimSpace(b.String())
	if len(name) > 255 {
		name = name[len(name)-255:]
	}
	return name
}

// POST /v1/projects/{id}/files — create metadata + presigned PUT.
func (s *Server) createProjectFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name            string `json:"name"`
		ContentType     string `json:"content_type"`
		SizeBytes       int64  `json:"size_bytes"`
		CustomerVisible bool   `json:"customer_visible"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	name := sanitizeFileName(req.Name)
	if !fileNameRe.MatchString(name) {
		problem(w, http.StatusBadRequest, "name must be 1-255 chars, no path separators")
		return
	}
	if !contentTypeAllow[req.ContentType] {
		problem(w, http.StatusBadRequest, "content_type not allowed")
		return
	}
	if req.SizeBytes <= 0 || req.SizeBytes > maxFileSize {
		problem(w, http.StatusBadRequest, "size_bytes must be 1..209715200")
		return
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", workspaceFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	var id, objectKey string
	err = tx.QueryRow(ctx, `
		INSERT INTO files (workspace_id, project_id, name, object_key, content_type, size_bytes, customer_visible, created_by)
		SELECT $1::uuid, p.id, $2,
		       $1::text || '/' || p.id::text || '/' || gen_random_uuid()::text,
		       $3, $4, $5, NULLIF(current_setting('app.user_id', true), '')::uuid
		FROM projects p WHERE p.id = $6 AND p.deleted_at IS NULL
		RETURNING id, object_key`,
		workspaceFromCtx(ctx), name, req.ContentType, req.SizeBytes, req.CustomerVisible, r.PathValue("id")).
		Scan(&id, &objectKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "project not found")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	uploadURL, err := presignS3(s.s3, http.MethodPut, objectKey, 15*time.Minute, time.Now())
	if err != nil {
		problem(w, http.StatusInternalServerError, "storage unavailable")
		return
	}
	writeJSON(w, http.StatusCreated, fileOut{
		ID: id, ProjectID: r.PathValue("id"), Name: name,
		ContentType: req.ContentType, SizeBytes: req.SizeBytes,
		CustomerVis: req.CustomerVisible, Status: "pending",
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		UploadURL: uploadURL,
	})
}

// POST /v1/files/{id}/uploaded — client PUT completed; HEAD-verify.
func (s *Server) confirmFileUploaded(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", workspaceFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	var objectKey string
	var sizeBytes int64
	var status string
	err = tx.QueryRow(ctx, `
		UPDATE files SET status = 'uploaded', updated_at = now()
		WHERE id = $1 AND status = 'pending' AND deleted_at IS NULL
		RETURNING object_key, size_bytes, status`,
		r.PathValue("id")).Scan(&objectKey, &sizeBytes, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "file not found or not pending")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	// HEAD the object; refuse if missing or size mismatched.
	headURL, err := presignS3(s.s3, http.MethodHead, objectKey, time.Minute, time.Now())
	if err != nil {
		problem(w, http.StatusInternalServerError, "storage unavailable")
		return
	}
	hreq, _ := http.NewRequestWithContext(ctx, http.MethodHead, headURL, nil)
	hres, err := s.http.Do(hreq)
	if err != nil || hres.StatusCode != http.StatusOK {
		_, _ = tx.Exec(ctx, "UPDATE files SET status='failed', updated_at=now() WHERE id=$1", r.PathValue("id"))
		_ = tx.Commit(ctx)
		problem(w, http.StatusConflict, "object missing or HEAD failed — file marked failed")
		return
	}
	_ = hres.Body.Close()
	if cl := hres.ContentLength; cl >= 0 && cl != sizeBytes {
		_, _ = tx.Exec(ctx, "UPDATE files SET status='failed', updated_at=now() WHERE id=$1", r.PathValue("id"))
		_ = tx.Commit(ctx)
		problem(w, http.StatusConflict, "uploaded size does not match declared size")
		return
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'file', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'file.uploaded', 'api')`,
		r.PathValue("id")); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "uploaded"})
}

// GET /v1/projects/{id}/files — metadata list.
func (s *Server) listProjectFiles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", workspaceFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows, err := tx.Query(ctx, `
		SELECT f.id, f.project_id, f.name, f.content_type, f.size_bytes, f.status, f.customer_visible, f.created_at
		FROM files f
		WHERE f.project_id = $1 AND f.deleted_at IS NULL
		ORDER BY f.created_at DESC`, r.PathValue("id"))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []fileOut{}
	for rows.Next() {
		var f fileOut
		var created time.Time
		if err := rows.Scan(&f.ID, &f.ProjectID, &f.Name, &f.ContentType, &f.SizeBytes, &f.Status, &f.CustomerVis, &created); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		f.CreatedAt = created.UTC().Format(time.RFC3339)
		out = append(out, f)
	}
	writeJSON(w, http.StatusOK, out)
}

// DELETE /v1/files/{id} — soft delete row (object stays in versioned
// bucket for audit; a purge worker is out of scope for v1).
func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", workspaceFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	ct, err := tx.Exec(ctx, `
		UPDATE files SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, r.PathValue("id"))
	if err != nil || ct.RowsAffected() == 0 {
		problem(w, http.StatusNotFound, "file not found")
		return
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'file', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'file.deleted', 'api')`,
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

// GET /v1/files/{id}/url — fresh presigned GET (15 min).
func (s *Server) fileDownloadURL(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", workspaceFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	var objectKey, status string
	err = tx.QueryRow(ctx, `
		SELECT object_key, status FROM files
		WHERE id = $1 AND deleted_at IS NULL AND status = 'uploaded'`,
		r.PathValue("id")).Scan(&objectKey, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "file not found or not uploaded")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	url, err := presignS3(s.s3, http.MethodGet, objectKey, 15*time.Minute, time.Now())
	if err != nil {
		problem(w, http.StatusInternalServerError, "storage unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url, "expires_in": "900"})
}

// GET /v1/portal/{token}/files — portal ctx: customer_visible files of
// the link's project (RLS does the scoping; handler only sets ctx).
func (s *Server) listPortalFiles(ctx context.Context, tx pgx.Tx, w http.ResponseWriter, _ *http.Request) (int, error) {
	rows, err := tx.Query(ctx, `
		SELECT f.id, f.project_id, f.name, f.content_type, f.size_bytes, f.status, f.customer_visible, f.created_at
		FROM files f
		WHERE f.customer_visible AND f.deleted_at IS NULL
		ORDER BY f.created_at DESC`)
	if err != nil {
		return 0, fmt.Errorf("list portal files: %w", err)
	}
	defer rows.Close()
	out := []fileOut{}
	for rows.Next() {
		var f fileOut
		var created time.Time
		if err := rows.Scan(&f.ID, &f.ProjectID, &f.Name, &f.ContentType, &f.SizeBytes, &f.Status, &f.CustomerVis, &created); err != nil {
			return 0, fmt.Errorf("scan portal file: %w", err)
		}
		f.CreatedAt = created.UTC().Format(time.RFC3339)
		out = append(out, f)
	}
	writeJSON(w, http.StatusOK, out)
	return 0, nil
}

// GET /v1/portal/{token}/files/{file_id}/url — presigned GET for a file
// the portal RLS can see.
func (s *Server) portalFileURL(ctx context.Context, tx pgx.Tx, w http.ResponseWriter, r *http.Request) (int, error) {
	var objectKey string
	err := tx.QueryRow(ctx, `
		SELECT object_key FROM files
		WHERE id = $1 AND customer_visible AND status = 'uploaded' AND deleted_at IS NULL`,
		r.PathValue("file_id")).Scan(&objectKey)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "file not available")
			return 0, nil
		}
		return 0, fmt.Errorf("portal file url: %w", err)
	}
	url, err := presignS3(s.s3, http.MethodGet, objectKey, 15*time.Minute, time.Now())
	if err != nil {
		return 0, fmt.Errorf("presign: %w", err)
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url, "expires_in": "900"})
	return 0, nil
}
