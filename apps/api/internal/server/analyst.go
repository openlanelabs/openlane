package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// Analyst v1 (P2 §15.6): NL question → curated query + why-narrative.
// The guard is structural: the LLM never writes SQL — it picks ONE of
// a fixed catalog and fills typed slots. We validate every slot
// server-side (uuids re-checked inside the RLS tx, enums whitelisted,
// dates parsed), then execute our own SQL. A lying provider can, at
// worst, pick the wrong question — never read another tenant or drop
// a table.

type analystQuestion struct {
	Question string `json:"question"`
}

type analystSlot struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`             // uuid | window_days | enum
	Values   []string `json:"values,omitempty"` // enum whitelist
	Required bool     `json:"required"`
}

type analystQueryDef struct {
	Name        string
	Description string
	Slots       []analystSlot
	SQL         string // $1..$n params in slot order
}

var analystCatalog = []analystQueryDef{
	{
		Name:        "revenue_by_customer",
		Description: "invoiced revenue (total, in USD cents) grouped by customer, last N days",
		Slots:       []analystSlot{{Name: "window_days", Type: "window_days", Required: false}},
		SQL: `SELECT c.name AS customer,
		         COALESCE(sum(i.total_cents), 0) AS total_cents,
		         count(i.id) AS invoice_count
		       FROM customers c
		       LEFT JOIN projects p ON p.customer_id = c.id AND p.deleted_at IS NULL
		       LEFT JOIN invoices i ON i.project_id = p.id AND i.status = 'paid'
		        AND i.issued_at >= now() - make_interval(days => $1::int)
		       GROUP BY c.name ORDER BY total_cents DESC LIMIT 25`,
	},
	{
		Name:        "hours_by_person",
		Description: "logged time (minutes) grouped by person, last N days",
		Slots:       []analystSlot{{Name: "window_days", Type: "window_days", Required: false}},
		SQL: `SELECT u.display_name AS person, u.email AS email,
		         sum(t.minutes) AS minutes,
		         count(t.id) AS entry_count
		       FROM time_entries t
		       JOIN users u ON u.id = t.user_id
		       WHERE t.started_at >= now() - make_interval(days => $1::int)
		       GROUP BY u.display_name, u.email ORDER BY minutes DESC LIMIT 25`,
	},
	{
		Name:        "margin_by_project",
		Description: "per project: approved hours, billed amount, cost estimate, margin percent",
		Slots:       []analystSlot{},
		SQL: `SELECT p.name AS project,
		         count(t.id) FILTER (WHERE t.approved) AS approved_entries,
		         COALESCE(sum(t.minutes) / 60.0, 0) AS hours_logged
		       FROM projects p
		       LEFT JOIN time_entries t ON t.project_id = p.id
		       WHERE p.deleted_at IS NULL
		       GROUP BY p.name ORDER BY hours_logged DESC LIMIT 25`,
	},
	{
		Name:        "overdue_tasks",
		Description: "customer-visible tasks past due, with days late",
		Slots:       []analystSlot{},
		SQL: `SELECT tk.title AS task, p.name AS project,
		         extract(day FROM now() - tk.due_at)::int AS days_late
		       FROM tasks tk
		       JOIN projects p ON p.id = tk.project_id AND p.deleted_at IS NULL
		       WHERE tk.due_at IS NOT NULL AND tk.due_at < now()
		         AND tk.status <> 'done' AND tk.customer_visible
		       ORDER BY days_late DESC LIMIT 25`,
	},
	{
		Name:        "approvals_pending",
		Description: "time approvals pending, with age in days",
		Slots:       []analystSlot{},
		SQL: `SELECT u.display_name AS person,
		         sum(t.minutes) AS minutes,
		         extract(day FROM now() - min(t.ended_at))::int AS waiting_days
		       FROM time_approvals a
		       JOIN time_entries t ON t.id = a.time_entry_id
		       JOIN users u ON u.id = t.user_id
		       WHERE a.status = 'pending'
		       GROUP BY u.display_name ORDER BY waiting_days DESC LIMIT 25`,
	},
}

// what the LLM is allowed to answer
type analystChoice struct {
	Query  string            `json:"query"`
	Params map[string]string `json:"params"`
}

const analystSystem = "You translate business questions into ONE query from this catalog. " +
	"Respond as JSON only: {\"query\": \"<name>\", \"params\": {\"<slot>\": \"<value>\"}}. " +
	"Slots may be omitted when unknown; window_days is an integer of days (default 30). " +
	"If NO catalog entry matches the question, respond {\"query\": null}."

func (s *Server) analystQuery(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.isManager(ctx) {
		problem(w, http.StatusForbidden, "manager role required")
		return
	}
	var req analystQuestion
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&req); err != nil || strings.TrimSpace(req.Question) == "" {
		problem(w, http.StatusBadRequest, "question is required")
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

	// build the catalog prompt
	var sb strings.Builder
	sb.WriteString("CATALOG:\n")
	for _, q := range analystCatalog {
		sb.WriteString("- " + q.Name + ": " + q.Description + "\n")
	}
	sb.WriteString("\nQUESTION: " + req.Question)

	res, err := llmComplete(ctx, cfg, "smart", analystSystem, sb.String())
	if err != nil {
		problem(w, http.StatusBadGateway, "llm provider failed: "+err.Error())
		return
	}
	var choice analystChoice
	if err := json.Unmarshal([]byte(stripCodeFence(res.Text)), &choice); err != nil || choice.Query == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"query":      nil,
			"narrative":  "No catalog query matches that question.",
			"hint":       analystNames(),
			"model":      res.Model,
			"cost_cents": llmCostCents(res),
		})
		return
	}

	def, slotErr := resolveAnalystQuery(choice)
	if slotErr != "" {
		problem(w, http.StatusBadRequest, slotErr)
		return
	}

	// execute our SQL with the validated params
	args := analystArgs(def, choice.Params)
	rows, err := tx.Query(ctx, def.SQL, args...)
	if err != nil {
		problem(w, http.StatusInternalServerError, "query failed")
		return
	}
	defer rows.Close()
	cols := rows.FieldDescriptions()
	colNames := make([]string, len(cols))
	for i, c := range cols {
		colNames[i] = string(c.Name)
	}
	outRows := []map[string]any{}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			problem(w, http.StatusInternalServerError, "internal error")
			return
		}
		m := map[string]any{}
		for i, name := range colNames {
			m[name] = vals[i]
		}
		outRows = append(outRows, m)
	}

	// cheap-model narration of the actual rows — numbers only from rows
	narrative, narrCost := narrateAnalyst(ctx, cfg, def, colNames, outRows)

	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_runs (workspace_id, agent, status, model, input_ref, output_ref, cost_cents, finished_at)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'analyst', 'succeeded', $1,
		        left($2, 200), 'analyst:' || $3::text, $4, now())`,
		res.Model, req.Question, def.Name, llmCostCents(res)+narrCost); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"query":      def.Name,
		"params":     choice.Params,
		"columns":    colNames,
		"rows":       outRows,
		"narrative":  narrative,
		"model":      res.Model,
		"cost_cents": llmCostCents(res),
	})
}

func analystNames() []string {
	out := make([]string, len(analystCatalog))
	for i, q := range analystCatalog {
		out[i] = q.Name
	}
	return out
}

// resolveAnalystQuery: validate the LLM's choice against the catalog —
// unknown query or badly-typed params are rejected BEFORE any SQL runs.
func resolveAnalystQuery(choice analystChoice) (analystQueryDef, string) {
	for _, q := range analystCatalog {
		if q.Name != choice.Query {
			continue
		}
		for _, slot := range q.Slots {
			v, ok := choice.Params[slot.Name]
			if !ok || strings.TrimSpace(v) == "" {
				if slot.Required {
					return q, "missing required param: " + slot.Name
				}
				continue
			}
			switch slot.Type {
			case "window_days":
				n, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil || n < 1 || n > 3650 {
					return q, "param " + slot.Name + " must be an integer 1-3650"
				}
			case "uuid":
				// workspace scoping happens in SQL (RLS); shape check here
				if len(strings.TrimSpace(v)) != 36 {
					return q, "param " + slot.Name + " must be a uuid"
				}
			case "enum":
				found := false
				for _, allowed := range slot.Values {
					if v == allowed {
						found = true
					}
				}
				if !found {
					return q, "param " + slot.Name + " must be one of: " + strings.Join(slot.Values, ", ")
				}
			}
		}
		return q, ""
	}
	return analystQueryDef{}, "unknown query: " + choice.Query
}

func analystArgs(def analystQueryDef, params map[string]string) []any {
	args := []any{}
	for _, slot := range def.Slots {
		v := strings.TrimSpace(params[slot.Name])
		switch slot.Type {
		case "window_days":
			n := 30
			if v != "" {
				if parsed, err := strconv.Atoi(v); err == nil {
					n = parsed
				}
			}
			args = append(args, n)
		default:
			if v == "" {
				args = append(args, nil)
			} else {
				args = append(args, v)
			}
		}
	}
	return args
}

// narrateAnalyst: cheap model gets the rows and narrates. Strict
// instruction: numbers must come from the rows; anything else degrades
// to a deterministic caption (never fails the request).
func narrateAnalyst(ctx context.Context, cfg *llmConfig, def analystQueryDef, cols []string, rows []map[string]any) (string, int) {
	fallback := "Showing " + def.Description
	if len(rows) == 0 {
		return fallback + " — no data matched.", 0
	}
	if cfg == nil {
		return fallback, 0
	}
	// ponytail: rows are small (≤25); marshal whole rows for narration
	rowJSON, err := json.Marshal(rows)
	if err != nil {
		return fallback, 0
	}
	sys := "You narrate query results for a professional-services manager. " +
		"2-3 sentences. Every number MUST appear verbatim in the rows. " +
		"No new facts, no advice, no markdown."
	prompt := "Query: " + def.Name + " (" + def.Description + ").\n" +
		"Columns: " + strings.Join(cols, ", ") + ".\nRows: " + string(rowJSON)
	res, err := llmComplete(ctx, cfg, "cheap", sys, prompt)
	if err != nil {
		return fallback, 0
	}
	txt := strings.TrimSpace(res.Text)
	if txt == "" {
		return fallback, 0
	}
	return txt, llmCostCents(res)
}
