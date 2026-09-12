-- BYO-LLM router (P2 §15, §373): per-workspace LLM config, one row
-- (upsert). api_key sealed AES-256-GCM with OPENLANE_INTEGRATION_KEY
-- (same envelope as jira/salesforce PATs). Azure = openai-compatible
-- base_url; ollama = self-hosted, key unused, cost 0.

-- +goose Up
CREATE TABLE llm_configs (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id    uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    provider        text NOT NULL CHECK (provider IN ('openai','anthropic','ollama')),
    base_url        text NOT NULL,
    cheap_model     text NOT NULL,
    smart_model     text NOT NULL,
    api_key_sealed  jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now(),
    created_by      uuid,
    UNIQUE (workspace_id)
);

ALTER TABLE llm_configs ENABLE ROW LEVEL SECURITY;
ALTER TABLE llm_configs FORCE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON llm_configs
    FOR ALL
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);
-- admin-only at the API layer (PUT); members read presence via GET —
-- the key never leaves sealed.

-- +goose Down
DROP POLICY tenant_isolation ON llm_configs;
ALTER TABLE llm_configs NO FORCE ROW LEVEL SECURITY;
ALTER TABLE llm_configs DISABLE ROW LEVEL SECURITY;
DROP TABLE IF EXISTS llm_configs;
