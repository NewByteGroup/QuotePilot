# ADR-002: Go core stack, `InTenantTx` design and the spike gate

- **Status:** Accepted
- **Date:** 2026-11-08
- **Deciders:** Pedro (backend lead). Review requested from Marcelo and Amanda.
- **Related:** ADR-001 (monorepo, Go/Python boundary), ADR-003 (Postgres roles), ADR-004 (River pipeline), ADR-005 (auth and the platform path), ADR-008 (testing)

## Context

ADR-001 chose a Go `core-api` but left the stack open and made the choice conditional on a one-day spike (its Reversibility section). `AGENTS.md` tells every agent not to add a library that is not listed in this ADR, so this document is the allow-list.

Constraints: three people, online phase ends 30/11, final 12/12. The core is a single-owner critical path (Pedro), so every dependency must be one he can debug at 2 a.m. Prefer the standard library, generated code that is checked in CI, and few moving parts.

## Decision

### 1. Stack (allow-list)

| Concern | Choice | Notes |
|---|---|---|
| Language | Go, version pinned in `go.mod` and read by CI (`go-version-file`) | Go 1.22 or later is required: `ServeMux` method and wildcard patterns, and the `std-http-server` generator. |
| HTTP server | `net/http` `ServeMux` | No router library. Middleware is hand-written: request id, panic recovery, structured request log, auth, tenant resolution. |
| API contract | **OpenAPI 3.0.3**, source in `packages/contracts` | Deviation from ADR-001, which says 3.1: `oapi-codegen` does not support 3.1 (it warns when it meets it). Revisit when it does. ADR-001 wording is to be amended. |
| Server stubs | `oapi-codegen` v2, generators `std-http-server` and `strict-server` | Generated code is committed; CI `contracts-drift` regenerates and fails on any diff. |
| Postgres driver | `pgx` v5 with `pgxpool` | |
| Queries | `sqlc`, engine `postgresql`, `sql_package: pgx/v5` | Schema input is the migration files. Queries live beside their module, generated code is committed and covered by `contracts-drift`. |
| Numeric mapping | `shopspring/decimal` with `jackc/pgx-shopspring-decimal` | `NUMERIC` maps to `decimal.Decimal`. No `float64` anywhere near money (`AGENTS.md`). |
| Vectors | `pgvector-go` | Queries always carry an explicit `organization_id` filter as well as RLS (spec section 5.4). |
| Migrations | `goose`, plain SQL, sequential integer versions | Run by a separate `cmd/migrate` binary with the `qp_migrator` DSN. The server never runs migrations at boot and never holds the migrator credential. |
| Job queue | River with `riverpgxv5`, schema `river` | River's own migrations run from `cmd/migrate` with the schema set explicitly (ADR-003 section 1). |
| JWT | `golang-jwt/jwt` v5 | Details in ADR-005. |
| Webhook signatures | Svix's Go library for verification only | Resend signs webhooks with Svix headers. Wrapped behind a small interface. |
| Resend client | `resend-go` v3 behind an interface owned by this project | Same rule as every third party in `AGENTS.md`. Fake implementation for tests. |
| Logging | `log/slog` with the JSON handler | `correlation_id` on every line. Bodies, rates and margins are never logged. |
| Config | Environment variables parsed into one validated struct in `internal/platform` | No config framework. Missing or malformed values fail startup. |
| Tests | `testing` plus `testify` (`require`, `assert`) | Strategy in ADR-008. |
| Lint | `golangci-lint` with `govet`, `staticcheck`, `errcheck`, `gosec`, `rowserrcheck`, `sqlclosecheck`, `depguard` | `depguard` enforces the pool rule below. |
| Vulnerability scan | `govulncheck`, `gosec` | CI `security` job. |

Version numbers are deliberately not pinned in this document; `go.mod` is the record. Anything not in this table needs Pedro's approval and an edit to this ADR.

**Explicitly not used:** an ORM (GORM, ent), a router or web framework (chi, gin, echo), viper, zap or zerolog, Redis, and `testcontainers-go` in the MVP. Integration tests use the Postgres 16 + pgvector service from `docker-compose.yml` locally and a service container in CI, so tests run against the exact role and schema model of ADR-003. `testcontainers-go` stays a possible later swap.

### 2. Package boundaries

```
apps/core-api/
├── cmd/server        # wiring only: config, dependency construction, start
├── cmd/migrate       # goose + rivermigrate, qp_migrator credential only
├── db/migrations     # NNNN_name.sql
└── internal/
    ├── tenantdb      # InTenantTx and the pool. The only tenant path to the database
    ├── platform      # config, logging, auth, pre-tenant (SECURITY DEFINER) calls (ADR-005)
    ├── pricing       # pure: no I/O, decimal only (ADR-004, RFC-001)
    ├── quotes        # state machine, versions, line items
    └── inbox         # ingest, RFQ lifecycle, attachments
```

Rules, enforced by `depguard` and review:

1. `pgxpool` may be imported only by `internal/tenantdb` and `internal/platform`. Every other package receives the interface below, so it cannot open a connection of its own.
2. `internal/pricing` imports no I/O package (`database/sql`, `pgx`, `net/http`, `os`, `time.Now` through an injected clock).
3. Nothing under `internal/` imports `cmd/`.
4. Generated code is never edited by hand.

### 3. `InTenantTx`

```go
type TenantRunner interface {
    InTenantTx(ctx context.Context, org uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error
    InTenantReadTx(ctx context.Context, org uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error
}
```

Behaviour (normative):

1. Reject `uuid.Nil` before touching the database.
2. Begin a transaction. The first statement is `SELECT set_config('app.current_organization_id', $1, true)` with the id as a bind parameter. The id is never formatted into SQL (`AGENTS.md` rule 3). The third argument `true` makes it transaction-local, so a pooled connection carries nothing to the next borrower.
3. Run `fn`. On error, or on panic (recover, roll back, re-panic), roll back; otherwise commit.
4. `InTenantReadTx` uses a read-only transaction. A statement timeout is set with `set_config('statement_timeout', ..., true)`.
5. No nesting. Calling `InTenantTx` from inside `fn` is a programming error and returns an error.
6. A job worker calls it first thing with the `organization_id` from the River envelope (ADR-004). IDs-only job args stay the rule.

RLS policies follow `AGENTS.md` rule 4, including `NULLIF(current_setting('app.current_organization_id', true), '')::uuid`, so a connection with no context yields zero rows instead of an error from `''::uuid`.

### 4. Startup assertions

`tenantdb.Open` connects as `qp_app` and refuses to start unless all of these hold (ADR-003 section 1):

- the role is not a superuser and does not have `BYPASSRLS`;
- the role does not own schema `app`, schema `river` or any table in them;
- every table in `app` that has an `organization_id` column has RLS enabled and forced.

The same facts are asserted again in CI (`infra/postgres/checks/roles.sql` and the schema lint in ADR-008). The runtime check exists so a misconfigured hosted project fails loudly instead of serving data.

### 5. Pool and connection mode

`core-api` and the River workers use the session-mode pooler or the direct connection (ADR-003 section 3). Pool size is configuration and must stay below the pooler's client limit; River's `LISTEN/NOTIFY` needs a session-stable connection. The exact limits are an open item of ADR-003 and are set at provisioning.

### 6. The spike gate (Mission 3)

The spike decides whether this ADR and ADR-001 become Accepted or whether the core flips to Python/FastAPI behind the same contract.

**Time box:** one working day, Pedro, on a `spike/go-core-gate` branch. The branch is never merged as-is (`AGENTS.md`). Following the TDD policy, the six gate tests are written first because they are the pass/fail criteria; the production code is then re-done through normal PRs.

**Pass criteria (all six):**

1. `docker compose up` brings up Postgres 16 with pgvector and the roles of ADR-003.
2. A migration creates two tenant tables with ENABLE and FORCE RLS, owned by `qp_migrator`; `qp_app` is not owner and has no BYPASSRLS.
3. `InTenantTx` plus one `sqlc` query works end to end, including the `NUMERIC` to `decimal` mapping.
4. The isolation tests pass: pooled connection reuse A then B leaks nothing; identical vectors in orgs A and B queried without a `WHERE` return zero rows of B; no tenant context returns zero rows or a clean error.
5. A webhook with a valid signature does an idempotent insert on `(organization_id, resend_message_id)`; an invalid signature is rejected.
6. A River job enqueued in the same transaction runs with tenant context. This also confirms whether River needs `bigserial` sequence grants and how its reindexer behaves without index ownership (both flagged in `infra/postgres/init/01-roles-and-schemas.sh`).

**Decision rule at the end of the day:**

| Outcome | Action |
|---|---|
| All six pass | ADR-002 Accepted. ADR-001 goes to the team for Accepted. |
| One or two fail, the cause is understood and the fix is under half a day | One half-day extension, once. Then re-evaluate against the table. |
| Anything else, or an isolation criterion (2 or 4) still failing after the extension | Switch the core to Python/FastAPI behind the same contract. Write the amendment to ADR-001. The schema, SQL, RLS policies, roles and isolation tests carry over unchanged; only the Go code is lost. |

**Timing:** the online phase ends 30/11. To keep the fallback affordable, the spike should finish within the first week of it. Pedro owns that date.

**Gate outcome (2026-11-10): PASS.** All six criteria verified locally and in the real CI pipeline (`integration-test` applies migrations as `qp_migrator`, then runs the isolation/River suite; `migrations` job rebuilds from scratch; `contracts-drift` proves zero sqlc drift). The spike merged into `develop` as the implementation foundation via the normal review path rather than as-is. Per the table this ADR is Accepted; ADR-001 goes to the team for Accepted.

**Note on agreement:** ADR-006 (AI agents, approved) assumes a Python `ai-engine`, which ADR-001 already fixed. It does not settle the Go-versus-Python question for the core. Marcelo's preference for Python is still unresolved, and a passing spike is the evidence to put in front of him.

## Alternatives considered

1. **A router framework (chi, gin, echo).** Go 1.22's `ServeMux` handles methods and path parameters, and `oapi-codegen` generates for it, so a framework adds a dependency without adding capability here.
2. **An ORM.** Hides the SQL that RLS correctness depends on, and makes the `tenantdb` boundary harder to enforce.
3. **`golang-migrate` or Atlas for migrations.** Both work. `goose` was chosen for plain SQL files with a simple, linear version scheme that the CI `migrations` check can verify. Easy to swap, since migrations are SQL.
4. **`testcontainers-go`.** Nice isolation per test, but it would bypass the role and schema setup that the compose file and CI service container already provide, which is exactly what the isolation tests need to prove.
5. **OpenAPI 3.1.** Preferable on paper, but the generator we rely on does not support it. Using 3.0.3 avoids a hand-written server layer.

## Consequences

**Positive**
- A short, enforced dependency list. Agents cannot quietly add libraries.
- The only way to reach tenant data is `InTenantTx`, and the lint rule makes bypassing it a CI failure rather than a code review catch.
- The spike has numeric pass/fail criteria and a pre-agreed fallback, so the decision is not re-litigated mid-week.

**Negative / risks**
- OpenAPI 3.0.3 loses some 3.1 features (for example, full JSON Schema alignment). Mitigation: keep contracts simple; revisit when the generator catches up.
- `sqlc` needs the schema in a form it can parse. Migrations that rely on constructs it does not understand may need workarounds. The spike exercises this.
- `depguard` stops direct pool imports but not a determined workaround. Code review and the isolation suite remain the real defence.

## Open items

- Exact Go and tool versions (recorded in `go.mod` and the CI config, not here).
- `golangci-lint` configuration file contents.
- Pool size and connection limits for the hosted project (ADR-003).
- Whether `strict-server` suits streaming endpoints such as the SSE status channel in ADR-004; it may need a plain handler for that one route.
