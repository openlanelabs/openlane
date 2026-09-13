-- Finance Guardian v1 (P2 §15.5): configurable margin thresholds
-- per workspace, one row. The guardian READS and flags; it never
-- writes money — there is no remediation path by design.

-- +goose Up

CREATE TABLE finance_guardian_configs (
    workspace_id   uuid PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    margin_warn_pct integer NOT NULL DEFAULT 30 CHECK (margin_warn_pct BETWEEN 0 AND 100),
    margin_red_pct  integer NOT NULL DEFAULT 20 CHECK (margin_red_pct BETWEEN 0 AND 100),
    min_billed      numeric(12,2) NOT NULL DEFAULT 0 CHECK (min_billed >= 0),
    enabled         boolean NOT NULL DEFAULT true,
    updated_at     timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE finance_guardian_configs ENABLE ROW LEVEL SECURITY;
ALTER TABLE finance_guardian_configs FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON finance_guardian_configs
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE ON finance_guardian_configs TO openlane_app;

-- +goose Down

DROP TABLE IF EXISTS finance_guardian_configs;
