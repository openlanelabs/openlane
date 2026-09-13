-- Config Agent v1 (P2 §15.3): the order-form → setup-sheet → approve
-- → execute flow. The sheet is the artifact of record (what the LLM
-- proposed, post-validation); executed_refs links the created rows.

-- +goose Up

CREATE TABLE config_sheets (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id     uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    project_id       uuid NOT NULL REFERENCES projects(id),
    form_response_id uuid,
    sheet            jsonb NOT NULL,
    status           text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'executed', 'failed')),
    executed_refs    jsonb,
    error            text,
    created_by       uuid,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX config_sheets_ws_idx ON config_sheets (workspace_id, created_at DESC);

ALTER TABLE config_sheets ENABLE ROW LEVEL SECURITY;
ALTER TABLE config_sheets FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON config_sheets
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE ON config_sheets TO openlane_app;

-- +goose Down

DROP TABLE IF EXISTS config_sheets;
