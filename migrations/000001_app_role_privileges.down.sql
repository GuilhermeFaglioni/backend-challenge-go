-- The role itself is cluster-wide and may be used by other databases, so it is
-- not dropped here; only the privileges this migration granted are revoked.
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE USAGE, SELECT ON SEQUENCES FROM wagering_app;
ALTER DEFAULT PRIVILEGES IN SCHEMA public
    REVOKE SELECT, INSERT, UPDATE, DELETE ON TABLES FROM wagering_app;

REVOKE USAGE ON SCHEMA public FROM wagering_app;
