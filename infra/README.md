# infra

Local infrastructure: Docker Compose support files and database init scripts.

## What belongs here

- `postgres/init/`: scripts run once on first volume init (roles, schemas).
- `postgres/checks/`: plain SQL checks for roles, schemas, grants and extensions.
- Other local-environment support files used by `docker-compose.yml`.

## What must never go here

- Real secrets or values in env files; names only (`.env.example`).
- Deploy configuration: it waits for ADR-007 (CI/CD and environments).

## Commands

- `make db-up` (from the repo root): start Postgres 16 with pgvector. Needs `.env`.
- `make db-reset`: destroy the local volume and re-run the init scripts.
- Checks: `docker compose exec -T db sh -c 'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' < infra/postgres/checks/roles.sql` (expected results are in the file's comments).

## Gotcha: init runs once

The init script runs only when the Postgres volume is empty. A failed first init leaves a
database without roles. Fix: `make db-reset`.

## Hosted Supabase

The init script targets the local container only. On hosted Supabase the extensions step is
manual: enable `vector` and `pg_trgm` in the dashboard, or via SQL into schema `extensions`.
Only `REVOKE TEMPORARY ON DATABASE ... FROM PUBLIC` is safe there; do not revoke `CONNECT`,
because Supabase-managed roles need it. The `anon`/`authenticated`/`service_role` stand-ins
are local-only (hosted Supabase already provides them).

The init script is not a hosted provisioning runbook. On Supabase the `postgres` role is not a
superuser and must be a member of `qp_migrator` to set default privileges for it. The
`extensions` schema already exists there (do not `CREATE` it). The full hosted provisioning
steps are deferred to ADR-007.
