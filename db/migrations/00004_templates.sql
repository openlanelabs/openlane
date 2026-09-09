-- +goose Up
-- Templates (spec §6.4): reusable blueprints. Phases/tasks live as JSONB at
-- P0 — a template is copied into project tasks on use; live projects pin the
-- version they were created from (new-version copies never mutate old rows).

CREATE TABLE templates (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 120),
    version TEXT NOT NULL DEFAULT '1.0.0' CHECK (version ~ '^\d+\.\d+\.\d+$'),
    category TEXT NOT NULL CHECK (category IN ('onboarding','migration','implementation','support')),
    phases JSONB NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(phases) = 'array'),
    is_active BOOLEAN NOT NULL DEFAULT true,
    usage_count INTEGER NOT NULL DEFAULT 0 CHECK (usage_count >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);
CREATE INDEX idx_templates_ws_category ON templates(workspace_id, category);

-- RLS: staff-only (no portal policy — customers never read templates).
ALTER TABLE templates ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON templates
    FOR ALL
    USING (
        workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
        AND deleted_at IS NULL
    )
    WITH CHECK (
        workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
    );

GRANT USAGE ON SCHEMA public TO openlane_app;
GRANT SELECT, INSERT, UPDATE ON TABLE templates TO openlane_app;

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation ON templates;
DROP TABLE IF EXISTS templates;
