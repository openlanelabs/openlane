package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Assistant v1 (P2 §15.6, the final Nitro agent): preps the
// QBR/steering deck + talk track. GROUNDING-FIRST: we gather the
// project's numbers deterministically (canonical margins, tasks,
// budget, approvals, signals) and the smart model may only use what
// we hand it — the Doc Agent's uncited-paragraph validator flags any
// fabricated claim, and the response echoes the grounding so a human
// verifies in one glance. Drafts save internal-only; humans publish.

type assistantDeckReq struct {
	ProjectID string `json:"project_id"`
	Kind      string `json:"kind"` // qbr | steering
}

type deckGrounding struct {
	Project          string   `json:"project"`
	Customer         string   `json:"customer"`
	Billed           float64  `json:"billed"`
	Cost             float64  `json:"cost"`
	MarginPct        *int     `json:"margin_pct"`
	BudgetHours      *int     `json:"budget_hours"`
	LoggedMinutes    int      `json:"logged_minutes"`
	OpenTasks        int      `json:"open_tasks"`
	OverdueTasks     int      `json:"overdue_tasks"`
	PendingApprovals int      `json:"pending_approvals"`
	Upcoming         []string `json:"upcoming"`
	Signals          []string `json:"signals"`
}

const assistantSystem = "You draft quarterly business review (QBR) and steering meeting decks for a " +
	"professional services firm. You receive GROUNDING DATA — the only facts you may use. " +
	"Output markdown only: sections '## Executive Summary', '## Wins', '## Risks & Mitigations', " +
	"'## Budget & Margin', '## Next Steps', then '## Talk Track' with 5-8 bullets of what to " +
	"actually say on the call. Every number must appear verbatim in the grounding data. " +
	"If the grounding lacks something a section needs, write 'TBD (no data)' — never invent. " +
	"Keep risks concrete and tied to the numbers; do not flatter."

func (s *Server) assistantDeck(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	var req assistantDeckReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "invalid body")
		return
	}
	if req.Kind != "qbr" && req.Kind != "steering" {
		problem(w, http.StatusBadRequest, "kind must be qbr or steering")
		return
	}
	if req.ProjectID == "" {
		problem(w, http.StatusBadRequest, "project_id is required")
		return
	}

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
	cfg, err := loadLLMConfig(ctx, tx)
	if err != nil {
		problem(w, http.StatusBadRequest, "no llm configured for this workspace — set one under Agents → BYO-LLM")
		return
	}

	// ---- grounding (deterministic, in-tx) ----
	g := deckGrounding{Upcoming: []string{}, Signals: []string{}}
	var customerName *string
	if err := tx.QueryRow(ctx, `SELECT p.name, c.name, p.budget_hours
		FROM projects p LEFT JOIN customers c ON c.id = p.customer_id
		WHERE p.id = $1::uuid AND p.deleted_at IS NULL`, req.ProjectID).
		Scan(&g.Project, &customerName, &g.BudgetHours); err != nil {
		problem(w, http.StatusNotFound, "project not found")
		return
	}
	if customerName != nil {
		g.Customer = *customerName
	}
	// canonical margins
	if mrows, err := marginRows(ctx, tx); err == nil {
		for _, m := range mrows {
			if m.ProjectID == req.ProjectID {
				g.Billed, g.Cost = m.Billed, m.Cost
				if m.Billed > 0 {
					pct := int((m.Billed - m.Cost) / m.Billed * 100)
					g.MarginPct = &pct
				}
			}
		}
	}
	// logged minutes (all statuses — burn view), open/overdue counts,
	// pending approvals
	_ = tx.QueryRow(ctx, `SELECT
		(SELECT COALESCE(sum(minutes),0) FROM time_entries WHERE project_id = $1::uuid AND deleted_at IS NULL),
		(SELECT count(*) FROM tasks WHERE project_id = $1::uuid AND deleted_at IS NULL AND status NOT IN ('done','waived')),
		(SELECT count(*) FROM tasks WHERE project_id = $1::uuid AND deleted_at IS NULL AND customer_visible AND due_at < now() AND status NOT IN ('done','waived')),
		(SELECT count(*) FROM approvals WHERE project_id = $1::uuid AND deleted_at IS NULL AND status = 'pending')`,
		req.ProjectID).Scan(&g.LoggedMinutes, &g.OpenTasks, &g.OverdueTasks, &g.PendingApprovals)
	// next 3 upcoming tasks
	ur, err := tx.Query(ctx, `SELECT title FROM tasks
		WHERE project_id = $1::uuid AND deleted_at IS NULL AND due_at > now() AND status NOT IN ('done','waived')
		ORDER BY due_at LIMIT 3`, req.ProjectID)
	if err == nil {
		for ur.Next() {
			var t string
			if ur.Scan(&t) == nil {
				g.Upcoming = append(g.Upcoming, t)
			}
		}
		ur.Close()
	}
	// this project's signals (deterministic subset of the §15.6 set:
	// budget_burn + overdue_cluster rows for THIS project)
	sr, err := tx.Query(ctx, `SELECT title FROM (
		SELECT format('%sh of %sh budget logged (%s%%)', p.budget_hours, l.mins/60, round(100.0*l.mins/(p.budget_hours*60))) AS title
		FROM projects p JOIN (SELECT project_id, sum(minutes) mins FROM time_entries WHERE deleted_at IS NULL GROUP BY project_id) l
		  ON l.project_id = p.id
		WHERE p.id = $1::uuid AND p.budget_hours > 0 AND l.mins >= p.budget_hours*60*0.8
		UNION ALL
		SELECT format('%s overdue customer tasks', count(*)) FROM tasks t
		WHERE t.project_id = $1::uuid AND t.deleted_at IS NULL AND t.customer_visible AND t.due_at < now()
		  AND t.status NOT IN ('done','waived')
		HAVING count(*) >= 3) s`, req.ProjectID)
	if err == nil {
		for sr.Next() {
			var t string
			if sr.Scan(&t) == nil {
				g.Signals = append(g.Signals, t)
			}
		}
		sr.Close()
	}

	// ---- prompt ----
	groundingJSON, _ := json.Marshal(g)
	prompt := "DECK KIND: " + req.Kind + "\nGROUNDING DATA (the ONLY facts you may use):\n" + string(groundingJSON)
	res, err := llmComplete(ctx, cfg, "smart", assistantSystem, prompt)
	if err != nil {
		problem(w, http.StatusBadGateway, "llm provider failed: "+err.Error())
		return
	}
	deck := strings.TrimSpace(res.Text)
	uncited := uncitedParagraphs(deck)

	// ---- store (Doc Agent pattern): docs + doc_versions v1, internal ----
	kindLabel := map[string]string{"qbr": "QBR", "steering": "Steering"}[req.Kind]
	var docID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO docs (workspace_id, project_id, title, customer_visible, latest_version, created_by)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1::uuid, $2, false, 1,
		        NULLIF(current_setting('app.user_id', true), '')::uuid)
		RETURNING id`,
		req.ProjectID, kindLabel+" deck — "+g.Project).Scan(&docID); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO doc_versions (workspace_id, doc_id, version, content_md, created_by)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, 1, $2,
		        NULLIF(current_setting('app.user_id', true), '')::uuid)`,
		docID, deck); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_runs (workspace_id, agent, status, model, input_ref, output_ref, cost_cents, finished_at)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'assistant', 'succeeded', $1,
		        $2, 'assistant:' || $3::text, $4, now())`,
		res.Model, req.Kind+":"+req.ProjectID, docID, llmCostCents(res)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"doc_id":     docID,
		"uncited":    uncited,
		"grounding":  g,
		"model":      res.Model,
		"cost_cents": llmCostCents(res),
	})
}
