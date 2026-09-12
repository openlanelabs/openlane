// OpenLane MCP server (P2 §15, Pillar 8) — stdio JSON-RPC 2.0.
// One process serves one workspace (tenancy by process): the env pins
// OPENLANE_MCP_WORKSPACE; every tool call runs scoped + kill-switched +
// logged to agent_runs.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/openlanelabs/openlane/apps/api/internal/server"
)

func main() {
	ws := os.Getenv("OPENLANE_MCP_WORKSPACE")
	dsn := os.Getenv("DATABASE_URL")
	if ws == "" || dsn == "" {
		fmt.Fprintln(os.Stderr, "OPENLANE_MCP_WORKSPACE and DATABASE_URL required")
		os.Exit(1)
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pool:", err)
		os.Exit(1)
	}
	defer pool.Close()
	m := server.NewMcpServer(pool)

	in := bufio.NewReader(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	for {
		line, err := in.ReadString('\n')
		if line == "" && err != nil {
			return
		}
		var req map[string]any
		if json.Unmarshal([]byte(line), &req) != nil {
			continue // noise on the wire: skip
		}
		resp := m.Dispatch(ctx, ws, req)
		if resp == nil {
			continue // notification — no response per MCP
		}
		if err := enc.Encode(resp); err != nil {
			return
		}
	}
}
