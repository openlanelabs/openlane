-- +goose Up
-- Salesforce one-way integration settings (issue #54): tenant RLS,
-- HMAC webhook secret encrypted at rest (AES-256-GCM, key from
-- OPENLANE_INTEGRATION_KEY).
--
-- NOTE: River's own schema (river_job etc.) is created at startup by
-- River's migrator (rivermigrate.Migrate) — official pattern when the
-- app uses goose for its own migrations. Reason: River v7 adds enum
-- value 'pending' AND uses it in the same version; goose's statement
-- splitter + single-tx-per-version semantics can't express that, while
-- River's migrator runs each version in its own transaction.

CREATE TABLE workspace_integrations (
    workspace_id UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    provider TEXT NOT NULL DEFAULT 'salesforce',
    instance_url TEXT,
    webhook_secret_enc BYTEA NOT NULL,
    default_template_id UUID REFERENCES templates(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE workspace_integrations ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON workspace_integrations
    FOR ALL
    USING (
        workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
    )
    WITH CHECK (
        workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
    );

-- closed-won dedupe: one project per Salesforce opportunity, forever
CREATE TABLE integration_sync_log (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    external_id TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (provider, external_id)
);
ALTER TABLE integration_sync_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON integration_sync_log
    FOR ALL
    USING (
        workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
    )
    WITH CHECK (
        workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
    );

-- webhook secret resolver: the HMAC receiver runs machine-auth (no
-- tenant ctx), so it reads via SECURITY DEFINER — same pattern as
-- portal_session (00002). Returns ws id + encrypted secret for a slug.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION sf_webhook_secret(slug text)
    RETURNS TABLE (workspace_id uuid, webhook_secret_enc bytea)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = public
    AS $$
    SELECT w.id, i.webhook_secret_enc
    FROM workspaces w
    JOIN workspace_integrations i ON i.workspace_id = w.id
    WHERE w.slug = sf_webhook_secret.slug
    $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION sf_webhook_secret(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION sf_webhook_secret(text) TO openlane_app;

GRANT USAGE ON SCHEMA public TO openlane_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE workspace_integrations TO openlane_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE integration_sync_log TO openlane_app;

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation ON integration_sync_log;
DROP TABLE IF EXISTS integration_sync_log;
DROP POLICY IF EXISTS tenant_isolation ON workspace_integrations;
DROP TABLE IF EXISTS workspace_integrations;
