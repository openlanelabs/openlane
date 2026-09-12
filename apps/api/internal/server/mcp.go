package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// MCP v1 (P2 §15 + Pillar 8): stdio JSON-RPC 2.0 server exposing
// read-only workspace surface. Every call gates on the workspace kill
// switch (agents_enabled) and logs an agent_runs row — the audit rail
// from day one. Write-capable tools come with the Guardian approval
// flow; v1 is the read surface the config/migration agents need.

// McpTool: name, description (shown to the calling LLM), and the call.
type McpTool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Call        func(ctx context.Context, ws string, args map[string]any) (any, error)
}

// McpServer: bound to a pool + a fixed workspace (env-configured at
// startup; one MCP process serves one workspace — tenancy by process).
type McpServer struct {
	pool *pgxpool.Pool
}

// NewMcpServer: pool from the same New() the API uses.
func NewMcpServer(pool *pgxpool.Pool) *McpServer {
	return &McpServer{pool: pool}
}

// mcpScoped: one tx, workspace scope set — the house rule.
func (m *McpServer) mcpScoped(ctx context.Context, ws string) (pgx.Tx, error) {
	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", ws); err != nil {
		_ = tx.Rollback(ctx)
		return nil, err
	}
	return tx, nil
}

// mcpKillSwitch: §15 per-workspace kill switch.
func (m *McpServer) mcpEnabled(ctx context.Context, ws string) (bool, error) {
	tx, err := m.mcpScoped(ctx, ws)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var enabled bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(agents_enabled, true) FROM workspace_settings
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid`).
		Scan(&enabled); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return true, nil // no settings row = default on
		}
		return false, err
	}
	return enabled, nil
}

// logAgentRun: the audit rail — one row per tool call.
func (m *McpServer) logAgentRun(ctx context.Context, ws, tool string, args map[string]any, status, errMsg string) {
	tx, err := m.mcpScoped(ctx, ws)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mcp: agent_runs log failed: %v\n", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	h := sha256.Sum256([]byte(tool + fmt.Sprint(args)))
	promptHash := hex.EncodeToString(h[:8])
	_, _ = tx.Exec(ctx, `
		INSERT INTO agent_runs (workspace_id, agent, status, prompt_hash, model, input_ref, output_ref, error, finished_at)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid, 'mcp', $1, $2, 'none', $3, $4, $5, now())`,
		status, promptHash, tool, "", errMsg)
	_ = tx.Commit(ctx)
}

// Tools: the v1 read surface.
func (m *McpServer) Tools() []McpTool {
	return []McpTool{
		{
			Name:        "list_projects",
			Description: "List the workspace's projects: id, name, status, health, progress %, target go-live.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Call: func(ctx context.Context, ws string, _ map[string]any) (any, error) {
				tx, err := m.mcpScoped(ctx, ws)
				if err != nil {
					return nil, err
				}
				defer func() { _ = tx.Rollback(ctx) }()
				rows, err := tx.Query(ctx, `
					SELECT id::text, name, status, health,
					       CASE WHEN total = 0 THEN 0 ELSE round(100.0 * done / total)::int END AS progress
					FROM (
						SELECT p.id, p.name, p.status, p.health,
						       count(t.id) AS total,
						       count(t.id) FILTER (WHERE t.status IN ('done','waived')) AS done
						FROM projects p
						LEFT JOIN tasks t ON t.project_id = p.id AND t.deleted_at IS NULL
						WHERE p.deleted_at IS NULL
						GROUP BY p.id, p.name, p.status, p.health
						ORDER BY p.created_at DESC LIMIT 100
					) x`)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				out := []map[string]any{}
				for rows.Next() {
					var id, name, status string
					var health string
					var progress int
					if err := rows.Scan(&id, &name, &status, &health, &progress); err != nil {
						return nil, err
					}
					out = append(out, map[string]any{"id": id, "name": name, "status": status, "health": health, "progress_pct": progress})
				}
				return out, rows.Err()
			},
		},
		{
			Name:        "list_tasks",
			Description: "List tasks of a project, optionally filtered by status (todo|in_progress|review|blocked|done|waived).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"project_id": map[string]any{"type": "string"},
					"status":     map[string]any{"type": "string"},
				},
				"required": []string{"project_id"},
			},
			Call: func(ctx context.Context, ws string, args map[string]any) (any, error) {
				pid, _ := args["project_id"].(string)
				if pid == "" {
					return nil, errors.New("project_id required")
				}
				tx, err := m.mcpScoped(ctx, ws)
				if err != nil {
					return nil, err
				}
				defer func() { _ = tx.Rollback(ctx) }()
				q := `SELECT id::text, title, status, due_at::text, required_fields
				      FROM tasks WHERE project_id = $1::uuid AND deleted_at IS NULL`
				qargs := []any{pid}
				if st, ok := args["status"].(string); ok && st != "" {
					q += ` AND status = $2`
					qargs = append(qargs, st)
				}
				q += ` ORDER BY created_at LIMIT 200`
				rows, err := tx.Query(ctx, q, qargs...)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				out := []map[string]any{}
				for rows.Next() {
					var id, title, status string
					var due *string
					var reqFields []byte
					if err := rows.Scan(&id, &title, &status, &due, &reqFields); err != nil {
						return nil, err
					}
					item := map[string]any{"id": id, "title": title, "status": status, "due_at": due}
					var rf []string
					_ = json.Unmarshal(reqFields, &rf)
					item["required_fields"] = rf
					out = append(out, item)
				}
				return out, rows.Err()
			},
		},
		{
			Name:        "list_people",
			Description: "List people with role, weekly capacity hours, skills, and active flag.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Call: func(ctx context.Context, ws string, _ map[string]any) (any, error) {
				tx, err := m.mcpScoped(ctx, ws)
				if err != nil {
					return nil, err
				}
				defer func() { _ = tx.Rollback(ctx) }()
				rows, err := tx.Query(ctx, `
					SELECT id::text, name, role, skills, capacity_hrs::text, active
					FROM people WHERE deleted_at IS NULL ORDER BY name LIMIT 200`)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				out := []map[string]any{}
				for rows.Next() {
					var id, name, role, cap string
					var skills []string
					var active bool
					if err := rows.Scan(&id, &name, &role, &skills, &cap, &active); err != nil {
						return nil, err
					}
					out = append(out, map[string]any{"id": id, "name": name, "role": role, "skills": skills, "capacity_hrs": cap, "active": active})
				}
				return out, rows.Err()
			},
		},
		{
			Name:        "utilization",
			Description: "One person's current utilization: minutes logged this week vs weekly capacity.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"person_id": map[string]any{"type": "string"}},
				"required":   []string{"person_id"},
			},
			Call: func(ctx context.Context, ws string, args map[string]any) (any, error) {
				pid, _ := args["person_id"].(string)
				if pid == "" {
					return nil, errors.New("person_id required")
				}
				tx, err := m.mcpScoped(ctx, ws)
				if err != nil {
					return nil, err
				}
				defer func() { _ = tx.Rollback(ctx) }()
				var out map[string]any
				err = tx.QueryRow(ctx, `
					SELECT jsonb_build_object(
					  'name', p.name,
					  'capacity_hrs', p.capacity_hrs::int,
					  'minutes_this_week', COALESCE((
					    SELECT sum(te.minutes) FROM time_entries te
					    WHERE te.workspace_id = p.workspace_id
					      AND te.user_id = p.user_id
					      AND te.created_at >= date_trunc('week', now())), 0),
					  'utilization_pct', CASE WHEN p.capacity_hrs > 0 AND p.user_id IS NOT NULL THEN
					    round(100.0 * COALESCE((
					      SELECT sum(te.minutes) FROM time_entries te
					      WHERE te.workspace_id = p.workspace_id
					        AND te.user_id = p.user_id
					        AND te.created_at >= date_trunc('week', now()), 0) / (60.0 * p.capacity_hrs))
					  END)
					)::text
					FROM people p WHERE p.id = $1::uuid AND p.deleted_at IS NULL`, pid).Scan(&out)
				if errors.Is(err, pgx.ErrNoRows) {
					return nil, errors.New("person not found")
				}
				var parsed map[string]any
				_ = json.Unmarshal([]byte(fmt.Sprint(out)), &parsed)
				return parsed, err
			},
		},
		{
			Name:        "portfolio_margins",
			Description: "Billed vs cost per project + workspace totals (approved+invoiced time only).",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Call: func(ctx context.Context, ws string, _ map[string]any) (any, error) {
				tx, err := m.mcpScoped(ctx, ws)
				if err != nil {
					return nil, err
				}
				defer func() { _ = tx.Rollback(ctx) }()
				rows, err := tx.Query(ctx, `
					SELECT pr.id::text, pr.name,
					       round(sum(EXTRACT(EPOCH FROM (te.ended_at - te.started_at))/3600 * COALESCE(pp.cost_rate, 0))::numeric, 2)::text AS cost,
					       round(sum(EXTRACT(EPOCH FROM (te.ended_at - te.started_at))/3600 * rc.hourly_rate)::numeric, 2)::text AS billed
					FROM projects pr
					JOIN time_entries te ON te.project_id = pr.id
					     AND te.status IN ('approved','invoiced') AND te.deleted_at IS NULL
					LEFT JOIN people pp ON pp.user_id = te.user_id AND pp.workspace_id = te.workspace_id
					LEFT JOIN rate_cards rc ON rc.workspace_id = te.workspace_id
					     AND rc.role = COALESCE(pp.role, '') AND rc.is_active
					     AND (rc.customer_id IS NULL OR rc.customer_id = pr.customer_id)
					WHERE pr.deleted_at IS NULL
					GROUP BY pr.id, pr.name
					HAVING sum(EXTRACT(EPOCH FROM (te.ended_at - te.started_at))/3600 * rc.hourly_rate) > 0
					ORDER BY 4 DESC LIMIT 100`)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				out := []map[string]any{}
				for rows.Next() {
					var id, name, cost, billed string
					if err := rows.Scan(&id, &name, &cost, &billed); err != nil {
						return nil, err
					}
					out = append(out, map[string]any{"project_id": id, "name": name, "cost": cost, "billed": billed})
				}
				return out, rows.Err()
			},
		},
		{
			Name:        "pending_time",
			Description: "The time-approval queue: submitted entries awaiting a manager decision.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
			Call: func(ctx context.Context, ws string, _ map[string]any) (any, error) {
				tx, err := m.mcpScoped(ctx, ws)
				if err != nil {
					return nil, err
				}
				defer func() { _ = tx.Rollback(ctx) }()
				rows, err := tx.Query(ctx, `
					SELECT te.id::text, u.display_name, te.minutes, te.started_at::text, te.note
					FROM time_entries te
					JOIN users u ON u.id = te.user_id
					WHERE te.status = 'submitted' AND te.deleted_at IS NULL
					ORDER BY te.started_at LIMIT 100`)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				out := []map[string]any{}
				for rows.Next() {
					var id, who, note string
					var minutes int
					var started string
					if err := rows.Scan(&id, &who, &minutes, &started, &note); err != nil {
						return nil, err
					}
					out = append(out, map[string]any{"id": id, "who": who, "minutes": minutes, "started_at": started, "note": note})
				}
				return out, rows.Err()
			},
		},
		{
			Name:        "search",
			Description: "Search projects, tasks, docs, and files by name/title substring. Returns kind + id + title.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"q": map[string]any{"type": "string"}},
				"required":   []string{"q"},
			},
			Call: func(ctx context.Context, ws string, args map[string]any) (any, error) {
				q, _ := args["q"].(string)
				if strings.TrimSpace(q) == "" {
					return nil, errors.New("q required")
				}
				tx, err := m.mcpScoped(ctx, ws)
				if err != nil {
					return nil, err
				}
				defer func() { _ = tx.Rollback(ctx) }()
				rows, err := tx.Query(ctx, `
					SELECT 'project' AS kind, id::text, name AS title FROM projects
					  WHERE deleted_at IS NULL AND name ILIKE '%' || $1 || '%'
					UNION ALL
					SELECT 'task', id::text, title FROM tasks
					  WHERE deleted_at IS NULL AND title ILIKE '%' || $1 || '%'
					UNION ALL
					SELECT 'doc', id::text, title FROM docs
					  WHERE deleted_at IS NULL AND title ILIKE '%' || $1 || '%'
					UNION ALL
					SELECT 'file', id::text, name FROM files
					  WHERE deleted_at IS NULL AND name ILIKE '%' || $1 || '%'
					LIMIT 50`, q)
				if err != nil {
					return nil, err
				}
				defer rows.Close()
				out := []map[string]any{}
				for rows.Next() {
					var kind, id, title string
					if err := rows.Scan(&kind, &id, &title); err != nil {
						return nil, err
					}
					out = append(out, map[string]any{"kind": kind, "id": id, "title": title})
				}
				return out, rows.Err()
			},
		},
	}
}

// Dispatch: one JSON-RPC request → response. The stdio loop lives in
// cmd/mcp; this is testable core.
func (m *McpServer) Dispatch(ctx context.Context, ws string, req map[string]any) map[string]any {
	id := req["id"]
	method, _ := req["method"].(string)
	resp := map[string]any{"jsonrpc": "2.0", "id": id}

	switch method {
	case "initialize":
		resp["result"] = map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "openlane", "version": "0.1.0"},
		}
	case "tools/list":
		tools := []map[string]any{}
		for _, t := range m.Tools() {
			tools = append(tools, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			})
		}
		resp["result"] = map[string]any{"tools": tools}
	case "tools/call":
		params, _ := req["params"].(map[string]any)
		name, _ := params["name"].(string)
		toolArgs, _ := params["arguments"].(map[string]any)
		if toolArgs == nil {
			toolArgs = map[string]any{}
		}
		// kill switch
		enabled, err := m.mcpEnabled(ctx, ws)
		if err != nil {
			resp["error"] = map[string]any{"code": -32603, "message": "internal error"}
			return resp
		}
		if !enabled {
			resp["error"] = map[string]any{"code": -32603, "message": "agents disabled for this workspace (kill switch)"}
			return resp
		}
		var found *McpTool
		for _, t := range m.Tools() {
			if t.Name == name {
				found = &t
			}
		}
		if found == nil {
			resp["error"] = map[string]any{"code": -32602, "message": "unknown tool " + name}
			return resp
		}
		out, err := found.Call(ctx, ws, toolArgs)
		status, errMsg := "succeeded", ""
		if err != nil {
			status, errMsg = "failed", err.Error()
		}
		m.logAgentRun(ctx, ws, name, toolArgs, status, errMsg)
		if err != nil {
			resp["error"] = map[string]any{"code": -32603, "message": errMsg}
			return resp
		}
		b, _ := json.Marshal(out)
		resp["result"] = map[string]any{
			"content": []map[string]any{{"type": "text", "text": string(b)}},
		}
	default:
		if id == nil {
			// notification (initialized etc.) — no response
			return nil
		}
		resp["error"] = map[string]any{"code": -32601, "message": "method not found"}
	}
	return resp
}
