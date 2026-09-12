-- +goose Up
-- OIDC SSO v1 (P1 §406). One IdP per workspace at v1; SAML can share
-- the table later (provider = protocol label).
CREATE TABLE sso_configs (
    id                  uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id        uuid NOT NULL UNIQUE REFERENCES workspaces(id) ON DELETE CASCADE,
    provider            text NOT NULL DEFAULT 'oidc',
    issuer              text NOT NULL CHECK (issuer LIKE 'https://%' OR issuer LIKE 'http://127.0.0.1%' OR issuer LIKE 'http://localhost%'),
    client_id           text NOT NULL,
    client_secret_enc   bytea NOT NULL,
    allowed_domains     text[] NOT NULL DEFAULT '{}',
    enforce             boolean NOT NULL DEFAULT false,
    created_at          timestamptz NOT NULL DEFAULT now(),
    updated_at          timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE sso_configs ENABLE ROW LEVEL SECURITY;
ALTER TABLE sso_configs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON sso_configs
    FOR ALL
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

-- IdP subject for identity linking (email match on first login, then sub)
ALTER TABLE users ADD COLUMN sso_subject text UNIQUE;

-- unauthenticated resolver for the login page (slug → config existence),
-- the *_webhook_secret pattern: no secrets in the output, just flags.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION sso_status_for(slug text)
RETURNS TABLE (enforce boolean, allowed_domains text[])
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT c.enforce, c.allowed_domains
    FROM sso_configs c
    JOIN workspaces w ON w.id = c.workspace_id
    WHERE w.slug = sso_status_for.slug
    LIMIT 1
$$;
-- +goose StatementEnd
GRANT EXECUTE ON FUNCTION sso_status_for(text) TO openlane_app;

-- authorize redirect needs issuer + client_id by slug (pre-auth path)
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION sso_authorize_for(slug text)
RETURNS TABLE (workspace_id uuid, issuer text, client_id text)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT c.workspace_id, c.issuer, c.client_id
    FROM sso_configs c
    JOIN workspaces w ON w.id = c.workspace_id
    WHERE w.slug = sso_authorize_for.slug
    LIMIT 1
$$;
-- +goose StatementEnd
GRANT EXECUTE ON FUNCTION sso_authorize_for(text) TO openlane_app;

-- callback needs the full config by workspace id (pre-auth, secret
-- stays in the SECURITY DEFINER boundary — never returned, used
-- server-side only... it IS the secret. This fn returns it.)
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION sso_config_by_ws(ws uuid)
RETURNS TABLE (issuer text, client_id text, client_secret_enc bytea, allowed_domains text[], enforce boolean)
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = public AS $$
    SELECT c.issuer, c.client_id, c.client_secret_enc, c.allowed_domains, c.enforce
    FROM sso_configs c
    WHERE c.workspace_id = sso_config_by_ws.ws
    LIMIT 1
$$;
-- +goose StatementEnd
GRANT EXECUTE ON FUNCTION sso_config_by_ws(uuid) TO openlane_app;

-- +goose Down
DROP FUNCTION IF EXISTS sso_config_by_ws(uuid);
DROP FUNCTION IF EXISTS sso_authorize_for(text);
DROP FUNCTION IF EXISTS sso_status_for(text);
ALTER TABLE users DROP COLUMN IF EXISTS sso_subject;
DROP POLICY tenant_isolation ON sso_configs;
ALTER TABLE sso_configs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE sso_configs DISABLE ROW LEVEL SECURITY;
DROP TABLE sso_configs;
