-- +goose Up
-- Time entries v1 (issue #52, spec §11 "Time simple"): staff-logged
-- intervals against tasks/projects. Append-only in v1 — corrections +
-- approval states land with P1 approvals. Policies (<=12h, no future,
-- no >7d back) are enforced in the API layer; RLS is tenant-only here.

CREATE TABLE time_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    task_id UUID REFERENCES tasks(id) ON DELETE SET NULL,
    user_id UUID NOT NULL REFERENCES users(id),
    started_at TIMESTAMPTZ NOT NULL,
    ended_at TIMESTAMPTZ NOT NULL,
    minutes INT NOT NULL CHECK (minutes > 0 AND minutes <= 720),
    note TEXT,
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','submitted','approved','rejected')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ,
    CONSTRAINT time_interval_valid CHECK (ended_at > started_at)
);

CREATE INDEX time_entries_project_idx ON time_entries(project_id) WHERE deleted_at IS NULL;
CREATE INDEX time_entries_user_idx ON time_entries(user_id, started_at) WHERE deleted_at IS NULL;

ALTER TABLE time_entries ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON time_entries
    FOR ALL
    USING (
        workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
    )
    WITH CHECK (
        workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
    );

GRANT USAGE ON SCHEMA public TO openlane_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE time_entries TO openlane_app;

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation ON time_entries;
DROP TABLE IF EXISTS time_entries;
