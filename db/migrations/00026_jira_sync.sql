-- +goose Up
-- Jira two-way task sync v1 (P1 §336). Link table + provider slot.
-- Conflicts: Jira wins for status (hard-coded v1 per spec).

-- allow jira in workspace_integrations
ALTER TABLE workspace_integrations DROP CONSTRAINT workspace_integrations_provider_check;
ALTER TABLE workspace_integrations ADD CONSTRAINT workspace_integrations_provider_check
    CHECK (provider IN ('salesforce','hubspot','jira'));

-- dedupe slot for ingested comments (jira comment id)
ALTER TABLE task_messages ADD COLUMN refs jsonb NOT NULL DEFAULT '{}';

-- extensible secret slot per provider (jira PAT today, OAuth tokens P1)
ALTER TABLE workspace_integrations ADD COLUMN external_refs jsonb NOT NULL DEFAULT '{}';

CREATE TABLE task_links (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    task_id       uuid NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    provider      text NOT NULL CHECK (provider IN ('jira')),
    issue_key     text NOT NULL,
    issue_id      text NOT NULL DEFAULT '',
    url           text NOT NULL DEFAULT '',
    last_synced_at timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (task_id, provider)
);
CREATE INDEX idx_task_links_issue ON task_links (workspace_id, provider, issue_key);

ALTER TABLE task_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE task_links FORCE ROW LEVEL SECURITY;
CREATE POLICY task_links_tenant ON task_links
    FOR ALL
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

-- webhook-secret resolver mirroring hs_webhook_secret
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION jira_webhook_secret(slug text)
RETURNS TABLE (workspace_id uuid, webhook_secret_enc bytea)
LANGUAGE sql SECURITY DEFINER STABLE SET search_path = public AS $$
    SELECT wi.workspace_id, wi.webhook_secret_enc
    FROM workspace_integrations wi
    JOIN workspaces w ON w.id = wi.workspace_id
    WHERE w.slug = jira_webhook_secret.slug AND wi.provider = 'jira';
$$;
-- +goose StatementEnd
GRANT EXECUTE ON FUNCTION jira_webhook_secret(text) TO openlane_app;
-- junction rows unlink with real DELETE (house grants are arw-only by
-- default; task_links has no tombstone — link state is the truth)
GRANT DELETE ON TABLE task_links TO openlane_app;

-- +goose Down
REVOKE DELETE ON TABLE task_links FROM openlane_app;
ALTER TABLE workspace_integrations DROP COLUMN IF EXISTS external_refs;
ALTER TABLE task_messages DROP COLUMN IF EXISTS refs;
DROP FUNCTION IF EXISTS jira_webhook_secret(text);
DROP POLICY task_links_tenant ON task_links;
ALTER TABLE task_links NO FORCE ROW LEVEL SECURITY;
ALTER TABLE task_links DISABLE ROW LEVEL SECURITY;
DROP TABLE task_links;
-- Down-narrowing guard (the 00022 lesson): jira rows must go first or
-- the new CHECK fails with 23514 on rows holding the extended value.
DELETE FROM workspace_integrations WHERE provider = 'jira';
ALTER TABLE workspace_integrations DROP CONSTRAINT workspace_integrations_provider_check;
ALTER TABLE workspace_integrations ADD CONSTRAINT workspace_integrations_provider_check
    CHECK (provider IN ('salesforce','hubspot'));
