-- Runs as superuser during postgres container init.
-- Creates the least-privilege application role used by the api.
-- Password is intended for local/dev only; production uses a secret manager.

DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'paybook_app') THEN
        CREATE ROLE paybook_app LOGIN PASSWORD 'paybook_app_pw';
    END IF;
END $$;
