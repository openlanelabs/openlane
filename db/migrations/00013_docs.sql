-- Docs v1 (§9/§266): versioned markdown per project. P0 shape: doc row
-- (title, visibility) + append-only doc_versions (spec: version history,
-- never overwrite). Yjs/Hocuspocus collab lands P1+ on the same tables.

-- +goose Up
CREATE TABLE docs (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    title TEXT NOT NULL CHECK (title <> '' AND char_length(title) <= 255),
    customer_visible BOOLEAN NOT NULL DEFAULT false,
    latest_version INT NOT NULL DEFAULT 1,
    created_by UUID REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);

-- workspace_id denormalized: every tenant table carries it for the
-- one-policy RLS shape (ADR-0002); version rows append, never update.
CREATE TABLE doc_versions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    doc_id UUID NOT NULL REFERENCES docs(id) ON DELETE CASCADE,
    version INT NOT NULL CHECK (version >= 1),
    content_md TEXT,
    created_by UUID REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (doc_id, version)
);

ALTER TABLE docs ENABLE ROW LEVEL SECURITY;
ALTER TABLE doc_versions ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON docs
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);
CREATE POLICY tenant_isolation ON doc_versions
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

-- portal: read customer-visible docs + their versions, linked projects
-- only, SELECT-only (staff author, customer reads — §7.3).
CREATE POLICY portal_scope ON docs
    USING (
        project_id IN (SELECT portal_project_ids(NULLIF(current_setting('app.portal_token_hash', true), '')))
        AND customer_visible
    );
CREATE POLICY portal_scope ON doc_versions
    USING (
        workspace_id = portal_link_workspace(NULLIF(current_setting('app.portal_token_hash', true), ''))
        AND doc_id IN (SELECT d.id FROM docs d WHERE d.customer_visible)
    );

CREATE INDEX idx_docs_project ON docs(project_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_doc_versions_doc ON doc_versions(doc_id, version DESC);
GRANT USAGE ON SCHEMA public TO openlane_app;
GRANT SELECT, INSERT, UPDATE ON TABLE docs TO openlane_app;
GRANT SELECT, INSERT ON TABLE doc_versions TO openlane_app;

-- +goose Down
DROP POLICY IF EXISTS portal_scope ON doc_versions;
DROP POLICY IF EXISTS portal_scope ON docs;
DROP POLICY IF EXISTS tenant_isolation ON doc_versions;
DROP POLICY IF EXISTS tenant_isolation ON docs;
DROP TABLE IF EXISTS doc_versions;
DROP TABLE IF EXISTS docs;
