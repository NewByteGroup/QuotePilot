# ADR-003: PostgreSQL 16 on Railway (dev) and Supabase (prod), with Cloudflare R2 object storage

- **Status:** On-Hold (open items remain: plans, account ownership, connection limits, provisioning runbooks)
- **Date:** 2026-11-06
- **Revised:** 2026-11-07 (local init script and checks), 2026-11-09 (Railway/Supabase/R2 topology per product spec v1.2); see "Revision notes"
- **Deciders:** Pedro (backend lead). Reviewed by Marcelo and Amanda for account ownership and cost.
- **Related:** ADR-001 (only `core-api` touches Postgres and storage), ADR-002 (Go stack, migrations tool), ADR-004 (River needs `LISTEN/NOTIFY`), ADR-005 (auth and the privileged pre-tenant path), ADR-007 (CI/CD and environments). `infra/postgres/init/01-roles-and-schemas.sh` is the executable form of sections 1, 2 and 4 for local (and CI once Mission 2 adds it).

## Context

ADR-001 puts all database and object-storage access behind `core-api`, and the product spec (sections 4.3 and 5.4, kept locally and not in git) requires PostgreSQL Row-Level Security with `FORCE ROW LEVEL SECURITY` on every tenant table, `pgvector`, and a tenant-prefixed S3-compatible bucket. The team is three people with four weeks, so Postgres and storage must be managed, not run by us.

The current topology places development PostgreSQL 16 with `pgvector` on Railway, production PostgreSQL 16 on Supabase in `sa-east-1`, and private object storage on Cloudflare R2. Supabase remains a backend-as-a-service whose default posture is the opposite of ours: it exposes tables to the browser through an auto-generated REST and GraphQL API, and its documented defaults grant table access to the `anon`, `authenticated` and `service_role` roles. Two of its roles bypass RLS by design. The risk in this decision is therefore not capability but **accidentally leaving a second door open around the RLS we rely on**.

Facts checked against the Supabase documentation on 2026-11-06 (re-verify at provisioning time, these change):

- The direct connection (`db.<ref>.supabase.co:5432`) is IPv6-only unless the paid IPv4 add-on is enabled. The shared pooler (Supavisor) is IPv4, with session mode on 5432 and transaction mode on 6543. A dedicated pooler (PgBouncer, port 6543) exists on paid plans.
- Transaction-mode pooling does not support session features: `SET`, `LISTEN/NOTIFY`, advisory locks, and (for Supavisor) prepared statements. Session mode supports them. (Stated in the self-hosting guide; the hosted shared pooler is the same software, but confirm for hosted at provisioning.)
- On existing projects, new tables in `public` are automatically granted `SELECT/INSERT/UPDATE/DELETE` to `anon`, `authenticated` and `service_role`, so a new table is reachable through the Data API the moment it is created. RLS protects rows but not schema visibility (GraphQL introspection).
- The original Supabase Storage option would have used S3 access keys that bypass storage RLS. This option is superseded by Cloudflare R2; R2 credentials likewise stay server-side and are scoped to the intended bucket where possible.
- `pgvector` supports HNSW and IVFFlat; from 0.8.0 filtered HNSW queries can use iterative scans (`hnsw.iterative_scan`, off by default).

## Decision

**Use Railway PostgreSQL 16 with `pgvector` for development, Supabase PostgreSQL 16 in `sa-east-1` for production, and Cloudflare R2 private buckets for object storage.** Supabase application services are outside the MVP: its Data API/PostgREST and `pg_graphql` are disabled, and Auth, Realtime and Edge Functions are not used. Only `core-api` connects to either database or to storage; `ai-engine` receives no database or storage credentials.

### 1. Roles: the app never connects as a provider-managed role

| Role | Purpose | Attributes |
|---|---|---|
| `qp_migrator` | owns schema `app` and all objects; runs migrations | `LOGIN`, `NOBYPASSRLS`, `NOSUPERUSER`, `NOCREATEDB`, `NOCREATEROLE` |
| `qp_app` | used by `core-api` and River workers | `LOGIN`, `NOBYPASSRLS`, `NOSUPERUSER`, `NOCREATEDB`, `NOCREATEROLE`, **not an owner of any table** |

- `core-api` and River workers use only `qp_app` on both hosted databases; they never use `postgres`, `service_role` or another provider-managed role.
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

### 2. Close the production Supabase Data API door

- All our tables live in schema `app` (and River's in schema `river`), **not in `public`**, and neither schema is exposed to the Data API.
- `REVOKE ALL` on both schemas and on their tables and sequences from `anon`, `authenticated` and `service_role`, plus `ALTER DEFAULT PRIVILEGES ... REVOKE` for the same roles.
- In the production Supabase project, disable the Data API and `pg_graphql` in the project settings. They add nothing for us and widen the surface.
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

`infra/postgres/checks/roles.sql` is the executable version: assertions that exit non-zero on any offender. Locally, the init script creates `NOLOGIN` stand-ins `anon`, `authenticated` and `service_role`, so the grants and ACL assertions run unchanged on the hosted databases. The same runbook applies to both hosted Postgres instances; how much of it fits Railway's managed Postgres (its bootstrap role, database ownership, default `PUBLIC` grants) is unverified and tracked in the open items. Before hosted use, `roles.sql` needs a named exception list tracked in ADR-007 (to be written): assertion [B3] (nobody is a member of `qp_migrator` or `qp_app`) rejects the hosted Supabase `postgres` membership that section 6 requires, and [X] (platform roles) also needs one. Assertion [G] needs no exception: it asserts `CONNECT` true and `TEMPORARY` false for `qp_app` and `qp_migrator` on the current database, which is acceptance item 8, so it must pass on hosted (`TEMPORARY` revoked from `PUBLIC`, `CONNECT` kept). If the `TEMPORARY` revoke turns out not to be applicable on a hosted database (who owns it there is unverified), [G] and acceptance item 8 must be revisited together. The stand-in roles make the other assertions run identically.

On Supabase (and locally, via the stand-ins) the check must also assert that the three roles exist — otherwise `has_table_privilege` errors — and that `has_schema_privilege(role, 'app', 'USAGE')` is false for each; on Railway those roles do not exist, so only the generic assertions apply. Also check `PUBLIC` grants with `aclexplode(c.relacl)` where `grantee = 0`. All of this is implemented in `roles.sql`.

The `ALTER DEFAULT PRIVILEGES ... REVOKE` for those roles is in the init script. The init script applies it locally to the stand-ins; the hosted provisioning runbooks (ADR-007) must run the same statements where the roles exist (Supabase only; Railway has no such roles).

- On the production Supabase project, run its security and performance advisors after every migration that creates a table, and treat an RLS or exposure finding as a failing check. Run the role and grant checks against both hosted databases.

### 3. Connection mode

- On production Supabase, `core-api` and the River workers use the **shared pooler in session mode (port 5432, IPv4)**. On development Railway, use a session-stable Postgres connection; confirm its actual endpoint and pool limit in ADR-007. Reasons: River uses `LISTEN/NOTIFY` and advisory locks for leader election, `pgx` uses prepared statements by default, and `InTenantTx` is simplest to reason about on a session-stable connection.
- For production Supabase, if the host that runs `core-api` has IPv6 (decided in ADR-007), prefer the **direct connection**, which has no pooler in the path. GitHub-hosted runners and Vercel have historically been IPv4-only (unverified here; re-check at provisioning), so assume CI and Vercel reach the hosted Supabase database through the pooler. ADR-007 depends on this.
- Migrations run as `qp_migrator` over the direct connection when reachable, otherwise session mode. Never through transaction mode.
- `InTenantTx` uses `SELECT set_config('app.current_organization_id', $1, true)` inside the transaction. The `true` makes it transaction-local, so it also stays correct if the connection mode ever changes to transaction pooling. The isolation suite includes pooled reuse A then B to prove it.
- Pool sizing: `pgxpool` max connections for `core-api` plus workers must stay below each provider's connection limit for its compute size minus its reserved connections. Record the Railway development and Supabase production numbers in ADR-007 and re-check when the compute size changes.
- Fallback if we are forced onto transaction mode: `pgx` with `QueryExecModeSimpleProtocol` (or `CacheDescribe`) and River in poll-only mode. Not the default.

### 4. Extensions

`vector` (pgvector, version 0.8 or newer, verified with `SELECT extversion FROM pg_extension WHERE extname='vector'`) and `pg_trgm` (LOCODE alias matching), created in the `extensions` schema. Who creates them: locally the init script, as the bootstrap superuser, into schema `extensions`; both app roles get `USAGE` on the schema. Reason: `qp_migrator` has no database-level `CREATE` and no `CREATE` on schema `extensions`, and pgvector is not marked trusted in its control file (`pg_trgm` is, but it still needs database `CREATE`); so the bootstrap superuser creates both. On the hosted databases it is a manual step in the provisioning runbook, because the init script does not run there: Supabase already has schema `extensions` (section 6), while Railway needs it created like locally. No ANN index in the MVP (ADR-001 data model): exact scan after the `organization_id` and modal filter. If an HNSW index is added later, enable `hnsw.iterative_scan` and test recall under the tenant filter.

### 5. Object storage

- Two **private** Cloudflare R2 buckets: `quotepilot-dev` for development and `quotepilot-prod` for production. In each bucket, keys use `tenants/{organization_id}/rfqs/...` and `tenants/{organization_id}/quotes/...`.
- Only `core-api` talks to Cloudflare R2, through its S3-compatible API with server-side, environment-specific credentials. `ai-engine` receives no R2 credentials. Because those credentials sit outside PostgreSQL RLS, **tenant isolation in storage is enforced by `core-api`**: it builds every key from the authenticated tenant, and hands out short-lived presigned URLs only after a permission check.
- Buckets stay private; browser and Supabase `anon`/`authenticated` roles receive no direct bucket credentials.
- The R2 client sits behind a project-owned S3-compatible storage interface (AGENTS.md, Dependencies), so callers do not depend on a provider-specific SDK.
- To confirm in the spike: presigned GET/PUT URLs work against the Cloudflare R2 S3 endpoint with our client library, in both buckets.

### 6. Environments

- **Local and CI:** `docker-compose.yml` with image `pgvector/pgvector:0.8.2-pg16` (pinned; bump deliberately, since section 4 needs pgvector 0.8 or newer). The init script creates the same roles and schemas as production, so the isolation suite runs against the real role model. The Supabase local stack is not required.
  - The port is bound to localhost only.
  - The healthcheck also requires the `qp_app` role, so a failed first init shows as unhealthy.
  - The init script runs on first volume init only; `make db-reset` recreates the volume.
  - Passwords come only from `.env`.
  - Locally, the `CONNECT` revoke covers only the application database: `postgres` and `template1` still grant `PUBLIC` `CONNECT` (and `postgres` also `TEMPORARY`), and any per-test database must repeat the `REVOKE`.
- **Hosted development:** PostgreSQL 16 with `pgvector` on Railway, with `vector` and `pg_trgm` in schema `extensions`, and schemas `app`/`river` owned by `qp_migrator`.
- **Hosted production:** Supabase PostgreSQL 16 project `quotepilot-prod` in `sa-east-1` (São Paulo), with the Data API and `pg_graphql` disabled. R2 uses the matching `quotepilot-dev` and `quotepilot-prod` buckets. Provisioning, accounts, costs and access steps remain pending ADR-007.
- **Hosted differences on Supabase** (from Supabase documentation and experience, not verified in this repo; confirm at provisioning). The init script targets the local container only; hosted provisioning is part of ADR-007:
  - `postgres` is not a superuser on Supabase. On Postgres 16+, role membership carries `ADMIN`/`INHERIT`/`SET` options; `postgres` needs membership in `qp_migrator` with the `SET` option to run `CREATE SCHEMA ... AUTHORIZATION qp_migrator` and `ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator`. Confirm the exact `GRANT` at provisioning.
  - Schema `extensions` already exists; do not create it. `GRANT USAGE ON SCHEMA extensions TO qp_migrator, qp_app` is required unless `PUBLIC` already has `USAGE` (confirm at provisioning).
  - The three Supabase roles already exist; the stand-ins are local only.
  - Revoke only `TEMPORARY` from `PUBLIC`. Supabase-managed roles need `CONNECT`, so the `CONNECT` revoke is local-only. Revoking `TEMPORARY` may affect Supabase-managed roles that rely on it; check at provisioning.
  - How much of this applies to Railway's managed Postgres (its bootstrap role, database ownership and default `PUBLIC` grants) is unverified; the Railway provisioning runbook in ADR-007 must establish it.
- **Region:** the production database region is `sa-east-1` (São Paulo), per product spec v1.2 section 4.5. Tenant data (rates, customers, margins) stays in that project. The Supabase project previously visible in Pedro's connected account belongs to another project (`us-east-1`) and is not reused. Record the Railway development region and R2 bucket placement in ADR-007.
- Free-tier projects have historically been paused after a period of inactivity (not re-verified here; check the current provider policies), and backups depend on the plan. The development database used for demos must not pause, and the final on 12/12 must not depend on an inactive project. Plan and backup choices remain pending ADR-007.

### 7. Account, secrets, backups

- The Supabase organization, and likewise the Railway and Cloudflare accounts, are owned by an account the team controls, with Marcelo and Amanda invited at developer level. A personal account as sole owner is a bus-factor risk.
- Railway and Supabase connection strings and environment-specific R2 S3 keys live in the secret store / GitHub environments; `.env.example` holds names only. Rotate exposed R2 keys and role passwords if they are ever pasted into a chat or a log.
- Confirm backup and restore capabilities for both Railway development and Supabase production in ADR-007. Before the pilot, run one production restore test into a scratch database; production PITR depends on the selected plan.

## Alternatives considered

1. **Self-host Postgres (VM or container).** Full control of roles and no BaaS surface, but we would own backups, upgrades, TLS and monitoring. Rejected for a three-person, four-week team.
2. **Another managed Postgres (AWS RDS, Neon, Cloud SQL).** Equally valid and reachable by the same `pgx` connection. The selected topology uses Railway for development and Supabase for production; these remain the exit routes (see Reversibility).
3. **Use Supabase fully (Data API plus Auth plus RLS from the browser).** Fastest for CRUD, but it moves tenant isolation into policies written against JWT claims and exposes the schema to the internet, and it is incompatible with ADR-001's rule that only `core-api` touches the database.
4. **Supabase Storage for files.** The original draft selected one Supabase S3-compatible bucket for everything. Superseded by Cloudflare R2 in product spec v1.2, retaining the private-bucket, tenant-prefix and presigned-URL rules.

## Consequences

**Positive**
- Managed PostgreSQL 16 with `pgvector` in development (Railway) and production (Supabase), plus private S3-compatible R2 buckets, with no database or storage infrastructure to self-host.
- The databases are reachable only by `core-api` and River workers under roles we control, which keeps the RLS story in ADR-001 intact.
- Local, CI and hosted use the same role model, so the isolation tests mean something.

**Negative / risks**
- Supabase defaults favour the exposed-API model. Every guard above (own schema, revokes, disabled Data API, advisors, the zero-rows grant check, the startup assertion) exists to prevent one forgotten default from opening a door. If the guards are not in CI they will rot.
- Session-mode pooling or direct connections cap concurrency at each provider's connection limit; fine for four tenants, a constraint if load grows.
- IPv6/IPv4 mismatch between the host and the Supabase database fails at connect time and is easy to misdiagnose; ADR-007 must record which path each environment uses.
- R2 S3 credentials sit outside PostgreSQL RLS; a leaked bucket credential may expose every tenant's files in that environment. Scope credentials per bucket and keep them server-side.
- The init script runs only on an empty volume, so role-model changes need `make db-reset` locally and a hosted runbook entry.
- Every function in `app` that `qp_app` calls needs an explicit `GRANT EXECUTE`, including implicit call sites (functions used in RLS policy expressions, column defaults, `CHECK` constraints and index expressions, and functions called through `sqlc`); forgetting it fails with a permission error at DML time.
- psql may echo a failed `CREATE ROLE` statement, including the interpolated password, into the container log. Rotate the password if that happens.

**Reversibility.** Standard Postgres wire protocol and the S3 API only. Moving either database means `pg_dump`/restore into another PostgreSQL 16 with `vector` and changing its connection setting; moving files means copying the relevant R2 bucket and changing the storage endpoint and credentials. No provider-specific database SQL or storage SDK is used.

## Verification at provisioning (acceptance for this ADR)

1. `SELECT rolname, rolsuper, rolbypassrls FROM pg_roles WHERE rolname IN ('qp_app','qp_migrator')` shows both false, on each hosted database (asserted in `infra/postgres/checks/roles.sql`).
2. The grants query in section 2 returns zero rows (`infra/postgres/checks/roles.sql`).
3. On Supabase production, the Data API and `pg_graphql` are disabled; advisors report no RLS or exposure findings on `app`.
4. `vector` is 0.8 or newer and `pg_trgm` is present, on each hosted database.
5. The spike's isolation suite passes against the Railway development and Supabase production databases as well as the local one.
6. Presigned upload and download round-trips work through the Cloudflare R2 S3 endpoint in `quotepilot-dev` and `quotepilot-prod`.
7. Default privileges cover sequences (granted to `qp_app`) and functions (closed in `app`, open to `qp_app` in `river`). This is asserted by tags K1a, K1b, K2, K3, K4, K5 and [X] in `infra/postgres/checks/roles.sql` and exercised by `make db-check-negative`. Optional inspection query: `SELECT defaclrole::regrole, defaclnamespace::regnamespace, defaclobjtype, defaclacl FROM pg_default_acl;`.
8. `has_database_privilege` shows `CONNECT` true and `TEMPORARY` false for both `qp_app` and `qp_migrator`.
9. Revoking `TEMPORARY` from `PUBLIC` on the hosted Supabase database does not break Supabase-managed roles that rely on it.

## Open items

- Railway development region and compute size; Supabase production compute size; account owner, payment and plans (Pedro, with the team). The production database region is fixed at `sa-east-1`. **Verified 2026-11-08:** `sa-east-1` (São Paulo) is Supabase's only South America region. Free plans pause after ~7 days of low activity (one-click restore, kept restorable for a year), so any Supabase project used for demos or the pilot must be Pro (~$25/mo); disposable developer projects can stay Free.
- Which accounts own the providers (section 7). Proposed default: a team-controlled NewByte account owns the Supabase organization, with Marcelo and Amanda invited at developer level; the same model applies to Railway and Cloudflare. Pedro confirms at provisioning time.
- Exact connection-limit numbers for the chosen compute sizes. **Verified (Supabase):** Nano (the default compute) allows 60 direct and 200 shared-pooler connections; raising pooler limits needs a larger compute add-on. Size `pgxpool` well under 60 per role and use Supavisor session mode — which River needs anyway for LISTEN/NOTIFY. Railway's development limits are unverified; record them in ADR-007.
- Whether the `roles.sql` assertions and the section 6 hosted differences apply unchanged to Railway's managed Postgres (its bootstrap role, database owner and default `PUBLIC` grants are unverified; the Supabase roles `anon`, `authenticated`, `service_role` do not exist there).
- Migrations tool and how migrations are applied in CI (ADR-002, ADR-007).
- The privileged pre-tenant role (ADR-005).
- Confirm whether River's schema uses `bigserial` (for example its job id) and which River version ADR-002 pins.
- River's periodic reindexer needs index ownership: disable it or accept `must be owner of index` log lines.
- Railway development and Supabase production provisioning runbooks, including the Cloudflare R2 buckets (ADR-007).
- Hosted PostgreSQL major versions: confirm Railway development and Supabase production use 16 at provisioning; record any mismatch with the pinned local image.

## Revision notes: changes and additions (2026-11-07)

The local role model was implemented in PR #2 (`infra/postgres/`). Departures from the first draft, approved by Pedro:

1. Default privileges on sequences for `qp_app`: `serial`/`bigserial` columns (for example River's job id, to be confirmed against the pinned River version) fail on `INSERT` without sequence `USAGE`.
2. Extensions created by the bootstrap superuser (an addition: the first draft did not say who creates extensions). `qp_migrator` has no database-level `CREATE` and no `CREATE` on schema `extensions`, and pgvector is not marked trusted in its control file (`pg_trgm` is, but it still needs database `CREATE`).
3. `TEMPORARY` (and, locally, `CONNECT`) revoked from `PUBLIC`: otherwise `qp_app` can create `pg_temp` tables that shadow `app.*` names. `CONNECT` is also revoked locally as defence in depth: only the two application roles can connect to the application database (local only; hosted Supabase roles need `CONNECT`).
4. Supabase role stand-ins (`anon`, `authenticated`, `service_role`) exist locally only, so the section 2 check runs identically on hosted and local.
5. Global `REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC` with schema `river` exempt: keeps ADR-005 `SECURITY DEFINER` functions closed by default, while River's helper function stays callable.
6. `search_path` pinned per role (`app, extensions`): removes dependence on session defaults; River migrations set their schema explicitly.
7. `qp_migrator` gained explicit `NOSUPERUSER NOCREATEDB NOCREATEROLE` (the first draft only said "not superuser"), and the sketch now creates schema `river`.

## Revision note: topology aligned with product spec v1.2 (2026-11-09)

Pedro confirmed development PostgreSQL 16 with `pgvector` on Railway, production PostgreSQL 16 on Supabase in `sa-east-1` with the Data API and `pg_graphql` disabled, and private Cloudflare R2 buckets `quotepilot-dev` and `quotepilot-prod`. This supersedes the first draft's single Supabase Storage bucket and shared Supabase staging project. The role/RLS controls and the `core-api`-only access rule remain; hosted provisioning details belong to ADR-007. This ADR remains On-Hold.
