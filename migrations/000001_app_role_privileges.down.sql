-- Undoes: the default privileges and the schema usage that migration 000001 granted to the application role wagering_app.
-- Loses: the application role's access (a protection, not data): it can no longer read or write tables created from now on, nor use the public schema, until the migration is applied again. No table data is touched.
-- Requires: nothing. The role itself is cluster-wide and may serve other databases, so it is not dropped here.
-- Command: make migrate-down N=1
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE USAGE, SELECT ON SEQUENCES FROM wagering_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM wagering_app;

REVOKE USAGE ON SCHEMA public FROM wagering_app;
