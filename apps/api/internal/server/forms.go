package server

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// forms.go — §9/§272: definitions + responses. Required fields are
// ENFORCED here (the Rocketlane gap): a missing required field 400s
// naming the field; unknown keys are stripped; types are checked.

type formField struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Type     string `json:"type"` // text | date | file
	Required bool   `json:"required"`
}

type formOut struct {
	ID          string      `json:"id"`
	ProjectID   string      `json:"project_id"`
	Title       string      `json:"title"`
	Description string      `json:"description,omitempty"`
	Fields      []formField `json:"fields"`
	Published   bool        `json:"published"`
}

func validateFormFields(fields []formField) string {
	if len(fields) == 0 || len(fields) > 50 {
		return "1-50 fields required"
	}
	seen := map[string]bool{}
	for _, f := range fields {
		if f.Key == "" || len(f.Key) > 64 || !isFormKey(f.Key) {
			return "field keys must be snake_case, 1-64 chars"
		}
		if seen[f.Key] {
			return "duplicate field key " + f.Key
		}
		seen[f.Key] = true
		if f.Type != "text" && f.Type != "date" && f.Type != "file" {
			return "field type must be text|date|file"
		}
	}
	return ""
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

func isFormKey(s string) bool {
	for i, r := range s {
		if r == '_' {
			continue
		}
		if r < 'a' || r > 'z' {
			if i == 0 || r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// validateAnswers enforces required fields + types server-side.
// Returns the sanitized map (unknown keys stripped).
func validateAnswers(fields []formField, answers map[string]string) (map[string]string, string) {
	out := map[string]string{}
	for _, f := range fields {
		v, ok := answers[f.Key]
		v = strings.TrimSpace(v)
		if f.Required && (!ok || v == "") {
			return nil, fmt.Sprintf("missing required field %q", f.Key)
		}
		if v == "" {
			continue
		}
		switch f.Type {
		case "date":
			if _, err := time.Parse("2006-01-02", v); err != nil {
				return nil, fmt.Sprintf("field %q must be a date (YYYY-MM-DD)", f.Key)
			}
		case "file":
			if len(v) > 36 { // ponytail: file answers reference a files.uuid; upload UI P1
				return nil, fmt.Sprintf("field %q must reference a file id", f.Key)
			}
		}
		if len(v) > 5000 {
			return nil, fmt.Sprintf("field %q too long (5000 max)", f.Key)
		}
		out[f.Key] = v
	}
	return out, ""
}

// POST /v1/projects/{id}/forms — staff create
func (s *Server) createForm(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title       string      `json:"title"`
		Description string      `json:"description"`
		Fields      []formField `json:"fields"`
		Published   *bool       `json:"published"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.Title == "" || len(req.Title) > 255 {
		problem(w, http.StatusBadRequest, "title must be 1-255 chars")
		return
	}
	if msg := validateFormFields(req.Fields); msg != "" {
		problem(w, http.StatusBadRequest, msg)
		return
	}
	published := true
	if req.Published != nil {
		published = *req.Published
	}
	ctx := r.Context()
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var f formOut
	err := tx.QueryRow(ctx, `
		INSERT INTO forms (workspace_id, project_id, title, description, fields, published, created_by)
		SELECT p.workspace_id, p.id, $2, NULLIF($3,''), $4, $5,
		       NULLIF(current_setting('app.user_id', true), '')::uuid
		FROM projects p
		WHERE p.id = $1::uuid AND p.deleted_at IS NULL
		RETURNING id, project_id, title, COALESCE(description,''), fields, published`,
		r.PathValue("id"), req.Title, req.Description, mustJSON(req.Fields), published).
		Scan(&f.ID, &f.ProjectID, &f.Title, &f.Description, &f.Fields, &f.Published)
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
	writeJSON(w, http.StatusCreated, f)
}

// GET /v1/projects/{id}/forms — staff list
func (s *Server) listProjectForms(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	rows, err := tx.Query(ctx, `
		SELECT id, project_id, title, COALESCE(description,''), fields, published
		FROM forms WHERE project_id = $1::uuid AND deleted_at IS NULL
		ORDER BY created_at DESC`, r.PathValue("id"))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []formOut{}
	for rows.Next() {
		var f formOut
		if err := rows.Scan(&f.ID, &f.ProjectID, &f.Title, &f.Description, &f.Fields, &f.Published); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		out = append(out, f)
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /v1/forms/{id}/responses.csv — staff CSV export (§272)
func (s *Server) exportFormResponsesCSV(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, ok := s.staffTx(r)
	if !ok {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var fields []formField
	var title string
	if err := tx.QueryRow(ctx,
		`SELECT title, fields FROM forms WHERE id = $1::uuid AND deleted_at IS NULL`,
		r.PathValue("id")).Scan(&title, &fields); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "form not found")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows, err := tx.Query(ctx, `
		SELECT answers, submitted_at, COALESCE(c.display_name,''), COALESCE(c.email,'')
		FROM form_responses fr
		LEFT JOIN contacts c ON c.id = fr.contact_id
		WHERE fr.form_id = $1::uuid
		ORDER BY fr.submitted_at DESC`, r.PathValue("id"))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()

	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q.csv", csvSafeTitle(title)))
	cw := csv.NewWriter(w)
	header := []string{"submitted_at", "contact_name", "contact_email"}
	for _, f := range fields {
		header = append(header, f.Key)
	}
	if err := cw.Write(header); err != nil {
		return
	}
	for rows.Next() {
		var answers map[string]string
		var submitted time.Time
		var name, email string
		if err := rows.Scan(&answers, &submitted, &name, &email); err != nil {
			return
		}
		rec := []string{submitted.UTC().Format(time.RFC3339), name, email}
		for _, f := range fields {
			rec = append(rec, answers[f.Key])
		}
		if err := cw.Write(rec); err != nil {
			return
		}
	}
	cw.Flush()
}

func csvSafeTitle(t string) string {
	out := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return -1
	}, t)
	if out == "" {
		return "responses"
	}
	return out
}

// GET /v1/portal/{token}/forms — portal sees published forms (fields to render)
func (s *Server) listPortalForms(ctx context.Context, tx pgx.Tx, w http.ResponseWriter, _ *http.Request) (int, error) {
	rows, err := tx.Query(ctx, `
		SELECT f.id, f.title, COALESCE(f.description,''), f.fields
		FROM forms f
		WHERE f.published AND f.deleted_at IS NULL
		ORDER BY f.created_at DESC`)
	if err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	defer rows.Close()
	type portalForm struct {
		ID          string      `json:"id"`
		Title       string      `json:"title"`
		Description string      `json:"description,omitempty"`
		Fields      []formField `json:"fields"`
	}
	out := []portalForm{}
	for rows.Next() {
		var f portalForm
		if err := rows.Scan(&f.ID, &f.Title, &f.Description, &f.Fields); err != nil {
			return http.StatusInternalServerError, errQuiet
		}
		out = append(out, f)
	}
	writeJSON(w, http.StatusOK, out)
	return 0, nil
}

// POST /v1/portal/{token}/forms/{id}/submit — one response per contact
func (s *Server) submitPortalForm(ctx context.Context, tx pgx.Tx, w http.ResponseWriter, r *http.Request) (int, error) {
	var req struct {
		Answers map[string]string `json:"answers"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return 0, nil
	}
	if len(req.Answers) > 50 {
		problem(w, http.StatusBadRequest, "too many answers")
		return 0, nil
	}
	sess := sessionFromCtx(ctx)

	// fetch the form under portal RLS (published, linked project) + enforce
	var fields []formField
	err := tx.QueryRow(ctx, `
		SELECT f.fields FROM forms f
		WHERE f.id = $1::uuid AND f.published AND f.deleted_at IS NULL`,
		r.PathValue("id")).Scan(&fields)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			problem(w, http.StatusNotFound, "form not available")
			return 0, nil
		}
		return http.StatusInternalServerError, errQuiet
	}
	answers, msg := validateAnswers(fields, req.Answers)
	if msg != "" {
		problem(w, http.StatusBadRequest, msg)
		return 0, nil
	}

	var id string
	err = tx.QueryRow(ctx, `
		INSERT INTO form_responses (workspace_id, form_id, contact_id, answers)
		SELECT f.workspace_id, f.id, $2::uuid, $3
		FROM forms f WHERE f.id = $1::uuid AND f.published
		RETURNING id`,
		r.PathValue("id"), sess.ContactID, mustJSON(answers)).Scan(&id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" { // already submitted — 404, no oracle
			problem(w, http.StatusNotFound, "form not available")
			return 0, nil
		}
		return http.StatusInternalServerError, errQuiet
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
		SELECT f.workspace_id, 'form', f.id, 'contact', $2, 'form.submitted', 'portal'
		FROM forms f WHERE f.id = $1`,
		r.PathValue("id"), sess.ContactID); err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	if err := tx.Commit(ctx); err != nil {
		return http.StatusInternalServerError, errQuiet
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
	return 0, nil
}
