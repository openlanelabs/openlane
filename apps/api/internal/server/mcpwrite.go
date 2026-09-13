package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// MCP write tools (P2 §347/§537) — the Guardian approval flow: write
// tools NEVER write directly. They create pending approvals carrying
// a JSON payload; a human approves via POST /v1/approvals/{id}/decide
// which executes the payload inside the decide tx. Agents propose,
// humans commit — the same contract as every other surface.

// proposeApproval: shared inserter for agent-proposed approvals.
// Duplicate-pending guard: an identical payload already pending →
// error (no second row).
func proposeApproval(ctx context.Context, m *McpServer, ws string, title, kind string, payload map[string]any) (any, error) {
	tx, err := m.mcpScoped(ctx, ws)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// duplicate pending w/ same title in this workspace?
	var dup string
	err = tx.QueryRow(ctx, `SELECT id::text FROM approvals
		WHERE workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
		  AND title = $1 AND status = 'pending' AND deleted_at IS NULL LIMIT 1`, title).Scan(&dup)
	if err == nil {
		return nil, fmt.Errorf("an identical proposal is already pending (approval %s) — wait for the human decision", dup)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	var id string
	if err := tx.QueryRow(ctx, `
		INSERT INTO approvals (workspace_id, project_id, title, description, status)
		VALUES (NULLIF(current_setting('app.workspace_id', true), '')::uuid,
		        $1::uuid, $2, $3, 'pending')
		RETURNING id`,
		payload["project_id"], title,
		"agent-proposal:"+kind+":"+string(mustJSON(payload))).Scan(&id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return map[string]any{
		"approval_id": id,
		"status":      "pending human approval",
		"note":        "Nothing was written. A manager must approve at Approvals → " + title,
	}, nil
}

// mcpWriteTools: the two v1 write tools. Appended to the read surface
// by Tools() — write capability is still behind the workspace kill
// switch (mcpEnabled gates ALL tool calls).
func mcpWriteTools(m *McpServer) []McpTool {
	return []McpTool{
		{
			Name:        "propose_task",
			Description: "Propose a new task on a project. CREATES A PENDING APPROVAL — nothing is written until a human approves it in the UI.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"project_id":  map[string]any{"type": "string", "description": "uuid of the project"},
					"title":       map[string]any{"type": "string", "description": "task title, 1-200 chars"},
					"description": map[string]any{"type": "string", "description": "optional detail"},
				},
				"required": []string{"project_id", "title"},
			},
			Call: func(ctx context.Context, ws string, args map[string]any) (any, error) {
				title, _ := args["title"].(string)
				title = strings.TrimSpace(title)
				if len(title) < 1 || len(title) > 200 {
					return nil, fmt.Errorf("title must be 1-200 chars")
				}
				projID, _ := args["project_id"].(string)
				if len(projID) != 36 {
					return nil, fmt.Errorf("project_id must be a uuid")
				}
				desc, _ := args["description"].(string)
				if len(desc) > 2000 {
					desc = desc[:2000]
				}
				return proposeApproval(ctx, m, ws, "Task: "+title, "task", map[string]any{
					"project_id": projID, "title": title, "description": desc,
				})
			},
		},
		{
			Name:        "propose_time",
			Description: "Propose logging time on a project (creates a pending approval; a human approval logs the entry).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"project_id": map[string]any{"type": "string", "description": "uuid of the project"},
					"minutes":    map[string]any{"type": "number", "description": "1-720 (§308 max day)"},
					"note":       map[string]any{"type": "string", "description": "optional note"},
				},
				"required": []string{"project_id", "minutes"},
			},
			Call: func(ctx context.Context, ws string, args map[string]any) (any, error) {
				projID, _ := args["project_id"].(string)
				if len(projID) != 36 {
					return nil, fmt.Errorf("project_id must be a uuid")
				}
				var mins int
				switch mv := args["minutes"].(type) {
				case float64:
					mins = int(mv)
				case int:
					mins = mv
				case json.Number:
					mins, _ = strconv.Atoi(mv.String())
				}
				if mins < 1 || mins > 720 {
					return nil, fmt.Errorf("minutes must be 1-720")
				}
				note, _ := args["note"].(string)
				if len(note) > 500 {
					note = note[:500]
				}
				return proposeApproval(ctx, m, ws,
					fmt.Sprintf("Time: %dm on project", mins), "time", map[string]any{
						"project_id": projID, "minutes": mins, "note": note,
					})
			},
		},
	}
}
