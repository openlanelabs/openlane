package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/openlanelabs/openlane/apps/api/internal/importer"
)

// importResult mirrors the contract's ImportResult schema.
type importResult struct {
	DryRun    bool                `json:"dry_run"`
	Source    string              `json:"source"`
	ProjectID *string             `json:"project_id"`
	OKCount   int                 `json:"ok_count"`
	Errors    []importer.RowError `json:"errors"`
}

func (s *Server) createImport(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil { // ponytail: 32MB cap; streaming when imports get big
		problem(w, http.StatusBadRequest, "multipart form required")
		return
	}
	src := strings.ToLower(strings.TrimSpace(r.FormValue("source")))
	if src == "" {
		problem(w, http.StatusBadRequest, "source required (asana|rocketlane|generic)")
		return
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		problem(w, http.StatusBadRequest, "file required")
		return
	}
	defer func() { _ = file.Close() }()

	v, rows := importer.Parse(file, src)
	if rows == nil { // unknown source / unreadable file — reason in v.Errors
		problem(w, http.StatusBadRequest, v.Errors[0].Reason)
		return
	}

	projectID := strings.TrimSpace(r.FormValue("project_id"))
	createJSON := strings.TrimSpace(r.FormValue("create_project"))
	if projectID == "" && createJSON == "" {
		problem(w, http.StatusBadRequest, "project_id or create_project required")
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

	// Resolve target. Foreign-workspace project ids don't resolve under RLS
	// (404, no oracle). create_project inserts a draft first.
	target := projectID
	if target == "" {
		var cp struct {
			CustomerID string `json:"customer_id"`
			Name       string `json:"name"`
		}
		if err := json.Unmarshal([]byte(createJSON), &cp); err != nil || len(cp.Name) < 3 || len(cp.Name) > 120 || cp.CustomerID == "" {
			problem(w, http.StatusBadRequest, "create_project requires customer_id and name (3-120)")
			return
		}
		if err := tx.QueryRow(ctx, `
			INSERT INTO projects (workspace_id, customer_id, name, status)
			VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, $2, 'draft')
			RETURNING id`, cp.CustomerID, cp.Name).Scan(&target); err != nil {
			if strings.Contains(err.Error(), "violates") { // FK (no such customer) or RLS
				problem(w, http.StatusNotFound, "customer not found")
				return
			}
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
	} else {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1)`, target).Scan(&exists); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !exists {
			problem(w, http.StatusNotFound, "project not found")
			return
		}
	}

	if r.URL.Query().Get("dry_run") == "true" {
		res := importResult{DryRun: true, Source: src, ProjectID: &target, OKCount: v.OK, Errors: emptyIfNil(v.Errors)}
		writeJSON(w, http.StatusOK, res)
		return
	}

	// Commit path: one transaction; title-overlong rows quarantine, others abort.
	for _, row := range rows {
		var due any
		if row.DueAt != nil {
			due = *row.DueAt
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO tasks (workspace_id, project_id, title, owner_type, customer_visible, status, due_at, completed_at)
			VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, $2, 'internal', false, $3, $4,
			        CASE WHEN $3 = 'done' THEN now() END)`,
			target, row.Title, row.Status, due); err != nil {
			if strings.Contains(err.Error(), "255") {
				v.Errors = append(v.Errors, importer.RowError{SourceRow: row.SourceRow, Reason: "title exceeds 255 chars"})
				v.OK--
				continue
			}
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'project', $1, 'user', 'import.completed', 'import', $2)`,
		target, fmt.Sprintf(`{"ok":%d,"quarantined":%d}`, v.OK, len(v.Errors))); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	res := importResult{DryRun: false, Source: src, ProjectID: &target, OKCount: v.OK, Errors: emptyIfNil(v.Errors)}
	writeJSON(w, http.StatusOK, res)
}

func emptyIfNil(e []importer.RowError) []importer.RowError {
	if e == nil {
		return []importer.RowError{}
	}
	return e
}
