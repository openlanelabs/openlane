package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Resourcing v1 (P1 §286-§298): people, allocations, utilization.
// Internal financials — portal never sees these (§207).

const personCols = `id, name, role, skills, cost_rate, bill_rate, capacity_hrs, timezone, active, created_at`

type personOut struct {
	ID          string   `json:"id"`
	UserID      *string  `json:"user_id"`
	Name        string   `json:"name"`
	Role        string   `json:"role"`
	Skills      []string `json:"skills"`
	CostRate    string   `json:"cost_rate"`
	BillRate    string   `json:"bill_rate"`
	CapacityHrs string   `json:"capacity_hrs"`
	Timezone    string   `json:"timezone"`
	Active      bool     `json:"active"`
	CreatedAt   string   `json:"created_at"`
}

// createPerson: POST /v1/people
func (s *Server) createPerson(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UserID      string   `json:"user_id"`
		Name        string   `json:"name"`
		Role        string   `json:"role"`
		Skills      []string `json:"skills"`
		CostRate    string   `json:"cost_rate"`
		BillRate    string   `json:"bill_rate"`
		CapacityHrs string   `json:"capacity_hrs"`
		Timezone    string   `json:"timezone"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if len(req.Name) < 1 || len(req.Name) > 120 {
		problem(w, http.StatusBadRequest, "name must be 1-120 chars")
		return
	}
	if len(req.Role) < 1 || len(req.Role) > 60 {
		problem(w, http.StatusBadRequest, "role must be 1-60 chars")
		return
	}
	for _, sk := range req.Skills {
		if len(sk) < 1 || len(sk) > 60 {
			problem(w, http.StatusBadRequest, "skill must be 1-60 chars")
			return
		}
	}
	if req.CapacityHrs == "" {
		req.CapacityHrs = "40"
	}
	if req.Timezone == "" {
		req.Timezone = "UTC"
	}
	if req.CostRate == "" {
		req.CostRate = "0"
	}
	if req.BillRate == "" {
		req.BillRate = "0"
	}
	if _, err := time.LoadLocation(req.Timezone); err != nil {
		problem(w, http.StatusBadRequest, "timezone must be an IANA name")
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

	var p personOut
	var userID *string
	if req.UserID != "" {
		userID = &req.UserID
	}
	if req.Skills == nil {
		req.Skills = []string{}
	}
	var created time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO people (workspace_id, user_id, name, role, skills, cost_rate, bill_rate, capacity_hrs, timezone)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, NULLIF($1, '')::uuid, $2, $3, $4, $5, $6, $7, $8)
		RETURNING `+personCols,
		req.UserID, req.Name, req.Role, req.Skills, req.CostRate, req.BillRate, req.CapacityHrs, req.Timezone).
		Scan(&p.ID, &p.Name, &p.Role, &p.Skills, &p.CostRate, &p.BillRate, &p.CapacityHrs, &p.Timezone, &p.Active, &created)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "23503") {
			problem(w, http.StatusBadRequest, "user already linked to a person in this workspace / user not found")
			return
		}
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	p.UserID = userID
	p.CreatedAt = created.UTC().Format(time.RFC3339)

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'person', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'person.created', 'api', $2)`,
		p.ID, `{"name":"`+p.Name+`"}`); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// listPeople: GET /v1/people?skills=react,go&active=true
func (s *Server) listPeople(w http.ResponseWriter, r *http.Request) {
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
	activeOnly := r.URL.Query().Get("active") == "true"
	rows, err := tx.Query(ctx, `
		SELECT `+personCols+` FROM people
		WHERE deleted_at IS NULL
		  AND (NOT $1::bool OR active)
		  AND (cardinality($2::text[]) = 0 OR skills && $2::text[])
		ORDER BY active DESC, name
		LIMIT 500`,
		activeOnly, toSlice(r.URL.Query().Get("skills")))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []personOut{}
	for rows.Next() {
		var p personOut
		var created time.Time
		if err := rows.Scan(&p.ID, &p.Name, &p.Role, &p.Skills, &p.CostRate, &p.BillRate, &p.CapacityHrs, &p.Timezone, &p.Active, &created); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		p.CreatedAt = created.UTC().Format(time.RFC3339)
		out = append(out, p)
	}
	writeJSON(w, http.StatusOK, out)
}

// patchPerson: PATCH /v1/people/{id} — active, rates, capacity, role, skills, timezone
func (s *Server) patchPerson(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Role        *string  `json:"role"`
		Skills      []string `json:"skills"`
		CostRate    *string  `json:"cost_rate"`
		BillRate    *string  `json:"bill_rate"`
		CapacityHrs *string  `json:"capacity_hrs"`
		Timezone    *string  `json:"timezone"`
		Active      *bool    `json:"active"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if req.Timezone != nil {
		if _, err := time.LoadLocation(*req.Timezone); err != nil {
			problem(w, http.StatusBadRequest, "timezone must be an IANA name")
			return
		}
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

	if req.Skills == nil {
		req.Skills = []string{}
	}
	var p personOut
	var userID *string
	var created time.Time
	err = tx.QueryRow(ctx, `
		UPDATE people SET
		  role = COALESCE($2, role),
		  skills = CASE WHEN $3::text[] IS NULL THEN skills ELSE $3::text[] END,
		  cost_rate = COALESCE($4, cost_rate),
		  bill_rate = COALESCE($5, bill_rate),
		  capacity_hrs = COALESCE($6, capacity_hrs),
		  timezone = COALESCE($7, timezone),
		  active = COALESCE($8, active),
		  updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING `+personCols,
		r.PathValue("id"), req.Role, req.Skills, req.CostRate, req.BillRate, req.CapacityHrs, req.Timezone, req.Active).
		Scan(&p.ID, &p.Name, &p.Role, &p.Skills, &p.CostRate, &p.BillRate, &p.CapacityHrs, &p.Timezone, &p.Active, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		problem(w, http.StatusNotFound, "person not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	_ = userID
	p.CreatedAt = created.UTC().Format(time.RFC3339)

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'person', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'person.updated', 'api', $2)`,
		p.ID, `{"active":`+boolText(p.Active)+`}`); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func boolText(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func toSlice(csv string) []string {
	if csv == "" {
		return []string{}
	}
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// createAllocation: POST /v1/people/{id}/allocations
func (s *Server) createAllocation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProjectID string `json:"project_id"`
		Role      string `json:"role"`
		HoursWeek string `json:"hours_week"`
		StartsOn  string `json:"starts_on"`
		EndsOn    string `json:"ends_on"`
		Kind      string `json:"kind"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "malformed request body")
		return
	}
	if _, err := time.Parse("2006-01-02", req.StartsOn); err != nil {
		problem(w, http.StatusBadRequest, "starts_on must be YYYY-MM-DD")
		return
	}
	if _, err := time.Parse("2006-01-02", req.EndsOn); err != nil {
		problem(w, http.StatusBadRequest, "ends_on must be YYYY-MM-DD")
		return
	}
	if req.EndsOn < req.StartsOn {
		problem(w, http.StatusBadRequest, "ends_on must be >= starts_on")
		return
	}
	if req.Kind == "" {
		req.Kind = "hard"
	}
	if req.Kind != "hard" && req.Kind != "soft" {
		problem(w, http.StatusBadRequest, "kind must be hard|soft")
		return
	}
	if len(req.Role) < 1 || len(req.Role) > 60 {
		problem(w, http.StatusBadRequest, "role must be 1-60 chars")
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

	var out allocationOut
	err = tx.QueryRow(ctx, `
		INSERT INTO allocations (workspace_id, person_id, project_id, role, hours_week, starts_on, ends_on, kind)
		SELECT NULLIF(current_setting('app.workspace_id', true), '')::uuid, p.id, pr.id, $3, $4, $5, $6, $7
		FROM people p, projects pr
		WHERE p.id = $1::uuid AND p.deleted_at IS NULL AND pr.id = $2::uuid AND pr.deleted_at IS NULL
		RETURNING id, person_id::text, project_id::text, role, hours_week, starts_on::text, ends_on::text, kind, created_at`,
		r.PathValue("id"), req.ProjectID, req.Role, req.HoursWeek, req.StartsOn, req.EndsOn, req.Kind).
		Scan(&out.ID, &out.PersonID, &out.ProjectID, &out.Role, &out.HoursWeek, &out.StartsOn, &out.EndsOn, &out.Kind, &out.CreatedRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		problem(w, http.StatusNotFound, "person or project not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	out.CreatedAt = out.CreatedRaw.UTC().Format(time.RFC3339)

	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'allocation', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'allocation.created', 'api', $2)`,
		out.ID, `{"person_id":"`+out.PersonID+`","project_id":"`+out.ProjectID+`"}`); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

type allocationOut struct {
	ID        string `json:"id"`
	PersonID  string `json:"person_id"`
	ProjectID string `json:"project_id"`
	Role      string `json:"role"`
	HoursWeek string `json:"hours_week"`
	StartsOn  string `json:"starts_on"`
	EndsOn    string `json:"ends_on"`
	Kind      string `json:"kind"`
	CreatedAt string `json:"created_at"`

	CreatedRaw time.Time
}

// listAllocations: GET /v1/people/{id}/allocations?project_id=&kind=
func (s *Server) listAllocations(w http.ResponseWriter, r *http.Request) {
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
	rows, err := tx.Query(ctx, `
		SELECT a.id, a.person_id::text, a.project_id::text, a.role, a.hours_week, a.starts_on::text, a.ends_on::text, a.kind, a.created_at
		FROM allocations a
		JOIN people p ON p.id = a.person_id AND p.id = $1::uuid AND p.deleted_at IS NULL
		WHERE a.deleted_at IS NULL
		  AND ($2 = '' OR a.project_id = $2::uuid)
		  AND ($3 = '' OR a.kind = $3)
		ORDER BY a.starts_on DESC LIMIT 200`,
		r.PathValue("id"), r.URL.Query().Get("project_id"), r.URL.Query().Get("kind"))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()
	out := []allocationOut{}
	for rows.Next() {
		var a allocationOut
		if err := rows.Scan(&a.ID, &a.PersonID, &a.ProjectID, &a.Role, &a.HoursWeek, &a.StartsOn, &a.EndsOn, &a.Kind, &a.CreatedRaw); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		a.CreatedAt = a.CreatedRaw.UTC().Format(time.RFC3339)
		out = append(out, a)
	}
	writeJSON(w, http.StatusOK, out)
}

// deleteAllocation: DELETE /v1/allocations/{id}
func (s *Server) deleteAllocation(w http.ResponseWriter, r *http.Request) {
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
	var id string
	err = tx.QueryRow(ctx, `
		UPDATE allocations SET deleted_at = now(), updated_at = now()
		WHERE id = $1::uuid AND deleted_at IS NULL
		RETURNING id`, r.PathValue("id")).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		problem(w, http.StatusNotFound, "allocation not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'allocation', $1, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'allocation.deleted', 'api', '{}')`, id); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getPersonUtilization: GET /v1/people/{id}/utilization?start=&end=
// §298: billable_hours_logged / capacity. v1: PTO model doesn't exist yet,
// so capacity is nominal weekly capacity scaled to the range (ponytail:
// real availability needs the calendar overlay — §544).
func (s *Server) getPersonUtilization(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	start, end := q.Get("start"), q.Get("end")
	if _, err := time.Parse("2006-01-02", start); err != nil {
		problem(w, http.StatusBadRequest, "start must be YYYY-MM-DD")
		return
	}
	if _, err := time.Parse("2006-01-02", end); err != nil {
		problem(w, http.StatusBadRequest, "end must be YYYY-MM-DD")
		return
	}
	if end < start {
		problem(w, http.StatusBadRequest, "end must be >= start")
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

	var out struct {
		PersonID        string   `json:"person_id"`
		LoggedMinutes   int      `json:"logged_minutes"`
		LoggableMinutes float64  `json:"loggable_minutes"` // capacity × workdays
		Utilization     *float64 `json:"utilization"`
		Weeks           float64  `json:"weeks"`
	}
	var capacityHrs float64
	err = tx.QueryRow(ctx, `
		SELECT p.id::text, p.capacity_hrs FROM people p
		WHERE p.id = $1::uuid AND p.deleted_at IS NULL`, r.PathValue("id")).
		Scan(&out.PersonID, &capacityHrs)
	if errors.Is(err, pgx.ErrNoRows) {
		problem(w, http.StatusNotFound, "person not found")
		return
	}
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(te.minutes), 0) FROM time_entries te
		WHERE te.workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
		  AND te.user_id = (SELECT user_id FROM people WHERE id = $1::uuid)
		  AND te.started_at::date BETWEEN $2 AND $3
		  AND te.deleted_at IS NULL`,
		r.PathValue("id"), start, end).Scan(&out.LoggedMinutes); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	// capacity: workdays (Mon–Fri) in range × daily capacity
	var workdays float64
	if err := tx.QueryRow(ctx, `SELECT count(*)::float8
		FROM generate_series($1::date, $2::date, '1 day') d
		WHERE extract(dow from d) BETWEEN 1 AND 5`, start, end).Scan(&workdays); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	out.Weeks = workdays / 5
	out.LoggableMinutes = workdays * capacityHrs * 60 / 5
	if out.LoggableMinutes > 0 {
		u := float64(out.LoggedMinutes) / out.LoggableMinutes
		out.Utilization = &u
	}

	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, out)
}
