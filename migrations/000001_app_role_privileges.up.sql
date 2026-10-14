-- Application role and default privileges.
--
-- The role normally already exists (created with a password by
-- deploy/postgres/init). It is created here without LOGIN only so the
-- migrations also run on databases that skipped that script, such as test
-- containers.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wagering_app') THEN
        CREATE ROLE wagering_app NOLOGIN;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO wagering_app;

-- Tables created by later migrations get these privileges automatically.
-- Tables that must be append-only (the ledger) revoke UPDATE and DELETE
-- explicitly in their own migration.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO wagering_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    GRANT USAGE, SELECT ON SEQUENCES TO wagering_app;
