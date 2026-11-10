# ADR-007: CI/CD, environments and hosted database provisioning

- **Status:** Proposed
- **Date:** 2026-11-08
- **Revised:** 2026-11-09 (topology fixed by product spec v1.2: Railway + Supabase + Cloudflare)
- **Deciders:** Pedro (backend lead). Review requested from Marcelo and Amanda.
- **Related:** ADR-001 (two runtimes, contracts-first), ADR-002 (Go stack, `go.mod` is the version record), ADR-003 (roles, pooler, extensions; its section 6 defines the hosted differences this runbook executes — the ADR itself is on hold pending this topology), ADR-004 (River), ADR-008 (what each CI layer proves), RFC-003 (Devin repair loop and its token/scope limits), product spec v1.2 section 4.5 (provider topology)

## Context

`AGENTS.md` fixes the required check names (`lint`, `test`, `integration-test`, `contracts-drift`, `migrations`, `security`, `devin-scope`, `build`), merge-commit-only history, and the rule that checks on stub apps must pass trivially rather than be skipped, so branch protection can require them from day one. It leaves undecided: the image registry, what the dev and staging environments are, the deploy mechanism, where secrets live, and how the hosted databases are provisioned. That is this ADR.

Product spec v1.2 (section 4.5) has since fixed the provider topology: **Railway runs the services and the dev Postgres; production data lives on Supabase `quotepilot-prod` in `sa-east-1`; `apps/web` ships to Cloudflare Pages; object storage is Cloudflare R2 (`quotepilot-dev` / `quotepilot-prod` buckets).** This ADR records the deploy and provisioning design inside that fixed topology rather than choosing a host. The earlier revision of this file proposed Fly.io `gru`; it is kept below as the documented alternative.

Until this ADR is accepted, no deploy jobs exist (`AGENTS.md`, CI/CD). The checks themselves are delivered on `ci/pipeline`.

## Decision

### 1. Registry

**`ghcr.io`** (GitHub Container Registry). Same account as the repository, `GITHUB_TOKEN` authentication in Actions with `packages: write`, no extra credentials to rotate. Images: `ghcr.io/newbytegroup/quotepilot-core-api`, `-ai-engine`, `-web`, tagged with the git SHA and `dev` / `staging` moving tags.

### 2. Environments and deploy mechanism

Two hosted environments, matching the two deploy triggers in `AGENTS.md`:

| Environment | Trigger | Purpose | Services | Postgres | Object storage |
|---|---|---|---|---|---|
| `dev` | push to `develop` (images built, pushed, deployed automatically) | Integration and dogfooding | Railway | Railway Postgres service in the dev project (pgvector) | R2 `quotepilot-dev` |
| `staging` | push to `main` | Demo and pre-final; the 12/12 environment; the pilot's production path | Railway | Supabase `quotepilot-prod` (`sa-east-1`) | R2 `quotepilot-prod` |

Spec v1.2 defines one production-shaped data path: what `main` deploys runs against the Supabase `quotepilot-prod` project, so the 12/12 demo and the pilot exercise the real hosted topology. `dev` is separated by infrastructure, not just by data — a different database service and a different bucket entirely.

Both environments deploy the same image set to Railway services; the environment differs only in its variables and which hosted database it points at. `ai-engine` is reachable only over Railway's private networking from `core-api` — it is never publicly exposed (ADR-001).

**Deploy mechanism (spec-fixed): Railway for services, Cloudflare Pages for `apps/web`, Cloudflare R2 for object storage.** Deploy is `railway up` (redeploy of the service's pinned `ghcr.io` image tag) from a GitHub Actions job gated on the environment; `apps/web` publishes to Cloudflare Pages on push. The known cost of this topology — no Railway region in South America — is documented under Consequences, with Fly.io `gru` kept as the documented alternative.

### 3. Secrets

- Only GitHub **environments** (`dev`, `staging`) hold deploy-time secrets: `POSTGRES_DSN`, `MIGRATOR_DSN`, `AI_ENGINE_SERVICE_TOKEN`, Resend API key and webhook signing secret, Firecrawl key, R2 (S3-compatible) access keys, JWT signing keys, `RAILWAY_TOKEN`.
- `RAILWAY_TOKEN` is scoped to the team workspace or the QuotePilot project, never a personal-account token — decide project-vs-workspace scoping when it is minted (open items).
- Workflows reference secrets with `environment:` on the job; they are never echoed, never written into the repo, and `.env.example` continues to hold names only (ADR-003 section 7).
- The Devin repair loop keeps its own boundary: its token opens PRs only, reads no secrets, and `devin-scope` is a required check (RFC-003 section 6).
- Rotation: any credential pasted into a chat or log is rotated; role passwords and storage keys rotate on suspicion, not schedule.

### 4. IPv4/IPv6 path per environment (records ADR-003's open item)

| Path | Transport | Why |
|---|---|---|
| `core-api` / River workers on Railway → Railway dev Postgres | **direct** over Railway's private network | Same-project Postgres: no pooler in the path, `LISTEN/NOTIFY` and prepared statements just work |
| `core-api` / River workers on Railway → Supabase `quotepilot-prod` | **Supavisor session-mode pooler** (IPv4) | The safe documented default: Railway egress IPv6 is unverified, and session mode still supports `LISTEN/NOTIFY`, `SET` and prepared statements (ADR-003 section 3) |
| CI (GitHub runners) → hosted DB, if ever needed | session-mode pooler (IPv4) | Runners are IPv4-only historically; CI normally uses the service container, not hosted |
| Migrations (`cmd/migrate`, `qp_migrator`) | direct on the Railway dev Postgres; session mode on Supabase | Never transaction mode (ADR-003 section 3) |
| Local dev | `127.0.0.1` to the compose container | — |

Verify at provisioning: if Railway's egress actually carries IPv6, the prod path flips to the **direct IPv6 connection** to Supabase's IPv6-only direct endpoint — a connection-string change, not code (ADR-003 section 3; transaction mode remains the last resort with simple protocol and River poll-only).

### 5. Hosted database provisioning runbook

Two hosted Postgres paths exist under this topology: the **Railway dev Postgres** and the **Supabase `quotepilot-prod`** project. This is the executable form of ADR-003 section 6, written for the Supabase path; where the same statements apply to the Railway dev Postgres it is called out. Run once per hosted database, with passwords from the secret store — never from the repo.

1. **Create the projects.** Supabase `quotepilot-prod`: organization owned by the team account, region `sa-east-1` (São Paulo — verified as Supabase's only South America region), plan **Pro** (~$25/mo — the Free plan pauses after ~7 days of low activity and the demo/pilot project must not pause; Nano compute: 60 direct / 200 shared-pooler connections). Railway: one project holding the dev Postgres service and the `dev` environment's services; record Railway region, plan and connection limits in the config (open items).
2. **Disable the Data API and `pg_graphql`** in the Supabase project settings (ADR-003 section 2). Not applicable to the Railway Postgres — it exposes no Data API.
3. **Roles.** `postgres` is not a superuser on Supabase: create `qp_migrator` and `qp_app` (`LOGIN NOBYPASSRLS NOSUPERUSER NOCREATEDB NOCREATEROLE`), then `GRANT qp_migrator TO postgres` with the `SET` option so `postgres` can run `CREATE SCHEMA ... AUTHORIZATION qp_migrator` and `ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator`. Confirm the exact grant syntax at provisioning (Postgres 16 membership options). **On the Railway dev Postgres this membership trick is Supabase-shaped and unverified:** what the Railway-provided `postgres` user may do — real superuser or not — decides whether the grant is needed at all. Record the answer at provisioning (open item).
4. **Extensions.** Schema `extensions` already exists on Supabase — do not create it. `GRANT USAGE ON SCHEMA extensions TO qp_migrator, qp_app` if `PUBLIC` does not already have it; `CREATE EXTENSION vector` (≥ 0.8) and `pg_trgm` there as `postgres` (ADR-003 section 4). On the Railway dev Postgres, create the `extensions` schema as on local, after verifying its managed Postgres image actually ships `vector` ≥ 0.8 (open item).
5. **Schemas and privileges.** Create `app` and `river` `AUTHORIZATION qp_migrator`; revoke from `PUBLIC`, `anon`, `authenticated`, `service_role`; default privileges granting `qp_app` DML, sequence `USAGE`, `EXECUTE` on `river` functions only; global `REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC`; pin `search_path = app, extensions` on both roles. The statements mirror `infra/postgres/init/01-roles-and-schemas.sh`. **Supabase-hosted differences:** revoke `TEMPORARY` only — do not revoke `CONNECT` (Supabase-managed roles need it); no stand-in roles (`anon`, `authenticated`, `service_role` exist for real); no `extensions` schema creation. **Railway dev:** the Supabase-managed roles do not exist, so the stand-in model applies as on local — gated on the step-3 question of what the provided user may grant.
6. **Storage.** Cloudflare R2 buckets `quotepilot-dev` and `quotepilot-prod`, private; S3-compatible access keys into the GitHub environments (ADR-003 section 5). R2 account and key provisioning is an open item.
7. **Verify.** Run `infra/postgres/checks/roles.sql` against `quotepilot-prod` with the exception list below, then the Supabase security and performance advisors — RLS or exposure findings are failing checks (ADR-003 section 2). Run `roles.sql` against the Railway dev Postgres too; its exception list is written once Railway's managed-role reality is known (open item).

**`roles.sql` hosted exception list** (for `quotepilot-prod`; tracked here per ADR-003 section 2):

| Assertion | Exception | Reason |
|---|---|---|
| `[B3]` (nobody is a member of `qp_migrator`/`qp_app`) | hosted `postgres` membership in `qp_migrator` | Required for `CREATE SCHEMA ... AUTHORIZATION` and `ALTER DEFAULT PRIVILEGES` (step 3) |
| `[X]` (grantee allow-list) | Supabase-managed roles present on hosted (`supabase_admin`, `pgbouncer`, etc.) | They exist on every hosted project; the allow-list gains a named hosted section, enumerated at provisioning |
| `[G]` (CONNECT granted, TEMPORARY revoked for our roles) | none — must still pass | Acceptance item 8 of ADR-003. If `TEMPORARY` cannot be revoked from `PUBLIC` on hosted (revoking it may affect Supabase-managed roles; unverified), [G] and that acceptance item are revisited together in this ADR |

8. **Pool sizing.** `pgxpool` max for `core-api` plus workers stays below each database's connection limit — for Supabase, minus its reserved connections (ADR-003 section 3). Record the numbers next to each DSN in the environment config; re-check when a plan changes.

### 6. Deploy job shape (once accepted)

- `push` to `develop`: build → push `ghcr.io` images tagged `<sha>` + `dev` → deploy job with `environment: dev` → `railway up` / service redeploy on `ghcr.io/...:<sha>` per app → `cmd/migrate` as `qp_migrator` (via `railway run` or the service's pre-deploy command) before the new version takes traffic.
- `push` to `main`: same flow against `staging`, promoting the exact image SHAs that `develop` last built (re-tag, never rebuild) so staging runs what dev proved.
- `apps/web` publishes to Cloudflare Pages — production on `main`, preview on `develop` (a `cloudflare-workers-and-pages` check already fires on this repo; wiring is an open item).
- Migrations always run before the new version goes live; a failed migration stops the deploy.

## Alternatives considered

Product spec v1.2 fixes the topology, so these are documented fallbacks, not candidates.

1. **Fly.io `gru` (São Paulo).** Verified ~$12/mo for two always-on services (`core-api` + `ai-engine`, 512 MB shared-cpu-1x each), the same region as Supabase `sa-east-1`, IPv6 egress enabling the direct database connection (no pooler in the path), managed TLS, and private service-to-service networking. **The documented alternative if the Railway→`sa-east-1` latency (Consequences) proves a problem in the pilot:** same images, same Supabase project, only the service host changes.
2. **A São Paulo VPS + Caddy + compose** (AWS Lightsail ~$24/mo, ~2 vCPU/4 GB). Identical runtime shape to local dev and CI, `ssh` + `docker compose pull && up -d` deploys, IPv6 direct connection, full control — at ~2× the Fly.io price plus full OS-patch responsibility. Reverts cleanly: same images, same database.
3. **Supabase for everything (Edge Functions, hosted workers).** Conflicts with ADR-001's boundary: `ai-engine` must stay a separately deployable runtime and only `core-api` may hold database credentials.
4. **A Kubernetes cluster or any orchestrator.** Absurd for 4 tenants and a 4-week MVP.

## Consequences

**Positive**

- Dev and CI prove the same artifacts staging runs (SHA-tagged images promoted, never rebuilt).
- The hosted provisioning is a checklist, not tribal knowledge — and `roles.sql` plus the exception list verify it mechanically.
- The dev path (Railway service → Railway Postgres over the private network) needs no pooler at all.
- The provider debate is settled by spec v1.2: one decision record instead of three independent hosting choices.

**Negative / risks**

- **Railway has no São Paulo region** — the closest is `us-east4` (Virginia). Railway services → Supabase `sa-east-1` therefore pay roughly **120–150 ms of RTT per query round trip**, and one pipeline stage can need several. Spec v1.2 accepted this cost; mitigation is pool sizing plus keeping per-request round trips few. **If latency proves a problem in the pilot, Fly.io `gru` (~$12/mo, same region as the database) is the documented alternative** (Alternatives, item 1).
- The session-mode pooler now sits in the prod data path because Railway egress IPv6 is unverified — a variable the direct connection avoids. The provisioning check in section 4 decides which path prod actually runs.
- Railway's managed Postgres role and extension surface is unverified; the runbook may grow a second exception list (open items).
- R2 and Pages add a second cloud vendor's provisioning and key-rotation surface.
- The `roles.sql` exception list can drift from hosted reality. Mitigation: every exception is named and justified in the table; re-run the check after any provider-side change.

## Open items

- **Railway details:** region (closest to the data is `us-east4`; confirm at provisioning), plan sizing for two services plus the dev Postgres, and which team account owns the workspace/project. `RAILWAY_TOKEN` scoping (project vs team/workspace token) is decided when it is minted.
- **Cloudflare Pages wiring for `apps/web`:** a `cloudflare-workers-and-pages` check already fires on this repo (the Cloudflare GitHub App is installed); build root `apps/web`, the build command, and the preview (`develop`) vs production (`main`) branch mapping still need confirming.
- **R2 provisioning:** Cloudflare account, and S3 API access keys for `quotepilot-dev` / `quotepilot-prod` (least-privilege per bucket where supported) into the GitHub environments.
- **RLS runbook on the Railway dev Postgres:** what the Railway-provided `postgres` user may grant (superuser or not — the `postgres`-membership trick in step 3 is Supabase-shaped), whether its managed Postgres image ships `pgvector` ≥ 0.8, and the resulting `roles.sql` exception list.
- **Monitoring/alerting:** proposed default — a hosted free tier (Better Stack: 10 monitors, 3-minute checks, one status page, Slack/email alerts) watching both environments' health endpoints. Hosted over self-hosted Uptime Kuma on purpose, so alerts still fire when the host itself is dead.
- Web hosting is no longer an open decision — product spec v1.2 fixes Cloudflare Pages. What remains is the wiring item above and the registrable-domain question it raises for CSRF (ADR-005 section 4).

## Verification at provisioning (acceptance for this ADR)

1. `roles.sql` passes on `quotepilot-prod` with only the named exceptions; the Railway dev Postgres run is recorded with its own exception list once known.
2. `SELECT extversion FROM pg_extension WHERE extname='vector'` returns ≥ 0.8 on both hosted databases.
3. Data API and `pg_graphql` disabled on Supabase; advisors report no RLS or exposure findings on `app`.
4. From a Railway service: `psql` reaches `quotepilot-prod` over the session-mode pooler; `pgx` prepared statements and River `LISTEN/NOTIFY` work end to end; the IPv6-egress check runs and its result (direct connection usable or not) is recorded.
5. R2 buckets `quotepilot-dev` and `quotepilot-prod` accept writes with the provisioned keys, scoped per bucket.
6. One `dev` deploy runs end to end from a push to `develop` — build → `ghcr.io` → `railway up` → migrations — and `apps/web` publishes to Cloudflare Pages.
