-- +goose Up
-- Open Nitro foundation (P2 §15): agent_runs audit rail + the
-- per-workspace kill switch. Agents log every run here — cost
-- transparency is the anti-Nitro wedge.

CREATE TABLE agent_runs (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    agent        text NOT NULL CHECK (agent IN ('doc','migration','workforce','resourcing','guardian','analyst','assistant','signals','mcp')),
    status       text NOT NULL DEFAULT 'running' CHECK (status IN ('running','succeeded','failed','needs_approval','cancelled')),
    prompt_hash  text NOT NULL DEFAULT '',
    model        text NOT NULL DEFAULT '',
    input_ref    text NOT NULL DEFAULT '',
    output_ref   text NOT NULL DEFAULT '',
    cost_cents   int  NOT NULL DEFAULT 0,
    error        text NOT NULL DEFAULT '',
    approved_by  uuid REFERENCES users(id),
    started_at   timestamptz NOT NULL DEFAULT now(),
    finished_at  timestamptz,
    created_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_agent_runs_ws ON agent_runs (workspace_id, created_at DESC);
CREATE INDEX idx_agent_runs_ws_agent ON agent_runs (workspace_id, agent, created_at DESC);

ALTER TABLE agent_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_runs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON agent_runs
    FOR ALL
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

-- §15 kill switch, per workspace (agents_enabled)
ALTER TABLE workspace_settings ADD COLUMN agents_enabled boolean NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE workspace_settings DROP COLUMN IF EXISTS agents_enabled;
DROP POLICY tenant_isolation ON agent_runs;
ALTER TABLE agent_runs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE agent_runs DISABLE ROW LEVEL SECURITY;
DROP TABLE agent_runs;
