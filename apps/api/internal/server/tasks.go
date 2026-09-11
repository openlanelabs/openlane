package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// staffTaskOut mirrors the contract's StaffTask schema.
type staffTaskOut struct {
	ID            string  `json:"id"`
	ProjectID     string  `json:"project_id"`
	Title         string  `json:"title"`
	DescriptionMD *string `json:"description_md"`
	OwnerType     string  `json:"owner_type"`
	Status        string  `json:"status"`
	DueAt         *string `json:"due_at"`
	Required      bool    `json:"required"`
	CustomerVis   bool    `json:"customer_visible"`
	CompletedAt   *string `json:"completed_at"`
	CreatedAt     string  `json:"created_at"`
}

const staffTaskCols = `id, project_id, title, description_md, owner_type, status, due_at, required, customer_visible, completed_at, created_at`

func scanStaffTask(row pgx.Row) (staffTaskOut, error) {
	var t staffTaskOut
	var created time.Time
	var desc *string
	var dueT, completedT *time.Time
	err := row.Scan(&t.ID, &t.ProjectID, &t.Title, &desc, &t.OwnerType, &t.Status, &dueT, &t.Required, &t.CustomerVis, &completedT, &created)
	t.DescriptionMD = desc
	t.CreatedAt = created.UTC().Format(time.RFC3339)
	if dueT != nil {
		s := dueT.UTC().Format(time.RFC3339)
		t.DueAt = &s
	}
	if completedT != nil {
		s := completedT.UTC().Format(time.RFC3339)
		t.CompletedAt = &s
	}
	return t, err
}

var validStatuses = map[string]bool{
	"todo": true, "in_progress": true, "blocked": true, "review": true, "done": true, "waived": true,
}

// §8.1 state machine: todo→in_progress→review→done; blocked from any; done terminal
// unless reopened (with reason). waived is a terminal admin action.
func transitionOK(from, to string) bool {
	if from == to {
		return true
	}
	switch {
	case to == "blocked":
		return true
	case from == "todo":
		return to == "in_progress"
	case from == "in_progress":
		return to == "review" || to == "done" || to == "todo"
	case from == "review":
		return to == "done" || to == "in_progress"
	case from == "blocked":
		return to == "todo" || to == "in_progress"
	case from == "done":
		return false // reopen is a separate guarded path (reason required)
	case from == "waived":
		return false
	}
	return false
}

func (s *Server) listProjectTasks(w http.ResponseWriter, r *http.Request) {
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	ctx := r.Context()

	// project scope check (RLS): foreign/deleted → 404, no oracle
	var projExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM projects WHERE id = $1 AND deleted_at IS NULL)`, r.PathValue("id")).Scan(&projExists); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if !projExists {
		problem(w, http.StatusNotFound, "project not found")
		return
	}

	q := `SELECT ` + staffTaskCols + ` FROM tasks WHERE project_id = $1 AND deleted_at IS NULL`
	args := []any{r.PathValue("id")}
	if st := r.URL.Query().Get("status"); st != "" {
		args = append(args, st)
		q += ` AND status = $2`
	}
	q += ` ORDER BY created_at`
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []staffTaskOut{}
	for rows.Next() {
		var t staffTaskOut
		var created time.Time
		var desc *string
		var dueT, compT *time.Time
		if err := rows.Scan(&t.ID, &t.ProjectID, &t.Title, &desc, &t.OwnerType, &t.Status, &dueT, &t.Required, &t.CustomerVis, &compT, &created); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		t.DescriptionMD, t.CreatedAt = desc, created.UTC().Format(time.RFC3339)
		if dueT != nil {
			s := dueT.UTC().Format(time.RFC3339)
			t.DueAt = &s
		}
		if compT != nil {
			s := compT.UTC().Format(time.RFC3339)
			t.CompletedAt = &s
		}
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title         string  `json:"title"`
		DescriptionMD *string `json:"description_md"`
		OwnerType     string  `json:"owner_type"`
		Status        string  `json:"status"`
		DueAt         *string `json:"due_at"`
		Required      *bool   `json:"required"`
		CustomerVis   *bool   `json:"customer_visible"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if strings.TrimSpace(req.Title) == "" || len(req.Title) > 255 {
		problem(w, http.StatusBadRequest, "title required (1-255)")
		return
	}
	if req.OwnerType == "" {
		req.OwnerType = "internal"
	}
	switch req.OwnerType {
	case "internal", "customer", "partner", "agent":
	default:
		problem(w, http.StatusBadRequest, "invalid owner_type")
		return
	}
	if req.Status == "" {
		req.Status = "todo"
	}
	if !validStatuses[req.Status] {
		problem(w, http.StatusBadRequest, "invalid status")
		return
	}
	reqd := true
	if req.Required != nil {
		reqd = *req.Required
	}
	vis := false
	if req.CustomerVis != nil {
		vis = *req.CustomerVis
	}

	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	ctx := r.Context()

	t, err := scanStaffTask(tx.QueryRow(ctx, `
		INSERT INTO tasks (workspace_id, project_id, title, description_md, owner_type, status, required, customer_visible, due_at)
		SELECT NULLIF(current_setting('app.workspace_id', true), '')::uuid, p.id, $2, $3, $4, $5, $6, $7, $8::timestamptz
		FROM projects p
		WHERE p.id = $1 AND p.deleted_at IS NULL
		RETURNING `+staffTaskCols,
		r.PathValue("id"), req.Title, req.DescriptionMD, req.OwnerType, req.Status, reqd, vis, req.DueAt))
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
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'task', $1, 'user', NULLIF(current_setting('app.user_id', true), '')::uuid, 'task.created', 'api', $2)`,
		t.ID, `{"title":`+jsonString(t.Title)+`}`); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func (s *Server) patchTask(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title         *string `json:"title"`
		DescriptionMD *string `json:"description_md"`
		Status        *string `json:"status"`
		DueAt         *string `json:"due_at"`
		CustomerVis   *bool   `json:"customer_visible"`
		ReopenReason  *string `json:"reopen_reason"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.Title != nil && (strings.TrimSpace(*req.Title) == "" || len(*req.Title) > 255) {
		problem(w, http.StatusBadRequest, "title must be 1-255")
		return
	}

	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	ctx := r.Context()
	id := r.PathValue("id")

	var cur staffTaskOut
	cur, err := scanStaffTask(tx.QueryRow(ctx, `
		SELECT `+staffTaskCols+` FROM tasks WHERE id = $1 AND deleted_at IS NULL`, id))
	if err == pgx.ErrNoRows {
		problem(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	// §8.1 transition guard (reopen = done→todo with reason is the only exit from done)
	if req.Status != nil && *req.Status != cur.Status {
		to := *req.Status
		if !validStatuses[to] {
			problem(w, http.StatusBadRequest, "invalid status")
			return
		}
		if cur.Status == "done" && to == "todo" {
			if req.ReopenReason == nil || strings.TrimSpace(*req.ReopenReason) == "" {
				problem(w, http.StatusBadRequest, "reopening a done task requires reopen_reason (§8.1)")
				return
			}
		} else if !transitionOK(cur.Status, to) {
			problem(w, http.StatusBadRequest, "invalid transition "+cur.Status+"→"+to+" (§8.1 state machine)")
			return
		}
	}

	statusVal := req.Status

	t, err := scanStaffTask(tx.QueryRow(ctx, `
		UPDATE tasks SET
		  title = COALESCE($2, title),
		  description_md = COALESCE($3, description_md),
		  status = COALESCE($4, status),
		  customer_visible = COALESCE($5, customer_visible),
		  due_at = COALESCE($6::timestamptz, due_at),
		  completed_at = CASE WHEN $4 = 'done' AND completed_at IS NULL THEN now()
		                      WHEN $4 = 'todo' THEN NULL
		                      ELSE completed_at END,
		  updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL
		RETURNING `+staffTaskCols,
		id, req.Title, req.DescriptionMD, statusVal, req.CustomerVis, req.DueAt))
	if err == pgx.ErrNoRows {
		problem(w, http.StatusNotFound, "task not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	// audit with reason on reopen
	auditAction := "task.updated"
	newVal := `{"status":"` + t.Status + `"}`
	if req.Status != nil && cur.Status == "done" && *req.Status == "todo" {
		auditAction = "task.reopened"
		newVal = `{"reason":` + jsonString(*req.ReopenReason) + `}`
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'task', $1, 'user', NULLIF(current_setting('app.user_id', true), '')::uuid, $2, 'api', $3)`,
		id, auditAction, newVal); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if t.Status == "done" && cur.Status != "done" {
		s.notifyEvent(ctx, workspaceFromCtx(ctx), "task.completed",
			"✅ Task completed: "+t.Title, t.ID)
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *Server) deleteTask(w http.ResponseWriter, r *http.Request) {
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	ctx := r.Context()
	tag, err := tx.Exec(ctx, `UPDATE tasks SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, r.PathValue("id"))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if tag.RowsAffected() == 0 {
		problem(w, http.StatusNotFound, "task not found")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'task', $1, 'user', NULLIF(current_setting('app.user_id', true), '')::uuid, 'task.deleted', 'api')`,
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
