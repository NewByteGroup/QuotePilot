# ADR-003: PostgreSQL 16 on Railway and Supabase, with Cloudflare R2 storage

- **Status:** On-Hold
- **Date:** 2026-11-06
- **Revised:** 2026-11-09 (provider topology from product spec v1.2; see "Revision notes")
- **Deciders:** Pedro (backend lead). Review requested from Marcelo and Amanda for account ownership and cost.
- **Related:** ADR-001 (only `core-api` touches Postgres and storage), ADR-002 (Go stack, migrations tool), ADR-004 (River needs `LISTEN/NOTIFY`), ADR-005 (auth and the privileged pre-tenant path), ADR-007 (CI/CD and environments). `infra/postgres/init/01-roles-and-schemas.sh` is the executable form of sections 1, 2 and 4 for local (and CI once Mission 2 adds it).

## Context

ADR-001 puts all database and object-storage access behind `core-api`, and the product spec (sections 4.3 and 5.4, kept locally and not in git) requires PostgreSQL Row-Level Security with `FORCE ROW LEVEL SECURITY` on every tenant table, `pgvector`, and a tenant-prefixed S3-compatible bucket. The team is three people with four weeks, so Postgres and storage must be managed, not run by us.

The product spec v1.2 section 4.5 fixes development Postgres on Railway, production Postgres on Supabase in `sa-east-1`, and object storage on Cloudflare R2. Supabase's default posture remains a production risk: it exposes tables through auto-generated REST and GraphQL APIs and grants access to managed roles. The risk is **accidentally leaving a second door open around the RLS we rely on**.

Facts checked against the Supabase documentation on 2026-11-06 (re-verify at provisioning time, these change):

- The direct connection (`db.<ref>.supabase.co:5432`) is IPv6-only unless the paid IPv4 add-on is enabled. The shared pooler (Supavisor) is IPv4, with session mode on 5432 and transaction mode on 6543. A dedicated pooler (PgBouncer, port 6543) exists on paid plans.
- Transaction-mode pooling does not support session features: `SET`, `LISTEN/NOTIFY`, advisory locks, and (for Supavisor) prepared statements. Session mode supports them. (Stated in the self-hosting guide; the hosted shared pooler is the same software, but confirm for hosted at provisioning.)
- On existing projects, new tables in `public` are automatically granted `SELECT/INSERT/UPDATE/DELETE` to `anon`, `authenticated` and `service_role`, so a new table is reachable through the Data API the moment it is created. RLS protects rows but not schema visibility (GraphQL introspection).
- The original Supabase Storage plan used S3 access keys that bypassed RLS; Cloudflare R2 now replaces that storage plan (revision note below).
- `pgvector` supports HNSW and IVFFlat; from 0.8.0 filtered HNSW queries can use iterative scans (`hnsw.iterative_scan`, off by default).

## Decision

**Use PostgreSQL 16 on Railway for development and on Supabase (`quotepilot-prod`, `sa-east-1`) for production, with Cloudflare R2 for object storage.** The production Supabase project uses none of its application services in the MVP (Data API/PostgREST, GraphQL, Auth, Realtime, Edge Functions, Studio-side policies). Only `core-api` reaches either database or R2; `ai-engine` has no database or storage credentials (ADR-001). This provider topology is confirmed in product spec v1.2 section 4.5. The ADR stays On-Hold until provisioning and CI decisions are resolved in ADR-007.

### 1. Roles: the app never connects as a Supabase role

| Role | Purpose | Attributes |
|---|---|---|
| `qp_migrator` | owns schema `app` and all objects; runs migrations | `LOGIN`, `NOBYPASSRLS`, `NOSUPERUSER`, `NOCREATEDB`, `NOCREATEROLE` |
| `qp_app` | used by `core-api` and River workers | `LOGIN`, `NOBYPASSRLS`, `NOSUPERUSER`, `NOCREATEDB`, `NOCREATEROLE`, **not an owner of any table** |

- `core-api` and workers never use `postgres`, `service_role` or any Supabase-managed role.
- Tables have `FORCE ROW LEVEL SECURITY`, so even the owner `qp_migrator` is subject to the policies.
- **Startup assertion:** `core-api` runs `SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user` on every new pool and refuses to start if true. It also refuses to start if `current_user` owns any table in `app`.
- A third role for the privileged pre-tenant path (alias to organization lookup, login, memberships, cross-tenant reprocess fan-out) is specified in ADR-005; it must be narrower than `qp_app`, never `BYPASSRLS`, and implemented as explicit grants or `SECURITY DEFINER` functions with a fixed `search_path`.

Sketch (the as-built local version is `infra/postgres/init/01-roles-and-schemas.sh`; hosted differences are in section 6; passwords come from the secret store or `.env`, never from the repo):

```sql
CREATE ROLE qp_migrator LOGIN NOBYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD :'migrator_pw';
CREATE ROLE qp_app      LOGIN NOBYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE PASSWORD :'app_pw';

REVOKE TEMPORARY ON DATABASE :"dbname" FROM PUBLIC;  -- local init also revokes CONNECT, see section 6

CREATE SCHEMA app   AUTHORIZATION qp_migrator;
CREATE SCHEMA river AUTHORIZATION qp_migrator;
REVOKE ALL ON SCHEMA app, river FROM PUBLIC;
GRANT USAGE ON SCHEMA app, river TO qp_app;

ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator IN SCHEMA app, river
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO qp_app;
ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator IN SCHEMA app, river
  GRANT USAGE, SELECT ON SEQUENCES TO qp_app;
ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator IN SCHEMA river
  GRANT EXECUTE ON FUNCTIONS TO qp_app;
ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator    -- no IN SCHEMA: all schemas
  REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC;

ALTER ROLE qp_migrator SET search_path = app, extensions;
ALTER ROLE qp_app      SET search_path = app, extensions;
-- immutable tables (audit logs, snapshots, tariffs) override the table grant per table:
-- REVOKE UPDATE, DELETE ON app.tenant_audit_logs FROM qp_app;
```

Omitted here: the stand-in roles and section 2 revokes (section 2), extension creation and `USAGE` on `extensions` (section 4), and, locally, `REVOKE CONNECT` paired with `GRANT CONNECT ... TO qp_migrator, qp_app`. The script also uses one statement per schema where the sketch writes `app, river`.

Why each addition to the first sketch:

- **Sequences.** `serial`/`bigserial` columns (for example River's job id, to be confirmed against the pinned River version) need sequence `USAGE` for `INSERT` to work as `qp_app`.
- **Extensions.** `qp_migrator` has no database-level `CREATE` and no `CREATE` on schema `extensions`, and pgvector is not marked trusted in its control file (`pg_trgm` is, but it still needs database `CREATE`); so the bootstrap superuser creates both (section 4).
- **Functions.** The global `REVOKE EXECUTE ... FROM PUBLIC` keeps `SECURITY DEFINER` functions from ADR-005 closed by default, so every function in `app` that `qp_app` calls needs an explicit `GRANT EXECUTE` in the same migration. This includes implicit call sites: functions used in RLS policy expressions, column defaults, `CHECK` constraints and index expressions, and functions called through `sqlc`; each fails with a permission error at DML time. Schema `river` is exempt: River's own helper function is used in its unique index and in `ON CONFLICT` targets, so `qp_app` must be able to run it.
- **TEMPORARY.** `PUBLIC` has it by default, which lets `qp_app` create `pg_temp` tables that shadow `app.*` names in a pooled session. The startup assertion would not catch that.
- **search_path.** Pinned per role. River migrations must run with the schema set explicitly (otherwise tables land in `app`, because `search_path` starts with `app`); the River client is configured with its schema explicitly.

### 2. Close the Data API door

- All our tables live in schema `app` (and River's in schema `river`), **not in `public`**, and neither schema is exposed to the Data API.
- `REVOKE ALL` on both schemas and on their tables and sequences from `anon`, `authenticated` and `service_role`, plus `ALTER DEFAULT PRIVILEGES ... REVOKE` for the same roles.
- Disable the Data API and `pg_graphql` in the project settings. They add nothing for us and widen the surface.
- **Provisioning check and CI test (must return zero rows):**

```sql
-- information_schema.role_table_grants only shows roles the current user belongs to,
-- so it can return zero rows while grants exist. Use has_table_privilege, which checks
-- the real privilege for any role regardless of who runs the query.
SELECT r.rolname, n.nspname, c.relname, p.priv
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
CROSS JOIN (VALUES ('anon'), ('authenticated'), ('service_role')) AS r(rolname)
CROSS JOIN (VALUES ('SELECT'), ('INSERT'), ('UPDATE'), ('DELETE'), ('TRUNCATE'), ('REFERENCES'), ('TRIGGER')) AS p(priv)
WHERE n.nspname IN ('app', 'river')
  AND c.relkind IN ('r', 'p', 'v', 'm')
  AND has_table_privilege(r.rolname, c.oid, p.priv);
```

`infra/postgres/checks/roles.sql` is the executable version: assertions that exit non-zero on any offender. Locally, the init script creates `NOLOGIN` stand-ins `anon`, `authenticated` and `service_role`, so the Supabase-specific grants and ACL assertions can be checked against local stand-ins. Before production Supabase use, `roles.sql` needs a named exception list tracked in ADR-007 (to be written): assertion [B3] (nobody is a member of `qp_migrator` or `qp_app`) rejects the hosted `postgres` membership that section 6 requires, and [X] (platform roles) also needs one. Assertion [G] needs no exception: it asserts `CONNECT` true and `TEMPORARY` false for `qp_app` and `qp_migrator` on the current database, which is acceptance item 8, so it must pass on Supabase (`TEMPORARY` revoked from `PUBLIC`, `CONNECT` kept). If the `TEMPORARY` revoke turns out not to be applicable on the hosted database (who owns it there is unverified), [G] and acceptance item 8 must be revisited together. ADR-007 must define the Railway development check path, where Supabase-managed roles do not exist.

The check must also assert that the three roles exist (otherwise `has_table_privilege` errors) and
that `has_schema_privilege(role, 'app', 'USAGE')` is false for each. Also check `PUBLIC` grants
with `aclexplode(c.relacl)` where `grantee = 0`. All of this is implemented in `roles.sql`.

The `ALTER DEFAULT PRIVILEGES ... REVOKE` for those roles is in the init script. The init script mirrors it locally (a no-op there); the hosted provisioning runbook (ADR-007) must run the same statements.

- On production Supabase, run its security and performance advisors after every migration that creates a table, and treat an RLS or exposure finding as a failing check.

### 3. Connection mode

- In production on Supabase, `core-api` and the River workers use the **shared pooler in session mode (port 5432, IPv4)** unless Railway's production host has a verified direct IPv6 path. River uses `LISTEN/NOTIFY` and advisory locks for leader election, and `pgx` uses prepared statements by default. ADR-007 records the tested connection path.
- In development on Railway, use a session-stable direct connection to the Railway Postgres service. Confirm connectivity, River's session features, TLS and pool limits during provisioning; ADR-007 records the tested path. Local and CI use the Docker database below.
- Migrations run as `qp_migrator` over the direct connection when reachable, otherwise session mode. Never through transaction mode.
- `InTenantTx` uses `SELECT set_config('app.current_organization_id', $1, true)` inside the transaction. The `true` makes it transaction-local, so it also stays correct if the connection mode ever changes to transaction pooling. The isolation suite includes pooled reuse A then B to prove it.
- Pool sizing: `pgxpool` max connections for `core-api` plus workers must stay below each environment's connection limit, accounting for Supabase's reserved connections in production. Record the numbers in the config and re-check when compute size changes.
- Fallback if we are forced onto transaction mode: `pgx` with `QueryExecModeSimpleProtocol` (or `CacheDescribe`) and River in poll-only mode. Not the default.

### 4. Extensions

`vector` (pgvector, version 0.8 or newer, verified with `SELECT extversion FROM pg_extension WHERE extname='vector'`) and `pg_trgm` (LOCODE alias matching) live in the `extensions` schema in every environment. Locally the init script creates them as the bootstrap superuser; both app roles get `USAGE` on the schema. Reason: `qp_migrator` has no database-level `CREATE` and no `CREATE` on schema `extensions`, and pgvector is not marked trusted in its control file (`pg_trgm` is, but it still needs database `CREATE`). On Railway development and Supabase production, privileged provisioning must create the extensions; the local init script does not run there. No ANN index in the MVP (ADR-001 data model): exact scan after the `organization_id` and modal filter. If an HNSW index is added later, enable `hnsw.iterative_scan` and test recall under the tenant filter.

### 5. Object storage

- Two **private Cloudflare R2 buckets**: `quotepilot-dev` and `quotepilot-prod`. Object keys remain `tenants/{organization_id}/rfqs/...` and `tenants/{organization_id}/quotes/...`.
- Only `core-api` talks to R2 through its S3-compatible API with server-side credentials. R2 does not enforce PostgreSQL RLS, so **tenant isolation in storage is enforced by `core-api`**: it builds every key from the authenticated tenant and issues short-lived presigned URLs only after a permission check (ADR-005).
- The storage client stays behind a project-owned interface (AGENTS.md, Dependencies). Credentials and bucket names are environment-specific; `ai-engine` receives neither storage credentials nor `organization_id`.
- Confirm presigned GET/PUT round-trips against each R2 bucket with the chosen client library before use.

### 6. Environments

- **Local and CI:** `docker-compose.yml` with image `pgvector/pgvector:0.8.2-pg16` (pinned; bump deliberately, since section 4 needs pgvector 0.8 or newer). The init script creates the same roles and schemas as production, so the isolation suite runs against the real role model. The Supabase local stack is not required.
  - The port is bound to localhost only.
  - The healthcheck also requires the `qp_app` role, so a failed first init shows as unhealthy.
  - The init script runs on first volume init only; `make db-reset` recreates the volume.
  - Passwords come only from `.env`.
  - Locally, the `CONNECT` revoke covers only the application database: `postgres` and `template1` still grant `PUBLIC` `CONNECT` (and `postgres` also `TEMPORARY`), and any per-test database must repeat the `REVOKE`.
- **Development:** Railway hosts `core-api`, `ai-engine` and PostgreSQL 16 from a pgvector image; Cloudflare Pages hosts the frontend and R2 holds the private `quotepilot-dev` bucket. Provision `vector` and `pg_trgm` in `extensions`, and `app` and `river` under the intended role model. Railway's database setup and checks need a runbook in ADR-007.
- **Production:** Railway hosts `core-api` and `ai-engine`; Cloudflare Pages hosts the frontend. The database is Supabase project `quotepilot-prod` in `sa-east-1` (São Paulo), with its Data API and `pg_graphql` disabled. Cloudflare R2 holds the private `quotepilot-prod` bucket. `ai-engine` is reached over Railway's private network. The product spec v1.2 names `X-Internal-Service-Key`, while proposed ADR-005 names a Bearer token; reconcile this service-auth contract before implementation. ADR-007 owns deployment, secrets, connection paths and the hosted provisioning runbook.
- **Supabase production differences** (from Supabase documentation and experience, not verified in this repo; confirm at provisioning). The init script targets the local container only; hosted provisioning is part of ADR-007:
  - `postgres` is not a superuser on Supabase. On Postgres 16+, role membership carries `ADMIN`/`INHERIT`/`SET` options; `postgres` needs membership in `qp_migrator` with the `SET` option to run `CREATE SCHEMA ... AUTHORIZATION qp_migrator` and `ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator`. Confirm the exact `GRANT` at provisioning.
  - Schema `extensions` already exists; do not create it. `GRANT USAGE ON SCHEMA extensions TO qp_migrator, qp_app` is required unless `PUBLIC` already has `USAGE` (confirm at provisioning).
  - The three Supabase roles already exist; the stand-ins are local only.
  - Revoke only `TEMPORARY` from `PUBLIC`. Supabase-managed roles need `CONNECT`, so the `CONNECT` revoke is local-only. Revoking `TEMPORARY` may affect Supabase-managed roles that rely on it; check at provisioning.
- The production region is `sa-east-1` per product spec v1.2; confirm its availability and the PostgreSQL version at provisioning. The former shared Supabase staging-project plan is superseded by Railway development Postgres.
- Free-tier projects have historically been paused after a period of inactivity (not re-verified here; check the current policy), and backups depend on the plan. The production project and the final on 12/12 must not depend on an account that may pause.

### 7. Account, secrets, backups

- The Supabase organization is owned by an account the team controls, with Marcelo and Amanda invited at developer level. A personal account as sole owner is a bus-factor risk.
- Connection strings and R2 S3-compatible keys live in the secret store / GitHub environments; `.env.example` holds names only. Rotate keys and role passwords if they are ever pasted into a chat or a log.
- Confirm the production Supabase backup plan and Railway development backup policy in ADR-007; PITR is optional. Before the pilot, run one production restore test into a scratch database.

## Alternatives considered

1. **Self-host Postgres (VM or container).** Full control of roles and no BaaS surface, but we would own backups, upgrades, TLS and monitoring. Rejected for a three-person, four-week team.
2. **Another managed Postgres (AWS RDS, Neon, Cloud SQL).** Equally valid and reachable by the same `pgx` connection. The confirmed topology uses Railway for development and Supabase for production; these providers remain possible exit routes (see Reversibility).
3. **Use Supabase fully (Data API plus Auth plus RLS from the browser).** Fastest for CRUD, but it moves tenant isolation into policies written against JWT claims and exposes the schema to the internet, and it is incompatible with ADR-001's rule that only `core-api` touches the database.
4. **Supabase Storage for files.** This was the first draft's chosen option. Product spec v1.2 supersedes it with Cloudflare R2 in both hosted environments; the project-owned S3 interface keeps the change local to `core-api` wiring.

## Consequences

**Positive**
- Managed Postgres 16 with pgvector and S3-compatible R2 storage, with no database or object-storage infrastructure to run ourselves.
- The database is reachable only by `core-api` and River workers under roles we control, which keeps the RLS story in ADR-001 intact.
- Local, CI and hosted use the same role model, so the isolation tests mean something.

**Negative / risks**
- Supabase defaults favour the exposed-API model. Every guard above (own schema, revokes, disabled Data API, advisors, the zero-rows grant check, the startup assertion) exists to prevent one forgotten default from opening a door. If the guards are not in CI they will rot.
- Session-mode pooling or direct connections cap concurrency at the project's connection limit; fine for four tenants, a constraint if load grows.
- IPv6/IPv4 mismatch between the host and the database fails at connect time and is easy to misdiagnose; ADR-007 must record which path each environment uses.
- R2 S3-compatible keys operate outside PostgreSQL RLS, so a leaked broadly scoped key can expose tenant files.
- The init script runs only on an empty volume, so role-model changes need `make db-reset` locally and a hosted runbook entry.
- Every function in `app` that `qp_app` calls needs an explicit `GRANT EXECUTE`, including implicit call sites (functions used in RLS policy expressions, column defaults, `CHECK` constraints and index expressions, and functions called through `sqlc`); forgetting it fails with a permission error at DML time.
- psql may echo a failed `CREATE ROLE` statement, including the interpolated password, into the container log. Rotate the password if that happens.

**Reversibility.** Standard Postgres wire protocol and the S3 API only. Leaving a database provider means `pg_dump`/restore into another Postgres 16 with `vector` and changing the connection settings. Leaving R2 means changing the S3 endpoint and credentials behind the storage interface. No provider-specific SQL or SDK is used in the domain logic.

## Verification at provisioning (acceptance for this ADR)

1. `SELECT rolname, rolsuper, rolbypassrls FROM pg_roles WHERE rolname IN ('qp_app','qp_migrator')` shows both false (asserted in `infra/postgres/checks/roles.sql`).
2. The grants query in section 2 returns zero rows (`infra/postgres/checks/roles.sql`).
3. Data API and `pg_graphql` are disabled; advisors report no RLS or exposure findings on `app`.
4. `vector` is 0.8 or newer and `pg_trgm` is present.
5. The spike's isolation suite passes against both hosted databases as well as the local one.
6. A presigned upload and download round-trip works through each Cloudflare R2 bucket's S3-compatible endpoint.
7. Default privileges cover sequences (granted to `qp_app`) and functions (closed in `app`, open to `qp_app` in `river`). This is asserted by tags K1a, K1b, K2, K3, K4, K5 and [X] in `infra/postgres/checks/roles.sql` and exercised by `make db-check-negative`. Optional inspection query: `SELECT defaclrole::regrole, defaclnamespace::regnamespace, defaclobjtype, defaclacl FROM pg_default_acl;`.
8. `has_database_privilege` shows `CONNECT` true and `TEMPORARY` false for both `qp_app` and `qp_migrator`.
9. Revoking `TEMPORARY` from `PUBLIC` on the hosted database does not break Supabase-managed roles that rely on it.

## Open items

- Compute size, connection limits, account owner and payment plan for Railway development and Supabase production (Pedro, with the team); production region is `sa-east-1`.
- Which account owns the organization (section 7).
- Exact connection-limit numbers for the chosen compute size.
- Migrations tool and how migrations are applied in CI (ADR-002, ADR-007).
- The privileged pre-tenant role (ADR-005).
- Confirm whether River's schema uses `bigserial` (for example its job id) and which River version ADR-002 pins.
- River's periodic reindexer needs index ownership: disable it or accept `must be owner of index` log lines.
- Hosted provisioning runbook (ADR-007).
- Hosted Postgres major version: local uses 16; confirm the Supabase project's version at provisioning and pin the local image to it, or record the mismatch.
- Confirm the Railway pgvector image version and provision both hosted databases with the same role, schema and extension checks as local/CI (ADR-007).

## Revision notes: changes and additions (2026-11-07)

The local role model was implemented in PR #2 (`infra/postgres/`). Pedro approved these departures from the first draft while the ADR was still Proposed; its current On-Hold status reflects the hosted provider and provisioning questions recorded on 2026-11-09:

1. Default privileges on sequences for `qp_app`: `serial`/`bigserial` columns (for example River's job id, to be confirmed against the pinned River version) fail on `INSERT` without sequence `USAGE`.
2. Extensions created by the bootstrap superuser (an addition: the first draft did not say who creates extensions). `qp_migrator` has no database-level `CREATE` and no `CREATE` on schema `extensions`, and pgvector is not marked trusted in its control file (`pg_trgm` is, but it still needs database `CREATE`).
3. `TEMPORARY` (and, locally, `CONNECT`) revoked from `PUBLIC`: otherwise `qp_app` can create `pg_temp` tables that shadow `app.*` names. `CONNECT` is also revoked locally as defence in depth: only the two application roles can connect to the application database (local only; hosted Supabase roles need `CONNECT`).
4. Supabase role stand-ins (`anon`, `authenticated`, `service_role`) exist locally only, so the section 2 check runs identically on hosted and local.
5. Global `REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC` with schema `river` exempt: keeps ADR-005 `SECURITY DEFINER` functions closed by default, while River's helper function stays callable.
6. `search_path` pinned per role (`app, extensions`): removes dependence on session defaults; River migrations set their schema explicitly.
7. `qp_migrator` gained explicit `NOSUPERUSER NOCREATEDB NOCREATEROLE` (the first draft only said "not superuser"), and the sketch now creates schema `river`.

## Revision note: provider topology (2026-11-09)

Product spec v1.2 section 4.5 fixes Railway PostgreSQL for development, Supabase PostgreSQL in `sa-east-1` for production, Cloudflare R2 for both storage buckets, Railway for `core-api` and `ai-engine`, and Cloudflare Pages for the frontend. This supersedes the first draft's Supabase Storage bucket, shared Supabase staging project and undecided production region. ADR-003 remains On-Hold for the hosted provisioning and CI acceptance details assigned to ADR-007.
