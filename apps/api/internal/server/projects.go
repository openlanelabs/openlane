package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// projectOut mirrors the contract's Project schema.
type projectOut struct {
	ID           string  `json:"id"`
	CustomerID   string  `json:"customer_id"`
	Name         string  `json:"name"`
	Status       string  `json:"status"`
	Health       string  `json:"health"`
	StartDate    *string `json:"start_date"`
	TargetGoLive *string `json:"target_go_live"`
	ProgressPct  int     `json:"progress_pct"`
	CreatedAt    string  `json:"created_at"`
}

const projectCols = `p.id, p.customer_id, p.name, p.status, p.health, p.start_date::text, p.target_go_live::text, p.created_at`
const projectInsCols = `id, customer_id, name, status, health, start_date::text, target_go_live::text, created_at`

func scanProject(row pgx.Row) (projectOut, error) {
	var p projectOut
	var created time.Time
	err := row.Scan(&p.ID, &p.CustomerID, &p.Name, &p.Status, &p.Health, &p.StartDate, &p.TargetGoLive, &created)
	p.CreatedAt = created.UTC().Format(time.RFC3339)
	return p, err
}

// progressSub computes done+waived / total for the project row.
const progressSub = `(
  SELECT CASE WHEN count(*) = 0 THEN 0
         ELSE round(100.0 * count(*) FILTER (WHERE t.status IN ('done','waived')) / count(*)) END
  FROM tasks t WHERE t.project_id = p.id AND t.deleted_at IS NULL) AS progress_pct`

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CustomerID   string  `json:"customer_id"`
		Name         string  `json:"name"`
		Status       string  `json:"status"`
		StartDate    *string `json:"start_date"`
		TargetGoLive *string `json:"target_go_live"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.Name == "" || len(req.Name) < 3 || len(req.Name) > 120 {
		problem(w, http.StatusBadRequest, "name must be 3-120 chars")
		return
	}
	if req.Status == "" {
		req.Status = "draft"
	}
	if req.Status != "draft" && req.Status != "active" {
		problem(w, http.StatusBadRequest, "status must be draft or active on create")
		return
	}
	if req.StartDate != nil && req.TargetGoLive != nil && *req.StartDate > *req.TargetGoLive {
		problem(w, http.StatusBadRequest, "start_date must be on or before target_go_live")
		return
	}
	if req.Status == "active" && req.StartDate == nil {
		problem(w, http.StatusBadRequest, "draft→active requires start_date (§6.3)")
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

	var p projectOut
	p, err = scanProject(tx.QueryRow(ctx, `
		INSERT INTO projects (workspace_id, customer_id, name, status, start_date, target_go_live)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, NULLIF($1,'')::uuid, $2, $3, $4::date, $5::date)
		RETURNING `+projectInsCols, req.CustomerID, req.Name, req.Status, req.StartDate, req.TargetGoLive))
	if err != nil {
		if strings.Contains(err.Error(), "violates") {
			problem(w, http.StatusNotFound, "customer not found")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'project', $1, 'user', NULLIF(current_setting('app.user_id', true), '')::uuid, 'project.created', 'api', $2)`,
		p.ID, `{"name":"`+req.Name+`"}`); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.notifyEvent(ctx, workspaceFromCtx(ctx), "project.created",
		"🚀 Project created: "+p.Name)
	writeJSON(w, http.StatusCreated, p)
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
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
	q := `SELECT ` + projectCols + ` FROM projects p WHERE p.deleted_at IS NULL`
	args := []any{}
	if st := r.URL.Query().Get("status"); st != "" {
		args = append(args, st)
		q += ` AND p.status = $` + itoa(len(args))
	}
	if cid := r.URL.Query().Get("customer_id"); cid != "" {
		args = append(args, cid)
		q += ` AND p.customer_id = $` + itoa(len(args))
	}
	q += ` ORDER BY p.created_at DESC LIMIT 500`
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []projectOut{}
	for rows.Next() {
		var p projectOut
		var created time.Time
		if err := rows.Scan(&p.ID, &p.CustomerID, &p.Name, &p.Status, &p.Health, &p.StartDate, &p.TargetGoLive, &created); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		p.CreatedAt = created.UTC().Format(time.RFC3339)
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
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
	p, err := scanProject(tx.QueryRow(ctx, `
		SELECT `+projectCols+` FROM projects p
		WHERE p.id = $1 AND p.deleted_at IS NULL`, r.PathValue("id")))
	if err == pgx.ErrNoRows {
		problem(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	var pct int
	if err := tx.QueryRow(ctx, `SELECT `+progressSub+` FROM projects p WHERE p.id = $1`, r.PathValue("id")).Scan(&pct); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	p.ProgressPct = pct
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) patchProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name         *string `json:"name"`
		Status       *string `json:"status"`
		StartDate    *string `json:"start_date"`
		TargetGoLive *string `json:"target_go_live"`
		Health       *string `json:"health"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.Name != nil && (len(*req.Name) < 3 || len(*req.Name) > 120) {
		problem(w, http.StatusBadRequest, "name must be 3-120 chars")
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
	id := r.PathValue("id")
	var cur projectOut
	cur, err = scanProject(tx.QueryRow(ctx, `
		SELECT `+projectCols+` FROM projects p WHERE p.id = $1 AND p.deleted_at IS NULL`, id))
	if err == pgx.ErrNoRows {
		problem(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	// §6.3 transition guards
	if req.Status != nil && *req.Status != cur.Status {
		switch *req.Status {
		case "active":
			if cur.Status != "draft" {
				problem(w, http.StatusBadRequest, "only draft→active is allowed to active")
				return
			}
			sd := req.StartDate
			if sd == nil && cur.StartDate != nil {
				sd = cur.StartDate
			}
			if sd == nil {
				problem(w, http.StatusBadRequest, "draft→active requires start_date (§6.3)")
				return
			}
			var openRequired int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE project_id = $1 AND deleted_at IS NULL`, id).Scan(&openRequired); err != nil {
				problem(w, http.StatusInternalServerError, "internal error")
				return
			}
			if openRequired == 0 {
				problem(w, http.StatusBadRequest, "draft→active needs ≥1 task (§6.3)")
				return
			}
		case "completed":
			if cur.Status != "active" {
				problem(w, http.StatusBadRequest, "only active→completed is allowed")
				return
			}
			var openReq int
			if err := tx.QueryRow(ctx, `
				SELECT count(*) FROM tasks
				WHERE project_id = $1 AND deleted_at IS NULL AND required AND status NOT IN ('done','waived')`, id).Scan(&openReq); err != nil {
				problem(w, http.StatusInternalServerError, "internal error")
				return
			}
			if openReq > 0 {
				problem(w, http.StatusBadRequest, "active→completed needs all required tasks done or waived (§6.3)")
				return
			}
		case "draft", "on_hold", "at_risk", "delayed", "cancelled":
			// allowed transitions at P0
		default:
			problem(w, http.StatusBadRequest, "invalid status")
			return
		}
	}

	p, err := scanProject(tx.QueryRow(ctx, `
		UPDATE projects p SET
		  name = COALESCE($2, p.name),
		  status = COALESCE($3, p.status),
		  health = COALESCE($4, p.health),
		  start_date = COALESCE($5::date, p.start_date),
		  target_go_live = COALESCE($6::date, p.target_go_live),
		  updated_at = now()
		WHERE p.id = $1 AND p.deleted_at IS NULL
		RETURNING id, customer_id, name, status, health, start_date::text, target_go_live::text, created_at`,
		id, req.Name, req.Status, req.Health, req.StartDate, req.TargetGoLive))
	if err == pgx.ErrNoRows {
		problem(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'project', $1, 'user', NULLIF(current_setting('app.user_id', true), '')::uuid, 'project.updated', 'api', $2)`,
		id, `{"status":"`+p.Status+`"}`); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) deleteProject(w http.ResponseWriter, r *http.Request) {
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
	id := r.PathValue("id")
	tag, err := tx.Exec(ctx, `UPDATE projects SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if tag.RowsAffected() == 0 {
		problem(w, http.StatusNotFound, "project not found")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'project', $1, 'user', NULLIF(current_setting('app.user_id', true), '')::uuid, 'project.archived', 'api')`, id); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
