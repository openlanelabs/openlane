-- Search v1 (§410/§445): typo-tolerant global search. pg_trgm + GIN
-- indexes on the four searchable name/title columns. Visibility stays
-- with RLS — the search query is a plain UNION through each table's
-- policies, so the handler structurally cannot leak (ADR-0002).

-- +goose Up
CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX idx_projects_name_trgm ON projects USING GIN (name gin_trgm_ops);
CREATE INDEX idx_tasks_title_trgm ON tasks USING GIN (title gin_trgm_ops);
CREATE INDEX idx_docs_title_trgm ON docs USING GIN (title gin_trgm_ops);
CREATE INDEX idx_files_name_trgm ON files USING GIN (name gin_trgm_ops);

-- +goose Down
DROP INDEX IF EXISTS idx_files_name_trgm;
DROP INDEX IF EXISTS idx_docs_title_trgm;
DROP INDEX IF EXISTS idx_tasks_title_trgm;
DROP INDEX IF EXISTS idx_projects_name_trgm;
