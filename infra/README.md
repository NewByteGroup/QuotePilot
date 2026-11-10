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
- Checks: `make db-check`, i.e. `docker compose exec -T db sh -c 'psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' < infra/postgres/checks/roles.sql`. Each assertion raises an error naming the offenders.
- `make db-check-negative`: applies deliberate mutations and expects `roles.sql` to fail on each. The negative tests wrap every mutation in a transaction that is never committed.

## Roles

The init script creates three application roles, none of them owners of tenant objects and
none with BYPASSRLS (ADR-003 section 1): `qp_migrator` owns the `app`/`river` schemas and runs
migrations; `qp_app` is the tenant request path — table DML inside `InTenantTx`, EXECUTE on
the three tenant-facing functions only; `qp_admin` is the platform path (ADR-005) — the only
role that may EXECUTE the `platform_*` SECURITY DEFINER functions, including
`platform_delete_organization`. `qp_app` must never be able to delete a tenant, so the
platform grants go to `qp_admin` exclusively; `qp_admin` itself holds no table or default
privileges anywhere (roles.sql `[N]`).

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
`extensions` schema already exists there (do not `CREATE` it). `qp_admin` is a custom role ops
must create manually like the other manual steps here — Supabase provisions nothing beyond the
platform roles; grant it `USAGE ON SCHEMA app` and pin `search_path = app, extensions`, then let
migrations add the explicit EXECUTE grants. The full hosted provisioning steps are deferred to
ADR-007.

### What will false-fail on hosted (`make db-check` equivalent)

Expected failures when `roles.sql` is pointed at hosted Supabase, to be resolved in ADR-007
(not yet written). Items marked "unverified" have not been tried against a real project.

- `[B3]`: the platform `postgres` role is a member of `qp_migrator` (see above); needs an allowlist.
- `[G]`: needs no exception (ADR-003 acceptance item 8): it must pass on hosted, so
  `REVOKE TEMPORARY ON DATABASE ... FROM PUBLIC` has to be applied there. Who owns the database on
  Supabase, and so who may run that revoke, is unverified; if it cannot be applied, revisit `[G]`
  and acceptance item 8 together.
- `[M]`: flags partitions that lack their own ENABLE + FORCE RLS. This is intended.
- `[X]` (allowlist of grantees in `app`/`river`) needs a named exception list for platform roles
  there; unverified which roles Supabase grants by default.
- Not a failure, but worth knowing: global default-ACL rows of platform roles are not checked
  (`[K1b]` checks only `qp_migrator`; `[K5]` only per-schema rows in `app` and `river`).
