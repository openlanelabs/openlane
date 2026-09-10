-- +goose Up
-- Project files (issue #50, spec §9 Pillar-4 slice): metadata rows only —
-- bytes live in VaultS3 behind presigned URLs. Client never chooses the
-- object key (server-generated {ws}/{project}/{uuid}) so path traversal is
-- structurally impossible. Status: pending -> uploaded (HEAD-verified).

CREATE TABLE files (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    object_key TEXT NOT NULL UNIQUE,
    content_type TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','uploaded','failed')),
    customer_visible BOOLEAN NOT NULL DEFAULT false,
    sha256 TEXT,
    created_by UUID REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);

CREATE INDEX files_project_idx ON files(project_id) WHERE deleted_at IS NULL;

ALTER TABLE files ENABLE ROW LEVEL SECURITY;

-- Staff ctx: workspace match. Portal ctx: the link's customer owns the
-- project AND the file is customer_visible.
CREATE POLICY tenant_isolation ON files
    FOR ALL
    USING (
        workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
        OR (
            -- portal reader: link (resolver-checked: active+unexpired) is
            -- scoped to this file's project, and the file is customer_visible
            customer_visible
            AND files.project_id IN (
                SELECT portal_project_ids(NULLIF(current_setting('app.portal_token_hash', true), ''))
            )
        )
    )
    WITH CHECK (
        workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid
    );

GRANT USAGE ON SCHEMA public TO openlane_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON TABLE files TO openlane_app;

-- +goose Down
DROP POLICY IF EXISTS tenant_isolation ON files;
DROP TABLE IF EXISTS files;
