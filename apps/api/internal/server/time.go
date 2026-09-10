package server

// Time entries v1 (issue #52, spec §11 "Time simple"): append-only staff
// logging against tasks/projects. Policies enforced here (12h max, no
// future, no >7d back); corrections + approvals are P1. Portal is not
// involved — staff-only by design.

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
)

type timeEntryOut struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	TaskID    string `json:"task_id,omitempty"`
	Minutes   int    `json:"minutes"`
	StartedAt string `json:"started_at"`
	EndedAt   string `json:"ended_at"`
	Note      string `json:"note,omitempty"`
	Status    string `json:"status"`
}

const (
	maxEntryMinutes = 12 * 60            // spec: max 12h without overtime flag (flag is P1)
	maxBackdate     = 7 * 24 * time.Hour // spec: no logs >7 days back
)

// logTime is shared by POST /v1/tasks/{id}/time and /v1/projects/{id}/time.
// targetTaskID empty = project-level entry.
func (s *Server) logTime(targetTaskID string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			StartedAt string `json:"started_at"`
			EndedAt   string `json:"ended_at"`
			Note      string `json:"note"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
			problem(w, http.StatusBadRequest, "malformed request body")
			return
		}
		started, err := time.Parse(time.RFC3339, req.StartedAt)
		if err != nil {
			problem(w, http.StatusBadRequest, "started_at must be RFC3339")
			return
		}
		ended, err := time.Parse(time.RFC3339, req.EndedAt)
		if err != nil {
			problem(w, http.StatusBadRequest, "ended_at must be RFC3339")
			return
		}
		now := time.Now()
		// spec policies: positive interval, <=12h, ended not future, not >7d back
		if !ended.After(started) {
			problem(w, http.StatusBadRequest, "ended_at must be after started_at")
			return
		}
		if ended.Sub(started) > maxEntryMinutes*time.Minute {
			problem(w, http.StatusBadRequest, "entries over 12h are rejected in v1 (overtime flag comes with approvals)")
			return
		}
		if ended.After(now.Add(time.Minute)) {
			problem(w, http.StatusBadRequest, "ended_at cannot be in the future")
			return
		}
		if started.Before(now.Add(-maxBackdate)) {
			problem(w, http.StatusBadRequest, "cannot log more than 7 days back")
			return
		}
		minutes := int(ended.Sub(started).Minutes())

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

		// resolve the task or project (RLS: other workspaces' rows 404)
		var projectID string
		var taskID *string
		if targetTaskID != "" {
			err = tx.QueryRow(ctx, `
				SELECT project_id FROM tasks
				WHERE id = $1 AND deleted_at IS NULL`, targetTaskID).Scan(&projectID)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					problem(w, http.StatusNotFound, "task not found")
					return
				}
				problem(w, http.StatusInternalServerError, "internal error")
				return
			}
			taskID = &targetTaskID
		} else {
			err = tx.QueryRow(ctx, `
				SELECT id FROM projects
				WHERE id = $1 AND deleted_at IS NULL`, r.PathValue("id")).Scan(&projectID)
			if err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					problem(w, http.StatusNotFound, "project not found")
					return
				}
				problem(w, http.StatusInternalServerError, "internal error")
				return
			}
		}

		// time entries are personal (spec: timesheets per user) — an
		// anonymous/system actor (static CI token) cannot log time.
		userID := userFromCtx(ctx)
		if userID == "" {
			problem(w, http.StatusBadRequest, "time entries require a user identity (login)")
			return
		}
		var id string
		err = tx.QueryRow(ctx, `
			INSERT INTO time_entries (workspace_id, project_id, task_id, user_id, started_at, ended_at, minutes, note)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, NULLIF($8,''))
			RETURNING id`,
			workspaceFromCtx(ctx), projectID, taskID, userID, started, ended, minutes, req.Note).
			Scan(&id)
		if err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source)
			VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'time_entry', $1, 'user',
			        NULLIF(current_setting('app.user_id', true), '')::uuid, 'time.logged', 'api')`,
			id); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		if err := tx.Commit(ctx); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		out := timeEntryOut{
			ID: id, ProjectID: projectID, Minutes: minutes,
			StartedAt: started.UTC().Format(time.RFC3339),
			EndedAt:   ended.UTC().Format(time.RFC3339),
			Note:      req.Note, Status: "draft",
		}
		if taskID != nil {
			out.TaskID = *taskID
		}
		writeJSON(w, http.StatusCreated, out)
	}
}

func (s *Server) logTaskTime(w http.ResponseWriter, r *http.Request) {
	s.logTime(r.PathValue("id"))(w, r)
}
func (s *Server) logProjectTime(w http.ResponseWriter, r *http.Request) { s.logTime("")(w, r) }

// listTime handles both project + me scopes. meScope=true filters to the
// caller's user_id and ignores the project filter.
func (s *Server) listTime(meScope bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		q := r.URL.Query()
		from, to := q.Get("from"), q.Get("to")
		if from == "" {
			from = time.Now().AddDate(0, 0, -7).UTC().Format(time.RFC3339)
		}
		if to == "" {
			to = time.Now().UTC().Format(time.RFC3339)
		}
		fromT, err := time.Parse(time.RFC3339, from)
		if err != nil {
			problem(w, http.StatusBadRequest, "from must be RFC3339")
			return
		}
		toT, err := time.Parse(time.RFC3339, to)
		if err != nil {
			problem(w, http.StatusBadRequest, "to must be RFC3339")
			return
		}

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

		sql := `
			SELECT t.id, t.project_id, t.task_id::text, t.minutes, t.started_at, t.ended_at, COALESCE(t.note,''), t.status
			FROM time_entries t
			WHERE t.deleted_at IS NULL AND t.started_at >= $1 AND t.started_at < $2`
		args := []any{fromT, toT}
		if meScope {
			sql += ` AND t.user_id = $3`
			args = append(args, userFromCtx(ctx))
		} else {
			sql += ` AND t.project_id = $3::uuid`
			args = append(args, r.PathValue("id"))
		}
		sql += ` ORDER BY t.started_at DESC`
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		defer rows.Close()
		out := []timeEntryOut{}
		for rows.Next() {
			var e timeEntryOut
			var taskID *string
			var started, ended time.Time
			if err := rows.Scan(&e.ID, &e.ProjectID, &taskID, &e.Minutes, &started, &ended, &e.Note, &e.Status); err != nil {
				problem(w, http.StatusInternalServerError, "internal error")
				return
			}
			if taskID != nil {
				e.TaskID = *taskID
			}
			e.StartedAt = started.UTC().Format(time.RFC3339)
			e.EndedAt = ended.UTC().Format(time.RFC3339)
			out = append(out, e)
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func (s *Server) listProjectTime(w http.ResponseWriter, r *http.Request) { s.listTime(false)(w, r) }
func (s *Server) listMyTime(w http.ResponseWriter, r *http.Request)      { s.listTime(true)(w, r) }
