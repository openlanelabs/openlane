-- +goose Up
-- Auth core (issue #39, spec §16/§26): magic-link login tokens + refresh
-- sessions. Both tables are SYSTEM-managed: rows are validated by opaque
-- SHA-256 hashes at exact-lookup, never scoped by client context, so RLS
-- NONE (the API role gets INSERT/SELECT/UPDATE; no DELETE — revocation is
-- a timestamp, keeping forensic history).

CREATE TABLE login_tokens (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    email TEXT NOT NULL,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE CHECK (char_length(token_hash) = 64),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ,
    created_ip TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_login_tokens_email ON login_tokens(email, created_at DESC);

CREATE TABLE sessions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    refresh_hash TEXT NOT NULL UNIQUE CHECK (char_length(refresh_hash) = 64),
    family_id UUID NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_sessions_family ON sessions(family_id);
CREATE INDEX idx_sessions_user_ws ON sessions(user_id, workspace_id);

-- Deliberately NO RLS on these system tables (unlike tenant data tables):
-- lookups are always by exact opaque hash (single row, no tenant predicates),
-- and a no-policy RLS table would be deny-all dead weight. Grants restrict
-- access to the app role; rows carry no tenant-readable data beyond hashes.
-- ADR-0002's "RLS on every table" governs tenant data; these are system
-- plumbing like goose_db_version. Revisit if sessions ever become listable.

GRANT USAGE ON SCHEMA public TO openlane_app;
GRANT SELECT, INSERT, UPDATE ON TABLE login_tokens TO openlane_app;
GRANT SELECT, INSERT, UPDATE ON TABLE sessions TO openlane_app;

-- Cross-workspace membership listing (workspace picker): memberships is
-- RLS'd per-workspace, so a user's own list needs a definer escape hatch
-- scoped to exactly one user_id.
-- +goose StatementBegin
CREATE FUNCTION user_memberships(p_user_id uuid)
RETURNS TABLE (workspace_id uuid, name text, slug text, role text)
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = public
AS $$
    SELECT w.id, w.name, w.slug, m.role
    FROM memberships m JOIN workspaces w ON w.id = m.workspace_id
    WHERE m.user_id = p_user_id
    ORDER BY w.name;
$$;
-- +goose StatementEnd
GRANT EXECUTE ON FUNCTION user_memberships(uuid) TO openlane_app;

-- +goose Down
DROP FUNCTION IF EXISTS user_memberships(uuid);
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS login_tokens;
