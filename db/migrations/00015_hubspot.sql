-- HubSpot one-way (issue #66) + fixes latent #55 bug: the SF worker's
-- template branch referenced projects.description/templates.description
-- which never existed — it only ever passed tests via the no-template
-- branch. Adds those columns, plus external_refs (§6.3) so created
-- projects record their CRM origin.

-- +goose Up
ALTER TABLE projects
    ADD COLUMN description TEXT,
    ADD COLUMN external_refs JSONB NOT NULL DEFAULT '{}'::jsonb
        CHECK (jsonb_typeof(external_refs) = 'object');

ALTER TABLE templates
    ADD COLUMN description TEXT;

-- multi-provider integrations: PK (workspace_id, provider) so SF and
-- HubSpot settings coexist. Existing single-provider rows survive.
ALTER TABLE workspace_integrations DROP CONSTRAINT workspace_integrations_pkey;
ALTER TABLE workspace_integrations
    ADD PRIMARY KEY (workspace_id, provider);
ALTER TABLE workspace_integrations
    ADD CONSTRAINT workspace_integrations_provider_check
    CHECK (provider IN ('salesforce','hubspot'));

-- webhook secret resolver for hubspot rows (sf_webhook_secret shape,
-- 00010 — same machine-auth SECURITY DEFINER pattern)
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION hs_webhook_secret(slug text)
    RETURNS TABLE (workspace_id uuid, webhook_secret_enc bytea)
    LANGUAGE sql STABLE SECURITY DEFINER
    SET search_path = public
    AS $$
    SELECT w.id, i.webhook_secret_enc
    FROM workspaces w
    JOIN workspace_integrations i ON i.workspace_id = w.id
    WHERE w.slug = hs_webhook_secret.slug
      AND i.provider = 'hubspot'
    $$;
-- +goose StatementEnd
REVOKE ALL ON FUNCTION hs_webhook_secret(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION hs_webhook_secret(text) TO openlane_app;

-- +goose Down
DROP FUNCTION IF EXISTS hs_webhook_secret(text);
ALTER TABLE workspace_integrations DROP CONSTRAINT IF EXISTS workspace_integrations_provider_check;
ALTER TABLE workspace_integrations DROP CONSTRAINT workspace_integrations_pkey;
-- back to one provider per workspace: drop hubspot rows, then the
-- workspace_id-only PK is unique again
DELETE FROM workspace_integrations WHERE provider = 'hubspot';
ALTER TABLE workspace_integrations ADD PRIMARY KEY (workspace_id);
ALTER TABLE templates DROP COLUMN IF EXISTS description;
ALTER TABLE projects DROP COLUMN IF EXISTS external_refs;
ALTER TABLE projects DROP COLUMN IF EXISTS description;
