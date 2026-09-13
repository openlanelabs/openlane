package server

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"
)

// Finance Guardian v1 (P2 §15.5): configurable margin thresholds.
// It READS and FLAGS — never fixes (no money-write path exists by
// design; §15.5 'doesn't auto-fix money without approval').
//
// marginRowsSQL is the CANONICAL per-project margin query (extracted
// per the third-copy rule): §324 semantics — approved+invoiced time
// only, bill rate resolved customer-card-first, cost from people,
// unpriced tracked not zeroed. Consumers: the guardian, margin_dip
// (signals), margins endpoints.
const marginRowsSQL = `
	SELECT pr.id::text, pr.name,
	       COALESCE(sum(te.minutes * rates.bill_rate / 60.0), 0)::numeric(12,2) AS billed,
	       COALESCE(sum(te.minutes * rates.cost_rate / 60.0), 0)::numeric(12,2) AS cost,
	       COALESCE(sum(CASE WHEN rates.priced = 0 THEN te.minutes ELSE 0 END), 0) AS unpriced_minutes
	FROM projects pr
	JOIN time_entries te ON te.project_id = pr.id
	     AND te.status IN ('approved', 'invoiced') AND te.deleted_at IS NULL
	JOIN memberships m ON m.user_id = te.user_id AND m.workspace_id = te.workspace_id
	JOIN LATERAL (
	    -- effective role: the person's job role first (business
	    -- truth), membership role as fallback (margins.go compat)
	    SELECT COALESCE((SELECT NULLIF(pe.role, '') FROM people pe
	              WHERE pe.user_id = te.user_id AND pe.deleted_at IS NULL
	                AND pe.workspace_id = te.workspace_id LIMIT 1), m.role) AS role
	) eff ON true
	JOIN LATERAL (
	    SELECT
	        COALESCE((SELECT pe.cost_rate FROM people pe
	                  WHERE pe.user_id = te.user_id AND pe.deleted_at IS NULL
	                    AND pe.workspace_id = te.workspace_id LIMIT 1), 0) AS cost_rate,
	        COALESCE((SELECT rcr.hourly_rate
	                  FROM rate_card_rates rcr
	                  JOIN rate_cards rc ON rc.id = rcr.rate_card_id AND rc.is_active
	                  WHERE rcr.role = eff.role AND rc.workspace_id = te.workspace_id
	                    AND rc.customer_id = pr.customer_id
	                  LIMIT 1),
	                 (SELECT rcr.hourly_rate
	                  FROM rate_card_rates rcr
	                  JOIN rate_cards rc ON rc.id = rcr.rate_card_id AND rc.is_active
	                  WHERE rcr.role = eff.role AND rc.workspace_id = te.workspace_id
	                    AND rc.customer_id IS NULL
	                  LIMIT 1), 0) AS bill_rate,
	        (SELECT count(*) FROM people pe
	         WHERE pe.user_id = te.user_id AND pe.deleted_at IS NULL
	           AND pe.workspace_id = te.workspace_id) AS priced
	) rates ON true
	WHERE pr.deleted_at IS NULL
	GROUP BY pr.id, pr.name`

type marginRow struct {
	ProjectID       string
	Name            string
	Billed          float64
	Cost            float64
	UnpricedMinutes float64
}

func marginRows(ctx context.Context, q interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) ([]marginRow, error) {
	rs, err := q.Query(ctx, marginRowsSQL)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	out := []marginRow{}
	for rs.Next() {
		var r marginRow
		if err := rs.Scan(&r.ProjectID, &r.Name, &r.Billed, &r.Cost, &r.UnpricedMinutes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rs.Err()
}

// guardian finance flags
type financeFlag struct {
	ProjectID  string  `json:"project_id"`
	Name       string  `json:"name"`
	Billed     float64 `json:"billed"`
	Cost       float64 `json:"cost"`
	MarginPct  *int    `json:"margin_pct"`
	Severity   string  `json:"severity"`
	Threshold  int     `json:"threshold"`
	Suggestion string  `json:"suggestion"`
}

type financeGuardianConfig struct {
	MarginWarnPct int     `json:"margin_warn_pct"`
	MarginRedPct  int     `json:"margin_red_pct"`
	MinBilled     float64 `json:"min_billed"`
	Enabled       bool    `json:"enabled"`
}

func defaultFinanceConfig() financeGuardianConfig {
	return financeGuardianConfig{MarginWarnPct: 30, MarginRedPct: 20, MinBilled: 0, Enabled: true}
}

func loadFinanceConfig(ctx context.Context, q interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}) financeGuardianConfig {
	cfg := defaultFinanceConfig()
	var warn, red int
	var minBilled float64
	var enabled bool
	err := q.QueryRow(ctx, `SELECT margin_warn_pct, margin_red_pct, min_billed, enabled
		FROM finance_guardian_configs
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid`).
		Scan(&warn, &red, &minBilled, &enabled)
	if err == nil {
		cfg = financeGuardianConfig{MarginWarnPct: warn, MarginRedPct: red, MinBilled: minBilled, Enabled: enabled}
	}
	return cfg
}

// getFinanceGuardian: GET /v1/agents/finance/guardian (manager+,
// kill-switched, agent_run 'guardian'). Flags, never fixes: the
// response is a read-only list; zero writes outside the agent_runs
// log itself.
func (s *Server) financeGuardian(w http.ResponseWriter, r *http.Request) {
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
	var enabled bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT agents_enabled FROM workspace_settings
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid), true)`).
		Scan(&enabled); err != nil || !enabled {
		problem(w, http.StatusServiceUnavailable, "agents disabled for this workspace (kill switch)")
		return
	}
	fcfg := loadFinanceConfig(ctx, tx)
	if !fcfg.Enabled {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "flags": []financeFlag{}})
		return
	}

	rows, err := marginRows(ctx, tx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	flags := []financeFlag{}
	for _, row := range rows {
		if row.Billed < fcfg.MinBilled || row.Billed <= 0 {
			continue
		}
		pct := int((row.Billed - row.Cost) / row.Billed * 100)
		severity := ""
		threshold := 0
		suggestion := ""
		if pct < fcfg.MarginRedPct {
			severity = "red"
			threshold = fcfg.MarginRedPct
			suggestion = "review rates and scope — margin below the red threshold"
		} else if pct < fcfg.MarginWarnPct {
			severity = "warn"
			threshold = fcfg.MarginWarnPct
			suggestion = "watch this one — margin trending under the warn threshold"
		}
		if severity == "" {
			continue
		}
		if row.UnpricedMinutes > 0 {
			suggestion += "; some time is unpriced (no rate card role match)"
		}
		flags = append(flags, financeFlag{ProjectID: row.ProjectID, Name: row.Name,
			Billed: row.Billed, Cost: row.Cost, MarginPct: &pct,
			Severity: severity, Threshold: threshold, Suggestion: suggestion})
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO agent_runs (workspace_id, agent, status, model, input_ref, output_ref, finished_at)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'guardian', 'succeeded', 'none',
		        'finance:guardian', 'finance:' || $1::int || ' flags', now())`,
		len(flags)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":    true,
		"thresholds": map[string]any{"warn_pct": fcfg.MarginWarnPct, "red_pct": fcfg.MarginRedPct, "min_billed": fcfg.MinBilled},
		"flags":      flags,
	})
}

// putFinanceConfig: PUT /v1/agents/finance/config (admin) — same
// shape as the LLM config gate.
func (s *Server) putFinanceConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var req struct {
		MarginWarnPct *int     `json:"margin_warn_pct"`
		MarginRedPct  *int     `json:"margin_red_pct"`
		MinBilled     *float64 `json:"min_billed"`
		Enabled       *bool    `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "invalid body")
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
	if !isAdminRole(ctx, tx) {
		problem(w, http.StatusForbidden, "admin role required")
		return
	}
	cur := loadFinanceConfig(ctx, tx)
	if req.MarginWarnPct != nil {
		cur.MarginWarnPct = *req.MarginWarnPct
	}
	if req.MarginRedPct != nil {
		cur.MarginRedPct = *req.MarginRedPct
	}
	if req.MinBilled != nil {
		cur.MinBilled = *req.MinBilled
	}
	if req.Enabled != nil {
		cur.Enabled = *req.Enabled
	}
	if cur.MarginWarnPct < 0 || cur.MarginWarnPct > 100 || cur.MarginRedPct < 0 || cur.MarginRedPct > 100 {
		problem(w, http.StatusBadRequest, "thresholds must be 0-100")
		return
	}
	if cur.MarginWarnPct < cur.MarginRedPct {
		problem(w, http.StatusBadRequest, "warn threshold must be >= red threshold")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO finance_guardian_configs (workspace_id, margin_warn_pct, margin_red_pct, min_billed, enabled, updated_at)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, $1, $2, $3, $4, now())
		ON CONFLICT (workspace_id) DO UPDATE
		SET margin_warn_pct = $1, margin_red_pct = $2, min_billed = $3, enabled = $4, updated_at = now()`,
		cur.MarginWarnPct, cur.MarginRedPct, cur.MinBilled, cur.Enabled); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO audit_logs (workspace_id, entity_type, entity_id, actor_type, actor_id, action, source, new_value)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'finance_guardian_config', NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'user',
		        NULLIF(current_setting('app.user_id', true), '')::uuid, 'finance.config_saved', 'ui', $1)`,
		mustJSON(map[string]any{"warn": cur.MarginWarnPct, "red": cur.MarginRedPct,
			"min_billed": cur.MinBilled, "enabled": cur.Enabled})); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"margin_warn_pct": cur.MarginWarnPct, "margin_red_pct": cur.MarginRedPct,
		"min_billed": cur.MinBilled, "enabled": cur.Enabled,
	})
}

// getFinanceConfig: GET /v1/agents/finance/config (member-readable —
// the thresholds are not secret, the UI shows them).
func (s *Server) getFinanceConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true), set_config('app.portal_token_hash', '', true)",
		workspaceFromCtx(ctx)); err != nil {
		problem(w, http.StatusInternalServerError, "internal error")
		return
	}
	cfg := loadFinanceConfig(ctx, tx)
	_ = tx.Commit(ctx)
	writeJSON(w, http.StatusOK, cfg)
}
