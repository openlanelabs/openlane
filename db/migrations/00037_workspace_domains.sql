-- Custom domains (P1 §199/§229/§499): per-workspace branded portal
-- hosts. Verification = TXT challenge at _openlane-challenge.<domain>.
-- TLS termination lives at the hosting layer (see docs); the app owns
-- registration, verification, and host→workspace resolution.

-- +goose Up

CREATE TABLE workspace_domains (
    id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    domain       text NOT NULL UNIQUE,
    verified     boolean NOT NULL DEFAULT false,
    challenge    text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX workspace_domains_ws_idx ON workspace_domains (workspace_id);

ALTER TABLE workspace_domains ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_domains FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON workspace_domains
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON workspace_domains TO openlane_app;

-- +goose Down

DROP TABLE IF EXISTS workspace_domains;
