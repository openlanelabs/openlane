package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Resourcing Agent v1 (P2 §15.4): suggest a team for an allocation.
// Deterministic + explainable — the ranking IS the suggestion; every
// candidate carries a reason string ('explains why'). The §15.4 bias
// edge (always picks the same senior) is a load-balance penalty inside
// the score. No writes: suggest only.

type suggestReq struct {
	ProjectID string   `json:"project_id"`
	Role      string   `json:"role"`
	HoursWeek float64  `json:"hours_week"`
	StartsOn  string   `json:"starts_on"`
	EndsOn    string   `json:"ends_on"`
	Skills    []string `json:"skills"`
}

type candidateOut struct {
	PersonID  string   `json:"person_id"`
	Name      string   `json:"name"`
	Role      string   `json:"role"`
	Skills    []string `json:"skills"`
	Capacity  float64  `json:"capacity_hrs"`
	Allocated float64  `json:"allocated_hrs_window"`
	Headroom  float64  `json:"headroom_hrs"`
	Score     float64  `json:"score"`
	Reason    string   `json:"reason"`
}

func (s *Server) suggestTeam(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	var req suggestReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "invalid body")
		return
	}
	start, err := time.Parse("2006-01-02", req.StartsOn)
	if err != nil {
		problem(w, http.StatusBadRequest, "starts_on must be YYYY-MM-DD")
		return
	}
	end, err := time.Parse("2006-01-02", req.EndsOn)
	if err != nil || end.Before(start) {
		problem(w, http.StatusBadRequest, "ends_on must be YYYY-MM-DD after starts_on")
		return
	}
	if req.HoursWeek <= 0 || req.HoursWeek > 80 {
		problem(w, http.StatusBadRequest, "hours_week must be 1-80")
		return
	}
	if len(req.Skills) > 20 {
		problem(w, http.StatusBadRequest, "too many skills")
		return
	}

	// kill switch
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
	var enabled bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT agents_enabled FROM workspace_settings
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid), true)`).
		Scan(&enabled); err != nil || !enabled {
		problem(w, http.StatusServiceUnavailable, "agents disabled for this workspace (kill switch)")
		return
	}
	// project must exist (and RLS-scoped)
	var projName string
	if err := tx.QueryRow(ctx, `SELECT name FROM projects
		WHERE id = $1::uuid AND deleted_at IS NULL`, req.ProjectID).Scan(&projName); err != nil {
		problem(w, http.StatusNotFound, "project not found")
		return
	}

	rows, err := tx.Query(ctx, `
		SELECT p.id::text, p.name, p.role, p.skills, p.capacity_hrs,
		       COALESCE((
		         SELECT sum(a.hours_week * GREATEST(0,
		           (LEAST(a.ends_on, $2::date) - GREATEST(a.starts_on, $1::date) + 1)) / 7.0)
		         FROM allocations a
		         WHERE a.person_id = p.id AND a.deleted_at IS NULL AND a.kind = 'hard'
		           AND a.starts_on <= $2::date AND a.ends_on >= $1::date
		       ), 0) AS allocated
		FROM people p
		WHERE p.active AND p.deleted_at IS NULL`,
		start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer rows.Close()

	type cand struct {
		candidateOut
		missing int
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.PersonID, &c.Name, &c.Role, &c.Skills, &c.Capacity, &c.Allocated); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		// skill fit: required ⊆ person.skills → 0 missing
		c.missing = 0
		for _, want := range req.Skills {
			found := false
			for _, have := range c.Skills {
				if strings.EqualFold(strings.TrimSpace(have), strings.TrimSpace(want)) {
					found = true
					break
				}
			}
			if !found {
				c.missing++
			}
		}
		c.Headroom = c.Capacity - c.Allocated
		if c.Headroom < 0 {
			c.Headroom = 0
		}
		// §15.4 score: skill fit dominates, role match next, then
		// load-balance (the anti-bias penalty): lower allocated ratio
		// scores higher. Over-capacity people are excluded outright.
		c.Score = 0
		if c.Allocated <= c.Capacity {
			roleMatch := 0.0
			if req.Role != "" && strings.EqualFold(c.Role, req.Role) {
				roleMatch = 1
			}
			util := 0.0
			if c.Capacity > 0 {
				util = c.Allocated / c.Capacity
			}
			c.Score = float64(len(req.Skills)-c.missing)*10 + roleMatch*5 + (1-util)*3
		}
		parts := []string{}
		if len(req.Skills) > 0 {
			parts = append(parts, itoa(len(req.Skills)-c.missing)+"/"+itoa(len(req.Skills))+" skills match")
		}
		parts = append(parts, itoa(int(c.Allocated))+"h/"+itoa(int(c.Capacity))+"h allocated — "+itoa(int(c.Headroom))+"h headroom")
		if req.Role != "" {
			parts = append(parts, "role "+c.Role)
		}
		c.Reason = strings.Join(parts, ", ")
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	// sort: score desc, name asc (deterministic)
	for i := 0; i < len(cands); i++ {
		for j := i + 1; j < len(cands); j++ {
			if cands[j].Score > cands[i].Score ||
				(cands[j].Score == cands[i].Score && cands[j].Name < cands[i].Name) {
				cands[i], cands[j] = cands[j], cands[i]
			}
		}
	}
	out := []candidateOut{}
	for _, c := range cands {
		if c.Score <= 0 {
			continue // excluded: over capacity
		}
		out = append(out, c.candidateOut)
	}
	if len(out) > 10 {
		out = out[:10]
	}

	// audit rail
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_runs (workspace_id, agent, status, model, input_ref, output_ref, finished_at)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'resourcing', 'succeeded', 'none',
		        $1, $2, now())`,
		"suggest:project:"+req.ProjectID, firstOut(out)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"candidates": out})
}

func firstOut(out []candidateOut) string {
	if len(out) == 0 {
		return "no candidates"
	}
	return "top:" + out[0].Name
}
