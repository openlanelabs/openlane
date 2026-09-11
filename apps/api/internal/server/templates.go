package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type templateTask struct {
	Title         string `json:"title"`
	OwnerRole     string `json:"owner_role,omitempty"`
	DueOffsetDays *int   `json:"due_offset_days,omitempty"`
	Required      *bool  `json:"required,omitempty"`
	CustomerVis   *bool  `json:"customer_visible,omitempty"`
}

type templatePhase struct {
	Name  string         `json:"name"`
	Tasks []templateTask `json:"tasks"`
}

type templateOut struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Version     string          `json:"version"`
	Category    string          `json:"category"`
	Description string          `json:"description,omitempty"`
	Phases      json.RawMessage `json:"phases"`
	IsActive    bool            `json:"is_active"`
	UsageCount  int             `json:"usage_count"`
	CreatedAt   string          `json:"created_at"`
}

const templateCols = `id, name, version, category, COALESCE(description,''), phases, is_active, usage_count, created_at`

func scanTemplate(row pgx.Row) (templateOut, error) {
	var t templateOut
	var created time.Time
	err := row.Scan(&t.ID, &t.Name, &t.Version, &t.Category, &t.Description, &t.Phases, &t.IsActive, &t.UsageCount, &created)
	t.CreatedAt = created.UTC().Format(time.RFC3339)
	return t, err
}

func (s *Server) staffTx(r *http.Request) (pgx.Tx, bool) {
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false
	}
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true), set_config('app.user_id', $2, true)",
		workspaceFromCtx(ctx), userFromCtx(ctx)); err != nil {
		_ = tx.Rollback(ctx)
		return nil, false
	}
	return tx, true
}

func validatePhases(phases []templatePhase) string {
	if len(phases) == 0 {
		return "at least one phase required"
	}
	for _, ph := range phases {
		if strings.TrimSpace(ph.Name) == "" {
			return "phase name required"
		}
		for _, t := range ph.Tasks {
			if strings.TrimSpace(t.Title) == "" || len(t.Title) > 255 {
				return "task title required (1-255)"
			}
		}
	}
	return ""
}

func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Category    string          `json:"category"`
		Phases      []templatePhase `json:"phases"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.Name == "" || len(req.Name) > 120 {
		problem(w, http.StatusBadRequest, "name required (1-120)")
		return
	}
	switch req.Category {
	case "onboarding", "migration", "implementation", "support":
	default:
		problem(w, http.StatusBadRequest, "category must be onboarding|migration|implementation|support")
		return
	}
	if msg := validatePhases(req.Phases); msg != "" {
		problem(w, http.StatusBadRequest, msg)
		return
	}
	phasesJSON, _ := json.Marshal(req.Phases)

	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	t, err := scanTemplate(tx.QueryRow(r.Context(), `
		INSERT INTO templates (workspace_id, name, version, category, description, phases)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, '1.0.0', $2, NULLIF($3,''), $4)
		RETURNING `+templateCols, req.Name, req.Category, req.Description, phasesJSON))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(r.Context(), `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'template', $1, 'user', NULLIF(current_setting('app.user_id', true), '')::uuid, 'template.created', 'api', $2)`,
		t.ID, `{"version":"`+t.Version+`"}`); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	q := `SELECT ` + templateCols + ` FROM templates WHERE deleted_at IS NULL`
	args := []any{}
	if c := r.URL.Query().Get("category"); c != "" {
		args = append(args, c)
		q += ` AND category = $1`
	}
	q += ` ORDER BY created_at DESC LIMIT 200`
	rows, err := tx.Query(r.Context(), q, args...)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []templateOut{}
	for rows.Next() {
		var t templateOut
		var created time.Time
		if err := rows.Scan(&t.ID, &t.Name, &t.Version, &t.Category, &t.Phases, &t.IsActive, &t.UsageCount, &created); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		t.CreatedAt = created.UTC().Format(time.RFC3339)
		out = append(out, t)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getTemplate(w http.ResponseWriter, r *http.Request) {
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	t, err := scanTemplate(tx.QueryRow(r.Context(), `
		SELECT `+templateCols+` FROM templates WHERE id = $1 AND deleted_at IS NULL`, r.PathValue("id")))
	if err == pgx.ErrNoRows {
		problem(w, http.StatusNotFound, "template not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// newTemplateVersion copies the template as the next semver and flips active.
// Live projects keep reading the old version (§6.4 version pin).
func (s *Server) newTemplateVersion(w http.ResponseWriter, r *http.Request) {
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	ctx := r.Context()
	id := r.PathValue("id")
	src, err := scanTemplate(tx.QueryRow(ctx, `
		SELECT `+templateCols+` FROM templates WHERE id = $1 AND deleted_at IS NULL`, id))
	if err == pgx.ErrNoRows {
		problem(w, http.StatusNotFound, "template not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	// next patch version: 1.2.3 → 1.2.4
	next := bumpPatch(src.Version)
	t, err := scanTemplate(tx.QueryRow(ctx, `
		WITH deactivated AS (
			UPDATE templates SET is_active = false, updated_at = now()
			WHERE id = $1 AND is_active
		)
		INSERT INTO templates (workspace_id, name, version, category, description, phases, is_active)
		SELECT workspace_id, name, $2, category, description, phases, true
		FROM templates WHERE id = $1
		RETURNING `+templateCols, id, next))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'template', $1, 'user', NULLIF(current_setting('app.user_id', true), '')::uuid, 'template.version_created', 'api', $2)`,
		t.ID, `{"version":"`+t.Version+`","from":"`+src.Version+`"}`); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, t)
}

// createProjectFromTemplate: §6.4 — pick template, fill vars (name/customer/start),
// preview lands with the UI; create in <30s. Due = start + offset.
func (s *Server) createProjectFromTemplate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TemplateID string `json:"template_id"`
		CustomerID string `json:"customer_id"`
		Name       string `json:"name"`
		StartDate  string `json:"start_date"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.TemplateID == "" || req.CustomerID == "" || req.Name == "" || len(req.Name) > 120 || len(req.Name) < 3 {
		problem(w, http.StatusBadRequest, "template_id, customer_id, name (3-120) required")
		return
	}
	if req.StartDate == "" {
		problem(w, http.StatusBadRequest, "start_date required")
		return
	}

	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(r.Context()) }()
	ctx := r.Context()

	var phasesRaw []byte
	var tplID string
	if err := tx.QueryRow(ctx, `
		SELECT id, phases FROM templates WHERE id = $1 AND deleted_at IS NULL`, req.TemplateID).
		Scan(&tplID, &phasesRaw); err != nil {
		if err == pgx.ErrNoRows {
			problem(w, http.StatusNotFound, "template not found")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	var phases []templatePhase
	if err := json.Unmarshal(phasesRaw, &phases); err != nil || len(phases) == 0 {
		problem(w, http.StatusBadRequest, "template has no phases")
		return
	}

	p, err := scanProject(tx.QueryRow(ctx, `
		INSERT INTO projects (workspace_id, customer_id, name, status, start_date)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, $2, 'active', $3::date)
		RETURNING `+projectInsCols, req.CustomerID, req.Name, req.StartDate))
	if err != nil {
		if strings.Contains(err.Error(), "violates") {
			problem(w, http.StatusNotFound, "customer not found")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	for _, ph := range phases {
		for _, t := range ph.Tasks {
			offset := 0
			if t.DueOffsetDays != nil {
				offset = *t.DueOffsetDays
			}
			reqd := true
			if t.Required != nil {
				reqd = *t.Required
			}
			vis := false
			if t.CustomerVis != nil {
				vis = *t.CustomerVis
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO tasks (workspace_id, project_id, title, owner_type, required, customer_visible, due_at)
				VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, $2,
				        CASE WHEN $3 THEN 'customer' ELSE 'internal' END, $3, $4, $5::date + make_interval(days => $6))`,
				p.ID, t.Title, vis, reqd, req.StartDate, offset); err != nil {
				problem(w, http.StatusInternalServerError, "internal error")
				return
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE templates SET usage_count = usage_count + 1, updated_at = now() WHERE id = $1`, tplID); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'project', $1, 'user', NULLIF(current_setting('app.user_id', true), '')::uuid, 'project.created_from_template', 'api', $2)`,
		p.ID, `{"template_id":"`+tplID+`","template_version_pinned":true}`); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.notifyEvent(ctx, workspaceFromCtx(ctx), "project.created_from_template",
		"🚀 Project created from template: "+p.Name, p.ID)
	writeJSON(w, http.StatusCreated, p)
}

// bumpPatch increments the semver patch: "1.2.3" → "1.2.4".
func bumpPatch(v string) string {
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return "1.0.1"
	}
	a, b, c := atoi(parts[0]), atoi(parts[1]), atoi(parts[2])
	c++
	return itoa(a) + "." + itoa(b) + "." + itoa(c)
}

func atoi(s string) int {
	n := 0
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int(ch-'0')
	}
	return n
}
