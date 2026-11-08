# ADR-007: CI/CD, environments and hosted database provisioning

- **Status:** Proposed
- **Date:** 2026-11-08
- **Deciders:** Pedro (backend lead). Review requested from Marcelo and Amanda (web hosting is Amanda's call).
- **Related:** ADR-001 (two runtimes, contracts-first), ADR-002 (Go stack, `go.mod` is the version record), ADR-003 (Supabase roles, pooler, extensions; its section 6 defines the hosted differences this runbook executes), ADR-004 (River), ADR-008 (what each CI layer proves), RFC-003 (Devin repair loop and its token/scope limits)

## Context

`AGENTS.md` fixes the required check names (`lint`, `test`, `integration-test`, `contracts-drift`, `migrations`, `security`, `devin-scope`, `build`), merge-commit-only history, and the rule that checks on stub apps must pass trivially rather than be skipped, so branch protection can require them from day one. It leaves undecided: the image registry, what the dev and staging environments are, the deploy mechanism, where secrets live, and how the hosted Supabase project is provisioned. That is this ADR.

Until this ADR is accepted, no deploy jobs exist (`AGENTS.md`, CI/CD). The checks themselves are delivered on `ci/pipeline`.

## Decision

### 1. Registry

**`ghcr.io`** (GitHub Container Registry). Same account as the repository, `GITHUB_TOKEN` authentication in Actions with `packages: write`, no extra credentials to rotate. Images: `ghcr.io/newbytegroup/quotepilot-core-api`, `-ai-engine`, `-web`, tagged with the git SHA and `dev` / `staging` moving tags.

### 2. Environments and deploy mechanism

Two hosted environments, matching the two deploy triggers in `AGENTS.md`:

| Environment | Trigger | Purpose | Supabase project |
|---|---|---|---|
| `dev` | push to `develop` (images built, pushed, deployed automatically) | Integration and dogfooding | `quotepilot-staging` (shared, section 5) |
| `staging` | push to `main` | Demo and pre-final; the 12/12 environment | `quotepilot-staging` until the production project exists |

Production is created before the pilot tenants go live (ADR-003 section 6); until then `staging` and `dev` share the one hosted project, separated by database *data*, not infrastructure. Both deploy the same compose stack; the environment differs only in its variables.

**Recommended mechanism: one VPS running `docker compose`, fronted by Caddy for TLS.** Reasons: it is the same runtime shape as local dev and CI (`docker-compose.yml` already exists), a three-person team can debug it, and a VPS with both IPv4 and IPv6 can reach the hosted database over the **direct connection** (ADR-003 section 3) instead of the pooler. `core-api` and `ai-engine` run as two services on the VPS's private Docker network; `ai-engine` is never exposed publicly. Deploy is `ssh` + `docker compose pull && up -d` from a GitHub Actions job gated on the environment.

**Alternatives documented:** Fly.io (also IPv6-capable, less to manage, but another platform to learn and its machines model is one more thing at 2 a.m.); Render/Railway (IPv4-only egress in the plans we would use, which forces the session-mode pooler path for every request — viable fallback, not the default). If the VPS proves a burden during the online phase we move to Fly.io without changing the images or the database.

`apps/web` hosting is **Amanda's decision** (current assumption: Vercel; Vercel is IPv4-only, which only matters for server-side calls from the web app — the browser talks to `core-api` directly).

### 3. Secrets

- Only GitHub **environments** (`dev`, `staging`) hold deploy-time secrets: `POSTGRES_DSN`, `MIGRATOR_DSN`, `AI_ENGINE_SERVICE_TOKEN`, Resend API key and webhook signing secret, Firecrawl key, S3 access keys, JWT signing keys, the VPS `SSH_PRIVATE_KEY` and host.
- Workflows reference secrets with `environment:` on the job; they are never echoed, never written into the repo, and `.env.example` continues to hold names only (ADR-003 section 7).
- The Devin repair loop keeps its own boundary: its token opens PRs only, reads no secrets, and `devin-scope` is a required check (RFC-003 section 6).
- Rotation: any credential pasted into a chat or log is rotated; role passwords and S3 keys rotate on suspicion, not schedule.

### 4. IPv4/IPv6 path per environment (records ADR-003's open item)

| Path | Transport | Why |
|---|---|---|
| `core-api` / River workers on the VPS → hosted DB | **direct connection** (IPv6 available on the host) | No pooler in the path; River `LISTEN/NOTIFY` and prepared statements just work |
| CI (GitHub runners) → hosted DB, if ever needed | session-mode pooler (IPv4) | Runners are IPv4-only historically; CI normally uses the service container, not hosted |
| Migrations (`cmd/migrate`, `qp_migrator`) | direct when reachable, else session mode | Never transaction mode (ADR-003 section 3) |
| Local dev | `127.0.0.1` to the compose container | — |

If a host ever loses IPv6, the fallback is session-mode pooler — a connection-string change, not code (ADR-003 section 3; transaction mode remains the last resort with simple protocol and River poll-only).

### 5. Hosted database provisioning runbook

This is the executable form of ADR-003 section 6. Run once per hosted project, as the Supabase `postgres` user, with passwords from the secret store — never from the repo.

1. **Create the project** (`quotepilot-staging` first): organization owned by the team account, region closest to `core-api` and the pilot users (São Paulo if offered), a plan that does not pause and has daily backups. Record region, plan and connection limits in the config.
2. **Disable the Data API and `pg_graphql`** in project settings (ADR-003 section 2).
3. **Roles.** `postgres` is not a superuser on Supabase: create `qp_migrator` and `qp_app` (`LOGIN NOBYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE`), then `GRANT qp_migrator TO postgres` with the `SET` option so `postgres` can run `CREATE SCHEMA ... AUTHORIZATION qp_migrator` and `ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator`. Confirm the exact grant syntax at provisioning (Postgres 16 membership options).
4. **Extensions.** Schema `extensions` already exists on hosted — do not create it. `GRANT USAGE ON SCHEMA extensions TO qp_migrator, qp_app` if `PUBLIC` does not already have it; `CREATE EXTENSION vector` (≥ 0.8) and `pg_trgm` there as `postgres` (ADR-003 section 4).
5. **Schemas and privileges.** Create `app` and `river` `AUTHORIZATION qp_migrator`; revoke from `PUBLIC`, `anon`, `authenticated`, `service_role`; default privileges granting `qp_app` DML, sequence `USAGE`, `EXECUTE` on `river` functions only; global `REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC`; pin `search_path = app, extensions` on both roles. The statements mirror `infra/postgres/init/01-roles-and-schemas.sh`, with these hosted differences applied: **revoke `TEMPORARY` only — do not revoke `CONNECT`** (Supabase-managed roles need it); no stand-in roles (`anon`, `authenticated`, `service_role` exist for real); no `extensions` schema creation.
6. **Storage.** One private bucket `tenants`; S3 access keys into the GitHub environments (ADR-003 section 5).
7. **Verify.** Run `infra/postgres/checks/roles.sql` against the hosted project with the exception list below, then the Supabase security and performance advisors — RLS or exposure findings are failing checks (ADR-003 section 2).

**`roles.sql` hosted exception list** (tracked here per ADR-003 section 2):

| Assertion | Exception | Reason |
|---|---|---|
| `[B3]` (nobody is a member of `qp_migrator`/`qp_app`) | hosted `postgres` membership in `qp_migrator` | Required for `CREATE SCHEMA ... AUTHORIZATION` and `ALTER DEFAULT PRIVILEGES` (step 3) |
| `[X]` (grantee allow-list) | Supabase-managed roles present on hosted (`supabase_admin`, `pgbouncer`, etc.) | They exist on every hosted project; the allow-list gains a named hosted section, enumerated at provisioning |
| `[G]` (CONNECT granted, TEMPORARY revoked for our roles) | none — must still pass | Acceptance item 8 of ADR-003. If `TEMPORARY` cannot be revoked from `PUBLIC` on hosted (revoking it may affect Supabase-managed roles; unverified), [G] and that acceptance item are revisited together in this ADR |

8. **Pool sizing.** `pgxpool` max for `core-api` plus workers stays below the project's connection limit minus Supabase's reserved connections (ADR-003 section 3). Record the numbers next to the DSN in the environment config; re-check when the plan changes.

### 6. Deploy job shape (once accepted)

- `push` to `develop`: build → push `ghcr.io` images tagged `<sha>` + `dev` → deploy job with `environment: dev` → `ssh` → `docker compose pull && up -d` → `cmd/migrate` as `qp_migrator` before the new containers start.
- `push` to `main`: same flow against `staging`, promoting the exact image SHAs that `develop` last built (re-tag, never rebuild) so staging runs what dev proved.
- Migrations always run before the new version's containers; a failed migration stops the deploy.

## Alternatives considered

1. **Fly.io for the app services.** Strong fit (IPv6, Docker-native, scale-to-zero for a quiet dev environment). Kept as the documented alternative because a VPS + the existing compose file is the smallest step from local dev, and the team's remaining unknown is deploy mechanics, not platform features.
2. **Render or Railway.** Easiest git-push deploys, but IPv4-only egress forces the session-mode pooler for all database traffic. Acceptable fallback; not chosen while IPv6 hosting is available.
3. **Supabase for everything (Edge Functions, hosted workers).** Conflicts with ADR-001's boundary: `ai-engine` must stay a separately deployable runtime and only `core-api` may hold database credentials.
4. **A Kubernetes cluster or any orchestrator.** Absurd for 4 tenants and a 4-week MVP.

## Consequences

**Positive**
- Dev and CI prove the same artifacts staging runs (SHA-tagged images promoted, never rebuilt).
- The hosted provisioning is a checklist, not tribal knowledge — and `roles.sql` plus the exception list verify it mechanically.
- The direct-connection path removes the pooler as a variable for River and prepared statements.

**Negative / risks**
- A VPS is ours to patch and watch. Acceptable for the MVP window; monitoring is Caddy access logs plus container health until observability lands.
- The `roles.sql` exception list can drift from hosted reality. Mitigation: every exception is named and justified in the table; re-run the check after any Supabase-side change.
- `staging` and `dev` sharing one Supabase project risks cross-talk in demos. Mitigation: they deploy the same schema; data separation is by usage, and production separates before real tenants.

## Open items

- VPS provider and size (a small Hetzner-class instance is the default; Pedro picks and provisions).
- Whether the 12/12 demo runs `staging` or a freshly minted production project (decide with the pilot timeline).
- Monitoring/alerting for the hosted environments (deferred; revisit after the spike).
- Amanda's call on `apps/web` hosting (Vercel assumed) and the registrable-domain question it raises for CSRF (ADR-005 section 4).

## Verification at provisioning (acceptance for this ADR)

1. `roles.sql` passes on the hosted project with only the named exceptions.
2. `SELECT extversion FROM pg_extension WHERE extname='vector'` returns ≥ 0.8 in schema `extensions`.
3. Data API and `pg_graphql` disabled; advisors report no RLS or exposure findings on `app`.
4. `psql` from the VPS reaches the database over the direct connection; `pgx` prepared statements and River `LISTEN/NOTIFY` work end to end (exercised by the spike or the first deployed build).
5. One `dev` deploy runs `docker compose pull && up -d` end to end from a push to `develop`, migrations included.
