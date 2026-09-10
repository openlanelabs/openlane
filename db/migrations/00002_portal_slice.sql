-- +goose Up
-- +goose StatementBegin
-- P0 vertical slice (spec §19 tables, §25 #3): customers, contacts, projects,
-- tasks, portal_links, audit_logs. RLS per ADR-0002 + portal scope via
-- SECURITY DEFINER resolver so portal queries need no workspace context.

-- Customer organizations (tenant-scoped roots for external orgs).
CREATE TABLE customers (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name TEXT NOT NULL CHECK (name <> ''),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by UUID REFERENCES users(id),
    deleted_at TIMESTAMPTZ
);

-- Customer-side humans. is_portal_user gates portal link issuance.
CREATE TABLE contacts (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
    email TEXT NOT NULL CHECK (email <> ''),
    display_name TEXT NOT NULL CHECK (display_name <> ''),
    is_portal_user BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by UUID REFERENCES users(id),
    deleted_at TIMESTAMPTZ,
    UNIQUE (workspace_id, customer_id, email)
);

-- Projects (spec §6.3 subset; template link lands with templates table).
CREATE TABLE projects (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    customer_id UUID NOT NULL REFERENCES customers(id) ON DELETE RESTRICT,
    name TEXT NOT NULL CHECK (name <> ''),
    status TEXT NOT NULL DEFAULT 'draft'
        CHECK (status IN ('draft','active','on_hold','at_risk','delayed','completed','cancelled')),
    health TEXT NOT NULL DEFAULT 'unknown'
        CHECK (health IN ('green','amber','red','unknown')),
    owner_id UUID REFERENCES users(id),
    start_date DATE,
    target_go_live DATE,
    actual_go_live DATE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by UUID REFERENCES users(id),
    deleted_at TIMESTAMPTZ
);
CREATE INDEX idx_projects_ws_status ON projects(workspace_id, status);
CREATE INDEX idx_projects_ws_customer ON projects(workspace_id, customer_id);

-- Tasks (spec §8.1).
CREATE TABLE tasks (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title TEXT NOT NULL CHECK (title <> '' AND char_length(title) <= 255),
    description_md TEXT,
    owner_type TEXT NOT NULL CHECK (owner_type IN ('internal','customer','partner','agent')),
    status TEXT NOT NULL DEFAULT 'todo'
        CHECK (status IN ('todo','in_progress','blocked','review','done','waived')),
    required BOOLEAN NOT NULL DEFAULT true,
    customer_visible BOOLEAN NOT NULL DEFAULT false,
    due_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    completed_by UUID,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by UUID REFERENCES users(id),
    deleted_at TIMESTAMPTZ,
    CHECK (status <> 'done' OR completed_at IS NOT NULL)
);
CREATE INDEX idx_tasks_project ON tasks(project_id);
CREATE INDEX idx_tasks_portal ON tasks(project_id, due_at) WHERE customer_visible AND deleted_at IS NULL;

-- Portal magic links (spec §7.3, §17.2). DB stores SHA-256 of the opaque token;
-- the raw token exists only in the emailed URL. Email-bound, project+contact
-- scoped, 7-day expiry, single-table revocation.
CREATE TABLE portal_links (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    contact_id UUID NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE CHECK (char_length(token_hash) = 64),
    status TEXT NOT NULL DEFAULT 'active'
        CHECK (status IN ('active','revoked','expired')),
    expires_at TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by UUID REFERENCES users(id),
    deleted_at TIMESTAMPTZ
);
CREATE INDEX idx_portal_links_project ON portal_links(workspace_id, project_id);

-- Append-only audit trail (spec §6.3 activity + §17.11 SOC2).
CREATE TABLE audit_logs (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    entity_type TEXT NOT NULL,
    entity_id UUID NOT NULL,
    actor_type TEXT NOT NULL CHECK (actor_type IN ('user','contact','agent','system')),
    actor_id UUID,
    action TEXT NOT NULL,
    old_value JSONB,
    new_value JSONB,
    source TEXT NOT NULL CHECK (source IN ('ui','api','portal','agent','import','system')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_audit_ws_entity ON audit_logs(workspace_id, entity_type, entity_id, created_at DESC);

CREATE OR REPLACE FUNCTION portal_project_ids(p_token_hash text)
RETURNS SETOF UUID
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT pl.project_id
    FROM portal_links pl
    WHERE p_token_hash IS NOT NULL
      AND pl.token_hash = p_token_hash
      AND pl.status = 'active'
      AND pl.expires_at > now()
      AND pl.deleted_at IS NULL
$$;

-- Workspace resolver for portal audit writes: portal sessions have no
-- app.workspace_id, and a plain subquery on portal_links would itself be
-- RLS-filtered — so this runs as definer, mirroring portal_project_ids.
CREATE OR REPLACE FUNCTION portal_link_workspace(p_token_hash text)
RETURNS UUID
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT pl.workspace_id
    FROM portal_links pl
    WHERE p_token_hash IS NOT NULL
      AND pl.token_hash = p_token_hash
      AND pl.status = 'active'
      AND pl.expires_at > now()
      AND pl.deleted_at IS NULL
$$;

-- App role: migrations run as the DB owner; the API connects as openlane_app,
-- a non-owner role that is subject to RLS (superusers/owners bypass it silently).
-- Password is set by the operator (docker compose / CI bootstrap), never here.
DO $$ BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'openlane_app') THEN
    CREATE ROLE openlane_app LOGIN;
  END IF;
END $$;
GRANT USAGE ON SCHEMA public TO openlane_app;
GRANT SELECT, INSERT, UPDATE ON ALL TABLES IN SCHEMA public TO openlane_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE ON TABLES TO openlane_app;
GRANT EXECUTE ON FUNCTION portal_project_ids(text) TO openlane_app;
GRANT EXECUTE ON FUNCTION portal_link_workspace(text) TO openlane_app;

-- Portal scope resolver: token -> project IDs the token may touch.
-- SECURITY DEFINER: portal sessions run without app.workspace_id, and this
-- function is the ONLY path that lets them see rows (RLS policies below).
-- ponytail: revocation = status/expiry check; no per-token ACL rows until needed.
-- ---- RLS ----
ALTER TABLE customers  ENABLE ROW LEVEL SECURITY;
ALTER TABLE contacts  ENABLE ROW LEVEL SECURITY;
ALTER TABLE projects  ENABLE ROW LEVEL SECURITY;
ALTER TABLE tasks     ENABLE ROW LEVEL SECURITY;
ALTER TABLE portal_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_logs ENABLE ROW LEVEL SECURITY;
-- Belt and braces: even the table owner is subject to RLS on these.
-- (Superusers still bypass; that is why the API uses openlane_app, not the owner.)
ALTER TABLE customers  FORCE ROW LEVEL SECURITY;
ALTER TABLE contacts  FORCE ROW LEVEL SECURITY;
ALTER TABLE projects  FORCE ROW LEVEL SECURITY;
ALTER TABLE tasks     FORCE ROW LEVEL SECURITY;
ALTER TABLE portal_links FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_logs FORCE ROW LEVEL SECURITY;

-- Tenant policy for internal access: API sets app.workspace_id per tx.
-- (set_config('app.workspace_id', ...) — SET LOCAL can't take expressions)
CREATE POLICY tenant_isolation ON customers
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON contacts
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON projects
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON tasks
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON portal_links
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON audit_logs
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

-- Portal policies: no workspace context, scope = the token's project only.
-- audit_logs portal policy: portal actors can append (INSERT) for their
-- project's workspace; writes are append-only (no SELECT/UPDATE/DELETE).
-- Portal policies: no workspace context, scope = the token's project only,
-- and NEVER internal rows (spec §7.3-G3: never leak internal).
CREATE POLICY portal_scope ON tasks
    USING (
        project_id IN (SELECT portal_project_ids(NULLIF(current_setting('app.portal_token_hash', true), '')))
        AND customer_visible
    );
-- audit_logs portal policy: portal actors can append (INSERT) for their
-- project's workspace; writes are append-only (no SELECT/UPDATE/DELETE).
CREATE POLICY portal_scope_insert ON audit_logs
    FOR INSERT
    WITH CHECK (workspace_id = portal_link_workspace(NULLIF(current_setting('app.portal_token_hash', true), '')));
CREATE POLICY portal_scope ON projects
    USING (id IN (SELECT portal_project_ids(NULLIF(current_setting('app.portal_token_hash', true), ''))));

-- Allow the portal to complete customer-visible tasks in its project (§7.3 "3-click").
-- Tighter than tenant policy: token's project + customer-visible rows only,
-- and only the completion columns may change (WITH CHECK re-asserts scope).
CREATE POLICY portal_scope_update ON tasks FOR UPDATE
    USING (
        project_id IN (SELECT portal_project_ids(NULLIF(current_setting('app.portal_token_hash', true), '')))
        AND customer_visible
    )
    WITH CHECK (
        project_id IN (SELECT portal_project_ids(NULLIF(current_setting('app.portal_token_hash', true), '')))
        AND customer_visible
    );
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP POLICY IF EXISTS portal_scope_update ON tasks;
DROP POLICY IF EXISTS portal_scope ON projects;
DROP POLICY IF EXISTS portal_scope_insert ON audit_logs;
DROP POLICY IF EXISTS portal_scope ON tasks;
DROP POLICY IF EXISTS tenant_isolation ON audit_logs;
DROP POLICY IF EXISTS tenant_isolation ON portal_links;
DROP POLICY IF EXISTS tenant_isolation ON tasks;
DROP POLICY IF EXISTS tenant_isolation ON projects;
DROP POLICY IF EXISTS tenant_isolation ON contacts;
DROP POLICY IF EXISTS tenant_isolation ON customers;
DROP FUNCTION IF EXISTS portal_link_workspace(text);
DROP FUNCTION IF EXISTS portal_project_ids(text);
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS portal_links;
DROP TABLE IF EXISTS tasks CASCADE;
DROP TABLE IF EXISTS projects;
DROP TABLE IF EXISTS contacts;
DROP TABLE IF EXISTS customers;
-- +goose StatementEnd
