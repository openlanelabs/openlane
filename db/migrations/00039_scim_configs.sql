-- SCIM 2.0 provisioning (§406 Phase2): one bearer token per
-- workspace, sealed at rest (the sso_configs pattern). Token maps a
-- SCIM caller to its tenant — no session, no workspace header.

-- +goose Up

CREATE TABLE IF NOT EXISTS scim_configs (
  workspace_id uuid PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
  token_enc    bytea NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE scim_configs ENABLE ROW LEVEL SECURITY;
CREATE POLICY scim_ws ON scim_configs
  USING (workspace_id = current_setting('app.workspace_id', true)::uuid);

-- SCIM provisioning state: memberships carry the deactivation. The
-- role stays intact (reactivation is then a no-op beyond clearing
-- deactivated_at) — role CHECK constraints are untouched.
ALTER TABLE memberships ADD COLUMN IF NOT EXISTS deactivated_at timestamptz;

-- SECURITY DEFINER resolver: SCIM runs without app.workspace_id —
-- the bearer token IS the tenant credential. Returns sealed tokens
-- for constant-time comparison in Go (secrets never leave sealed).
CREATE OR REPLACE FUNCTION scim_resolve(presented text)
RETURNS TABLE (workspace_id uuid, token_enc bytea)
LANGUAGE sql SECURITY DEFINER SET search_path = public AS $$
  SELECT c.workspace_id, c.token_enc FROM scim_configs c
$$;
REVOKE ALL ON FUNCTION scim_resolve(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION scim_resolve(text) TO openlane_app;

-- +goose Down

DROP TABLE IF EXISTS scim_configs;
DROP FUNCTION IF EXISTS scim_resolve(text);
