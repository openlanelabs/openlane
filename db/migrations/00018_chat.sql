-- Chat-lite (§217/§274): per-task threads, staff + portal both post on
-- customer-visible tasks. Mentions (@uuid-of-user or plain @name) are
-- extracted and stored on the row — email/Slack push rides P1 SMTP
-- config; the data is here from day one. WS hub is P1 (§449); P0 is
-- REST + ?after= polling.

-- +goose Up
CREATE TABLE task_messages (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    task_id UUID NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
    author_type TEXT NOT NULL CHECK (author_type IN ('user','contact')),
    author_id UUID, -- users.id or contacts.id (nullable for system, future)
    body TEXT NOT NULL CHECK (char_length(body) BETWEEN 1 AND 2000),
    mentions JSONB NOT NULL DEFAULT '[]'::jsonb CHECK (jsonb_typeof(mentions) = 'array'),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE task_messages ENABLE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON task_messages
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

-- portal: read + write messages on customer_visible tasks in the
-- link's projects. Task scoping goes through tasks' own RLS-visible
-- rows (portal sees exactly those tasks); scalar resolvers only for
-- portal_links reads. PR #61 paired-read rule: SELECT policy covers
-- the portal list + any RETURNING.
CREATE POLICY portal_scope_insert ON task_messages FOR INSERT
    WITH CHECK (
        task_id IN (SELECT t.id FROM tasks t WHERE t.customer_visible AND t.deleted_at IS NULL)
        AND workspace_id = portal_link_workspace(NULLIF(current_setting('app.portal_token_hash', true), ''))
    );
CREATE POLICY portal_scope ON task_messages
    USING (workspace_id = portal_link_workspace(NULLIF(current_setting('app.portal_token_hash', true), '')));

-- chat spam edge (§276): count checks ride the author's own history
CREATE INDEX idx_task_messages_task ON task_messages(task_id, created_at);
CREATE INDEX idx_task_messages_author ON task_messages(author_type, author_id, created_at DESC);
GRANT USAGE ON SCHEMA public TO openlane_app;
GRANT SELECT, INSERT ON TABLE task_messages TO openlane_app;

-- +goose Down
DROP POLICY IF EXISTS portal_scope ON task_messages;
DROP POLICY IF EXISTS portal_scope_insert ON task_messages;
DROP POLICY IF EXISTS tenant_isolation ON task_messages;
DROP TABLE IF EXISTS task_messages;
