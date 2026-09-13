-- Portal white-label theming (§199/§499): one row per workspace.
-- logo_url + brand_color + no_branding (hide OpenLane wordmark).
-- Portal reads go through workspace_theme_for() (SECURITY DEFINER)
-- because portal sessions carry no workspace ctx — the same pattern
-- as portal_link_status.

-- +goose Up

CREATE TABLE workspace_themes (
    workspace_id uuid PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    logo_url     text,
    brand_color  text,
    no_branding  boolean NOT NULL DEFAULT false,
    updated_at   timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE workspace_themes ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_themes FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON workspace_themes
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE ON workspace_themes TO openlane_app;

-- Portal-side resolution (no workspace ctx): theme for a workspace id.
CREATE OR REPLACE FUNCTION workspace_theme_for(ws uuid)
RETURNS jsonb LANGUAGE sql SECURITY DEFINER STABLE SET search_path = public
AS $$ SELECT to_jsonb(t) - 'workspace_id' - 'updated_at' FROM workspace_themes t
 WHERE t.workspace_id = ws AND (t.logo_url IS NOT NULL OR t.brand_color IS NOT NULL OR t.no_branding) $$;

GRANT EXECUTE ON FUNCTION workspace_theme_for(uuid) TO openlane_app;

-- Public host routing (§229): verified domain -> workspace + theme.
-- SECURITY DEFINER: cross-tenant by design (a routing table, not tenant data).
CREATE OR REPLACE FUNCTION workspace_for_host(d text)
RETURNS jsonb LANGUAGE sql SECURITY DEFINER STABLE SET search_path = public
AS $$ SELECT to_jsonb(w) FROM (SELECT wd.workspace_id::text AS workspace_id, COALESCE(workspace_theme_for(wd.workspace_id), 'null'::jsonb) AS theme FROM workspace_domains wd WHERE wd.domain = lower(d) AND wd.verified) w $$;

GRANT EXECUTE ON FUNCTION workspace_for_host(text) TO openlane_app;

-- +goose Down

DROP FUNCTION IF EXISTS workspace_theme_for(uuid);
DROP TABLE IF EXISTS workspace_themes;
