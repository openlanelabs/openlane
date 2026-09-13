package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Config Agent v1 (P2 §15.3): reads a form response (the customer's
// order form), proposes a project setup sheet (tasks + budget) via the
// smart model, validates EVERYTHING in code, and only executes what a
// human approves — deterministic inserts in one tx. Agents propose,
// humans commit.

type configSheetReq struct {
	ProjectID      string `json:"project_id"`
	FormResponseID string `json:"form_response_id"`
}

// the validated sheet (what we store + return)
type configSheetTask struct {
	Title           string `json:"title"`
	OwnerType       string `json:"owner_type"`
	DueDays         int    `json:"due_days"`
	CustomerVisible bool   `json:"customer_visible"`
}

type configSheet struct {
	CustomerGoals string            `json:"customer_goals"`
	Tasks         []configSheetTask `json:"tasks"`
	BudgetHours   *int              `json:"budget_hours"`
	Notes         string            `json:"notes"`
}

const configAgentSystem = "You set up professional-services projects from customer order-form responses. " +
	"Given the form questions, the customer's answers, and project context, propose a setup sheet as JSON only: " +
	`{"customer_goals": "1-2 lines", "tasks": [{"title": "...", "owner_type": "internal"|"customer", ` +
	`"due_days": 1-365, "customer_visible": true|false}], "budget_hours": int|null, "notes": "..."}. ` +
	"Only use facts from the answers — if something is unknown, omit it or write 'TBD (not in responses)'. " +
	"3-8 tasks typical. Never invent services, dates, or numbers."

func (s *Server) configSheet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	var req configSheetReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&req); err != nil || req.ProjectID == "" || req.FormResponseID == "" {
		problem(w, http.StatusBadRequest, "project_id and form_response_id are required")
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

	// project + form + response, all inside the ws-scoped tx
	var projName string
	var budgetHours *int
	if err := tx.QueryRow(ctx, `SELECT name, budget_hours FROM projects
		WHERE id = $1::uuid AND deleted_at IS NULL`, req.ProjectID).Scan(&projName, &budgetHours); err != nil {
		problem(w, http.StatusNotFound, "project not found")
		return
	}
	var formTitle string
	var fieldsRaw, answersRaw []byte
	if err := tx.QueryRow(ctx, `
		SELECT f.title, f.fields, fr.answers
		FROM form_responses fr JOIN forms f ON f.id = fr.form_id AND f.deleted_at IS NULL
		WHERE fr.id = $1::uuid`, req.FormResponseID).Scan(&formTitle, &fieldsRaw, &answersRaw); err != nil {
		problem(w, http.StatusNotFound, "form response not found")
		return
	}

	res, err := llmComplete(ctx, cfg, "smart", configAgentSystem,
		"Project: "+projName+" (existing budget_hours: "+intPtrString(budgetHours)+")\n"+
			"Order form: "+formTitle+"\n\nQUESTIONS+ANSWERS:\n"+qaText(fieldsRaw, answersRaw))
	if err != nil {
		problem(w, http.StatusBadGateway, "llm provider failed: "+err.Error())
		return
	}

	// validate the LLM's proposal — code is the gate, not the prompt
	var raw configSheet
	var tasks []configSheetTask
	if err := json.Unmarshal([]byte(stripCodeFence(res.Text)), &raw); err == nil {
		for i, t := range raw.Tasks {
			if i >= 20 {
				break // cap; excess dropped
			}
			title := strings.TrimSpace(t.Title)
			if title == "" || len(title) > 200 || t.DueDays < 1 || t.DueDays > 365 {
				continue
			}
			if t.OwnerType != "internal" && t.OwnerType != "customer" {
				t.OwnerType = "internal" // safe default; validation keeps the row
			}
			tasks = append(tasks, configSheetTask{Title: title, OwnerType: t.OwnerType,
				DueDays: t.DueDays, CustomerVisible: t.CustomerVisible})
		}
	}
	if raw.BudgetHours != nil && (*raw.BudgetHours < 0 || *raw.BudgetHours > 10000) {
		raw.BudgetHours = nil
	}
	sheet := configSheet{CustomerGoals: raw.CustomerGoals, Tasks: tasks,
		BudgetHours: raw.BudgetHours, Notes: raw.Notes}

	var sheetID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO config_sheets (workspace_id, project_id, form_response_id, sheet, created_by)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1::uuid, $2::uuid, $3,
		        NULLIF(current_setting('app.user_id', true), '')::uuid)
		RETURNING id`,
		req.ProjectID, req.FormResponseID, mustJSON(sheet)).Scan(&sheetID); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_runs (workspace_id, agent, status, model, input_ref, output_ref, cost_cents, finished_at)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'workforce', 'succeeded', $1,
		        $2, 'config:' || $3::text, $4, now())`,
		res.Model, "form_response:"+req.FormResponseID, sheetID, llmCostCents(res)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"config_sheet_id": sheetID, "sheet": sheet,
		"model": res.Model, "cost_cents": llmCostCents(res),
	})
}

// configApprove: execute the sheet deterministically — tasks + budget
// in ONE tx; state machine draft→executed (re-approve 404). §15.3
// 'human Approve -> executes -> writes audit back'.
func (s *Server) configApprove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
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
	var projID string
	var sheetRaw []byte
	if err := tx.QueryRow(ctx, `
		SELECT project_id, sheet FROM config_sheets
		WHERE id = $1::uuid AND status = 'draft'`, r.PathValue("id")).
		Scan(&projID, &sheetRaw); err != nil {
		problem(w, http.StatusNotFound, "sheet not found or already executed")
		return
	}
	var sheet configSheet
	if err := json.Unmarshal(sheetRaw, &sheet); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}

	refs := map[string]any{"task_ids": []string{}, "budget_hours_set": sheet.BudgetHours != nil}
	for _, t := range sheet.Tasks {
		var id string
		if err := tx.QueryRow(ctx, `
			INSERT INTO tasks (workspace_id, project_id, title, owner_type, status, customer_visible, due_at, created_by)
			VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1::uuid, $2, $3, 'todo', $4,
			        now() + make_interval(days => $5::int),
			        NULLIF(current_setting('app.user_id', true), '')::uuid)
			RETURNING id`,
			projID, t.Title, t.OwnerType, t.CustomerVisible, t.DueDays).Scan(&id); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		refs["task_ids"] = append(refs["task_ids"].([]string), id)
	}
	if sheet.BudgetHours != nil {
		if _, err := tx.Exec(ctx, `
			UPDATE projects SET budget_hours = $2::int, updated_at = now()
			WHERE id = $1::uuid`, projID, *sheet.BudgetHours); err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE config_sheets SET status = 'executed', executed_refs = $2, updated_at = now()
		WHERE id = $1::uuid AND status = 'draft'`,
		r.PathValue("id"), mustJSON(refs)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'config_sheet', $1::uuid, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'config.executed', 'agent', $2)`,
		r.PathValue("id"), mustJSON(refs)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "status": "executed", "refs": refs})
}

// helpers

func intPtrString(p *int) string {
	if p == nil {
		return "none"
	}
	return strconv.Itoa(*p)
}

// qaText: render form fields + answers as "Q: … A: …" lines for the prompt.
func qaText(fieldsRaw, answersRaw []byte) string {
	var fields []formField
	var answers map[string]string
	_ = json.Unmarshal(fieldsRaw, &fields)
	_ = json.Unmarshal(answersRaw, &answers)
	var sb strings.Builder
	for _, f := range fields {
		q := f.Label
		if q == "" {
			q = f.Key
		}
		a := answers[f.Key]
		if a == "" {
			a = "(no answer)"
		}
		sb.WriteString("Q: " + q + "\nA: " + a + "\n")
	}
	out := sb.String()
	if out == "" {
		out = "(form has no fields)"
	}
	return out
}
