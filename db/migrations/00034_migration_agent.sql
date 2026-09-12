-- Migration Agent v1 (P2 §15.2) — one table for the whole
-- suggest→preview→approve→run flow. csv_text is the source of record;
-- result holds the transformed dataset (version 1); quarantine holds
-- bad rows + reasons (partial-fail edge). Rollback = DELETE.

-- +goose Up

CREATE TABLE migration_runs (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name         text NOT NULL,
    dest         text NOT NULL CHECK (dest IN ('salesforce_accounts', 'hubspot_contacts', 'generic')),
    status       text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'approved', 'done', 'failed')),
    csv_text     text NOT NULL,
    mapping      jsonb,
    plain_english text,
    preview      jsonb,
    result       jsonb,
    quarantine   jsonb,
    stats        jsonb,
    error        text,
    created_by   uuid,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX migration_runs_ws_idx ON migration_runs (workspace_id, created_at DESC);

ALTER TABLE migration_runs ENABLE ROW LEVEL SECURITY;
ALTER TABLE migration_runs FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON migration_runs
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON migration_runs TO openlane_app;

-- +goose Down

DROP TABLE IF EXISTS migration_runs;
