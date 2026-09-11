-- +goose Up
-- Resourcing v1 (P1 §286-§298): people + allocations + utilization data
-- layer. Heatmap/backfill/forecast ride these tables later.

CREATE TABLE people (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id       uuid REFERENCES users(id),
    name          text NOT NULL,
    role          text NOT NULL,
    skills        text[] NOT NULL DEFAULT '{}',
    cost_rate     numeric(12,2) NOT NULL DEFAULT 0 CHECK (cost_rate >= 0),
    bill_rate     numeric(12,2) NOT NULL DEFAULT 0 CHECK (bill_rate >= 0),
    capacity_hrs  numeric(5,1) NOT NULL DEFAULT 40 CHECK (capacity_hrs > 0 AND capacity_hrs <= 80),
    timezone      text NOT NULL DEFAULT 'UTC',
    active        boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz
);
CREATE INDEX idx_people_workspace ON people (workspace_id) WHERE deleted_at IS NULL;
CREATE UNIQUE INDEX idx_people_user_unique ON people (workspace_id, user_id)
    WHERE user_id IS NOT NULL AND deleted_at IS NULL;

CREATE TABLE allocations (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id  uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    project_id    uuid NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    person_id     uuid NOT NULL REFERENCES people(id) ON DELETE CASCADE,
    role          text NOT NULL,
    hours_week    numeric(5,1) NOT NULL CHECK (hours_week > 0 AND hours_week <= 80),
    starts_on     date NOT NULL,
    ends_on       date NOT NULL,
    kind          text NOT NULL DEFAULT 'hard' CHECK (kind IN ('hard','soft')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz,
    CHECK (ends_on >= starts_on)
);
CREATE INDEX idx_allocations_workspace ON allocations (workspace_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_allocations_person ON allocations (person_id);
CREATE INDEX idx_allocations_project ON allocations (project_id);

-- tenant RLS (staff-only: internal financials §207; no portal policy ever)
ALTER TABLE people ENABLE ROW LEVEL SECURITY;
ALTER TABLE people FORCE ROW LEVEL SECURITY;
CREATE POLICY people_tenant ON people
    FOR ALL
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

ALTER TABLE allocations ENABLE ROW LEVEL SECURITY;
ALTER TABLE allocations FORCE ROW LEVEL SECURITY;
CREATE POLICY allocations_tenant ON allocations
    FOR ALL
    USING (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid)
    WITH CHECK (workspace_id = NULLIF(current_setting('app.workspace_id', true), '')::uuid);

-- +goose Down
DROP POLICY allocations_tenant ON allocations;
ALTER TABLE allocations NO FORCE ROW LEVEL SECURITY;
ALTER TABLE allocations DISABLE ROW LEVEL SECURITY;
DROP TABLE allocations;
DROP POLICY people_tenant ON people;
ALTER TABLE people NO FORCE ROW LEVEL SECURITY;
ALTER TABLE people DISABLE ROW LEVEL SECURITY;
DROP TABLE people;
