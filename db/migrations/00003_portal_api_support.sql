-- +goose Up
-- Portal API support (issue #22 handlers): session resolver, link status,
-- soft-delete visibility fix, and removal of FORCE RLS.

-- 1) Session resolver: one SECURITY DEFINER call returning everything the
--    portal handlers need (project/customer/contact context). Portal
--    sessions have no workspace context and customers/contacts have no
--    portal policies — least privilege says expose exactly this tuple.
CREATE OR REPLACE FUNCTION portal_session(p_token_hash text)
RETURNS TABLE (
    workspace_id  uuid,
    project_id    uuid,
    project_name  text,
    customer_name text,
    contact_id    uuid,
    contact_name  text,
    expires_at    timestamptz
)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT pl.workspace_id, pr.id, pr.name, cu.name, ct.id, ct.display_name, pl.expires_at
    FROM portal_links pl
    JOIN projects pr ON pr.id = pl.project_id
    JOIN customers cu ON cu.id = pr.customer_id
    JOIN contacts ct ON ct.id = pl.contact_id
    WHERE p_token_hash IS NOT NULL
      AND pl.token_hash = p_token_hash
      AND pl.status = 'active'
      AND pl.expires_at > now()
      AND pl.deleted_at IS NULL
    LIMIT 1
$$;

-- 2) Link status for 401-vs-410 API semantics: 401 = never valid,
--    410 = revoked/expired (UI can offer "request a new link").
CREATE OR REPLACE FUNCTION portal_link_status(p_token_hash text)
RETURNS text
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT CASE
        WHEN pl.deleted_at IS NOT NULL THEN 'revoked'
        WHEN pl.status = 'active' AND pl.expires_at <= now() THEN 'expired'
        ELSE pl.status
    END
    FROM portal_links pl
    WHERE p_token_hash IS NOT NULL
      AND pl.token_hash = p_token_hash
    LIMIT 1
$$;

-- 3) Portal must never see soft-deleted tasks (policy gap caught while
--    writing handlers: deleted_at was not in the portal USING clause).
ALTER POLICY portal_scope ON tasks
    USING (
        project_id IN (SELECT portal_project_ids(NULLIF(current_setting('app.portal_token_hash', true), '')))
        AND customer_visible
        AND deleted_at IS NULL
    );
ALTER POLICY portal_scope_update ON tasks
    USING (
        project_id IN (SELECT portal_project_ids(NULLIF(current_setting('app.portal_token_hash', true), '')))
        AND customer_visible
        AND deleted_at IS NULL
    )
    WITH CHECK (
        project_id IN (SELECT portal_project_ids(NULLIF(current_setting('app.portal_token_hash', true), '')))
        AND customer_visible
        AND deleted_at IS NULL
    );

-- 4) Drop FORCE ROW LEVEL SECURITY: it breaks SECURITY DEFINER resolvers
--    when the table owner is not a superuser (CI/docker owners are
--    superusers, so tests pass vacuously — production would have the portal
--    silently return zero rows). The trust boundary is the openlane_app
--    role: RLS always applies to it. Owner connections are migration-only.
ALTER TABLE customers    NO FORCE ROW LEVEL SECURITY;
ALTER TABLE contacts    NO FORCE ROW LEVEL SECURITY;
ALTER TABLE projects    NO FORCE ROW LEVEL SECURITY;
ALTER TABLE tasks       NO FORCE ROW LEVEL SECURITY;
ALTER TABLE portal_links NO FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_logs  NO FORCE ROW LEVEL SECURITY;

GRANT EXECUTE ON FUNCTION portal_session(text) TO openlane_app;
GRANT EXECUTE ON FUNCTION portal_link_status(text) TO openlane_app;

-- +goose Down
DROP FUNCTION IF EXISTS portal_session(text);
DROP FUNCTION IF EXISTS portal_link_status(text);
ALTER POLICY portal_scope ON tasks
    USING (
        project_id IN (SELECT portal_project_ids(NULLIF(current_setting('app.portal_token_hash', true), '')))
        AND customer_visible
    );
ALTER POLICY portal_scope_update ON tasks
    USING (
        project_id IN (SELECT portal_project_ids(NULLIF(current_setting('app.portal_token_hash', true), '')))
        AND customer_visible
    )
    WITH CHECK (
        project_id IN (SELECT portal_project_ids(NULLIF(current_setting('app.portal_token_hash', true), '')))
        AND customer_visible
    );
ALTER TABLE customers    FORCE ROW LEVEL SECURITY;
ALTER TABLE contacts    FORCE ROW LEVEL SECURITY;
ALTER TABLE projects    FORCE ROW LEVEL SECURITY;
ALTER TABLE tasks       FORCE ROW LEVEL SECURITY;
ALTER TABLE portal_links FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_logs  FORCE ROW LEVEL SECURITY;
