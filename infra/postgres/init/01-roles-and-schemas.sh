#!/usr/bin/env bash
# Run by the official postgres image entrypoint as the bootstrap superuser, on
# FIRST volume initialization only (never on later starts). Creates roles,
# schemas and the vector/pg_trgm extensions. Passwords come from .env via the
# container environment; none are stored here (ADR-003 sections 1-2, 4).
# To re-run on a fresh volume: `make db-reset`.
set -euo pipefail

# Passwords are read inside psql with \getenv (psql 15+; the image is Postgres 16)
# so they never appear on the psql command line (visible in the process list).
# If a statement fails, psql and the server may echo it, including the interpolated
# password, into the container log; rotate the password if that happens (ADR-003 section 7).
# --single-transaction: a failure part-way rolls everything back, so no qp_app is
# left behind and the healthcheck stays unhealthy.
psql -v ON_ERROR_STOP=1 --single-transaction \
  -v dbname="$POSTGRES_DB" \
  --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<'EOSQL'
\getenv migrator_pw QP_MIGRATOR_PASSWORD
\getenv app_pw QP_APP_PASSWORD

-- 1. Supabase stand-ins. They mimic the Supabase-managed roles so the ADR-003
-- section 2 grants check runs locally exactly as on hosted.
-- DEVIATION from ADR-003 sketch: local only; hosted Supabase already has them.
CREATE ROLE anon NOLOGIN;
CREATE ROLE authenticated NOLOGIN;
CREATE ROLE service_role NOLOGIN;

-- 2. Application roles (ADR-003 section 1): neither owns tables nor bypasses RLS.
CREATE ROLE qp_migrator LOGIN NOBYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD :'migrator_pw';
CREATE ROLE qp_app LOGIN NOBYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD :'app_pw';

-- 3. Database-level privileges.
-- DEVIATION from ADR-003 sketch: PUBLIC has TEMPORARY by default, which lets
-- qp_app create pg_temp tables that shadow app.* names in a pooled session;
-- the ADR-003 startup assertion would not catch it.
-- The CONNECT revoke is local-only (hosted Supabase roles need CONNECT, see
-- infra/README.md). It covers only POSTGRES_DB: `postgres` still grants PUBLIC
-- CONNECT and TEMPORARY (NULL ACL), `template1` grants PUBLIC CONNECT only, and
-- any per-test database created later must repeat the REVOKE.
REVOKE TEMPORARY, CONNECT ON DATABASE :"dbname" FROM PUBLIC;
GRANT CONNECT ON DATABASE :"dbname" TO qp_migrator, qp_app;

-- 4. Extensions, created here as superuser (ADR-003 section 4: schema extensions).
-- DEVIATION from ADR-003 sketch: ADR-003 section 4 does not say who creates
-- extensions. vector is untrusted, so the bootstrap superuser creates both;
-- qp_migrator has no database-level CREATE. On hosted Supabase this is a manual
-- step, see infra/README.md.
CREATE SCHEMA extensions;
CREATE EXTENSION IF NOT EXISTS vector SCHEMA extensions;
CREATE EXTENSION IF NOT EXISTS pg_trgm SCHEMA extensions;
GRANT USAGE ON SCHEMA extensions TO qp_migrator, qp_app;

-- 5. Schemas (ADR-003 section 1): owned by the migrator, closed to everyone else.
CREATE SCHEMA app AUTHORIZATION qp_migrator;
CREATE SCHEMA river AUTHORIZATION qp_migrator;
REVOKE ALL ON SCHEMA app FROM PUBLIC;
REVOKE ALL ON SCHEMA river FROM PUBLIC;
REVOKE ALL ON SCHEMA app, river FROM anon, authenticated, service_role;
GRANT USAGE ON SCHEMA app TO qp_app;
GRANT USAGE ON SCHEMA river TO qp_app;
-- mimics hosted guard from ADR-003 section 2 (no-op locally, no default grant exists)
ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator IN SCHEMA app REVOKE ALL ON TABLES FROM anon, authenticated, service_role;
ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator IN SCHEMA app REVOKE ALL ON SEQUENCES FROM anon, authenticated, service_role;
ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator IN SCHEMA app REVOKE ALL ON FUNCTIONS FROM anon, authenticated, service_role;
ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator IN SCHEMA river REVOKE ALL ON TABLES FROM anon, authenticated, service_role;
ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator IN SCHEMA river REVOKE ALL ON SEQUENCES FROM anon, authenticated, service_role;
ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator IN SCHEMA river REVOKE ALL ON FUNCTIONS FROM anon, authenticated, service_role;

-- 6. Default privileges: objects the migrator creates are usable by qp_app.
ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator IN SCHEMA app
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO qp_app;
ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator IN SCHEMA river
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO qp_app;
-- DEVIATION from ADR-003 sketch: serial/bigserial columns such as river_job.id
-- need sequence USAGE or inserts fail for qp_app.
ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator IN SCHEMA app
  GRANT USAGE, SELECT ON SEQUENCES TO qp_app;
ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator IN SCHEMA river
  GRANT USAGE, SELECT ON SEQUENCES TO qp_app;
ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator IN SCHEMA river
  GRANT EXECUTE ON FUNCTIONS TO qp_app;
-- DEVIATION from ADR-003 sketch: no IN SCHEMA, so it applies to all schemas.
-- Schema app stays closed: every function in app that qp_app calls needs an
-- explicit GRANT EXECUTE in the same migration (ADR-005 SECURITY DEFINER
-- functions rely on this). Schema river is deliberately exempt (grant above)
-- because River's own migrations create helper functions
-- (river_job_state_in_bitmask, used in river_job_unique_idx and in ON CONFLICT
-- targets) that qp_app must be able to execute.
ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;

-- 7. Explicit search_path.
-- DEVIATION from ADR-003 sketch: pinned per role; the River schema is
-- configured explicitly in the River client, not via search_path.
-- Run River migrations with the schema set explicitly (rivermigrate Schema
-- "river"), otherwise tables land in app because search_path starts with app.
-- River's reindexer needs index ownership, so disable it or expect
-- `must be owner of index` log lines (Mission 3).
ALTER ROLE qp_migrator SET search_path = app, extensions;
ALTER ROLE qp_app SET search_path = app, extensions;
EOSQL
