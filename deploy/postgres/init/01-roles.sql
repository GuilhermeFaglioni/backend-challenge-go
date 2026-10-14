-- Runs once, when the Postgres container initializes an empty data directory.
--
-- Two roles:
--   wagering_owner (POSTGRES_USER) owns the schema and runs the migrations.
--   wagering_app is what the application connects as. It receives only the
--   privileges granted by the migrations, so it cannot, for example, UPDATE or
--   DELETE ledger entries even if the application tried to.
--
-- Local development password only. Never reuse it outside docker compose.
CREATE ROLE wagering_app LOGIN PASSWORD 'app_local_pw';
GRANT CONNECT ON DATABASE wagering TO wagering_app;
