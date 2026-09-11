-- +goose Up
-- Required task fields + timestamped project notes (P1 §590, §516.8).
-- Enforcement lives in the task state machine (tasks.go), not a trigger:
-- the 400 must name the missing field for the UI.

-- which fields must be filled before work starts (keys fixed in v1)
ALTER TABLE tasks ADD COLUMN required_fields jsonb NOT NULL DEFAULT '[]';

-- timestamped notes feed per project (append-only)
CREATE TABLE project_notes (
    id             uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id   uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    project_id     uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    author_user_id uuid REFERENCES users(id),
    body           text NOT NULL CHECK (length(body) BETWEEN 1 AND 2000),
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_project_notes_project ON project_notes (project_id, created_at DESC);

ALTER TABLE project_notes ENABLE ROW LEVEL SECURITY;
ALTER TABLE project_notes FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON project_notes
    FOR ALL
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

-- +goose Down
DROP POLICY tenant_isolation ON project_notes;
ALTER TABLE project_notes NO FORCE ROW LEVEL SECURITY;
ALTER TABLE project_notes DISABLE ROW LEVEL SECURITY;
DROP TABLE project_notes;
ALTER TABLE tasks DROP COLUMN IF EXISTS required_fields;
