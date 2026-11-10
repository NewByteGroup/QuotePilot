# core-api

Go core service: API, auth, tenant resolution, RLS transactions, pricing engine, quote
state machine, webhooks and the River job queue. It is the only component that connects to
Postgres or object storage (ADR-001, ADR-003).

## What belongs here

- `cmd/server`: wiring only (config, dependency construction, server start).
- `internal/{tenantdb,pricing,quotes,inbox,platform}`: all logic. `pricing` is pure, no I/O.
- `db/migrations`: SQL migrations, owned by the `qp_migrator` role.

## What must never go here

- LLM or agent code, and Python.
- String-formatted tenant ids, or tenant SQL outside `InTenantTx`.
- Floats near money.
- Tenant content in River job args (IDs only).
- Connecting as a superuser or any BYPASSRLS role.
- `internal/` importing `cmd/`.

## Commands

Run from `apps/core-api`:

- `make test`: unit tests (`go test -short ./...`; real-Postgres tests skip).
- `make integration-test`: `go run ./cmd/migrate` then the full suite via `POSTGRES_DSN`.
- `make generate`: `sqlc generate` (regenerates `internal/inbox/inboxdb`).
- `make lint`: `gofmt -l` + `go vet` (+ `golangci-lint` when installed).

The repo root `make test` runs all three apps.

## Database

- Two DSNs: `POSTGRES_DSN` (role `qp_app`) at runtime; `MIGRATOR_DSN` (role `qp_migrator`) for migrations only.
- Every function created in schema `app` that `qp_app` calls (including helpers used in RLS policies or called through sqlc) needs an explicit `GRANT EXECUTE ... TO qp_app` in the same migration.
- River migrations must run with Schema `"river"`.
