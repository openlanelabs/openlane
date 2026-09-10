-- Runs as POSTGRES_USER (owner) on first container init.
-- Creates the application role that RLS actually applies to (ADR-0002).
-- Password is env-driven at container init: APP_DB_PASSWORD (default openlane_app).
-- goose migrations grant the role its privileges; this only creates the login.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'openlane_app') THEN
        CREATE ROLE openlane_app LOGIN PASSWORD 'openlane_app';
    END IF;
END
$$;
