# Plan: implement the v1.3 data model migration

Spec: `docs/specs/2026-11-10-data-model-v1.3.md` (authoritative — read it first).
Branch: `feat/core-data-model-v13` off `develop`. One migration file, one
concern: the domain schema.

## Task 1 — migration `0002_domain_schema_v13.sql`

File: `apps/core-api/db/migrations/0002_domain_schema_v13.sql`, goose
`-- +goose Up` / `-- +goose Down`, `-- +goose StatementBegin/End` around
functions and triggers.

Up, in this order:

1. `CREATE TYPE` the 13 enums in `app` (spec §6).
2. Camada A: `organizations`, `users`, `organization_memberships`; then
   `REVOKE ALL ON <each> FROM qp_app`.
3. Camada C: `connectors`, `public_snapshots`, `healing_sessions`
   (`failing_payload_hash` + `failing_payload_storage_key`, not the TEXT
   payload); then `REVOKE INSERT, UPDATE, DELETE ON <each> FROM qp_app`
   (SELECT stays via migrator default privileges).
4. Camada B tables in FK order: `pricing_rules`, `private_tariff_batches`,
   `private_tariffs`, `rfq_requests`, `rfq_attachments`, `carrier_offers`,
   `org_quote_counters`, `quotes`, `quote_line_items`, `tenant_audit_logs`,
   `organization_connector_preferences`, `suppression_list`.
   Each gets: `organization_id uuid NOT NULL REFERENCES organizations(id)`,
   the composite index starting `organization_id`, `ENABLE` + `FORCE` RLS,
   and `tenant_isolation_policy` with the NULLIF expression (copy the exact
   pattern from `0001_tenant_tables.sql`).
   Extra revokes: `REVOKE UPDATE, DELETE ON private_tariffs, tenant_audit_logs
   FROM qp_app`.
5. `DROP TABLE app.private_tariff_embeddings` (folded into
   `private_tariffs.embedding`).
6. Functions and triggers:
   - `app.resolve_inbound_org(text)`, `app.list_memberships_for_email(text)`,
     `app.get_current_organization()` — all `SECURITY DEFINER`, `SET
     search_path = app`, then `GRANT EXECUTE ... TO qp_app`.
   - `app.platform_insert_public_snapshot`, `platform_update_connector_status`,
     `platform_insert_healing_session` — same pattern, EXECUTE → `qp_admin`
     (the platform path's credential; `qp_app` keeps tenant-facing functions
     only).
   - `block_snapshot_mutation()` + `block_approved_quote_mutation()` trigger
     functions and their `BEFORE UPDATE OR DELETE` triggers.
7. `COMMENT ON COLUMN quotes.final_price_brl` recording the round-per-line
   rule.

Down: drop triggers/functions/types/tables in reverse; recreate
`private_tariff_embeddings` as `0001` had it (with its policy).

## Task 2 — update the moved isolation test

`internal/tenantdb/spike_test.go`: the identical-vectors test used
`private_tariff_embeddings`. Point it at `private_tariffs` (insert needs a
`private_tariff_batches` row first per FK; then two orgs, identical
`embedding`, no-WHERE similarity query → zero rows from B).

## Task 3 — schema lint test

New `internal/tenantdb/schema_lint_test.go` (integration, `RequirePostgres`,
migrator DSN for catalog reads is fine):

- every `app` table with an `organization_id` column has
  `relrowsecurity AND relforcerowsecurity` and a policy named
  `tenant_isolation_policy`. Sole allowlist entry:
  `organization_memberships` (Layer A, comment 9). `org_quote_counters`
  is Camada B — it must have RLS like the rest.
- `has_table_privilege('qp_app', 'app.public_snapshots', 'UPDATE')` etc. are
  false for Camada C writes; same for `private_tariffs` and
  `tenant_audit_logs` UPDATE/DELETE.
- `organizations`, `users`, `organization_memberships` deny `qp_app`
  SELECT.
- immutability smoke checks: `UPDATE`/`DELETE` on `public_snapshots` as
  `qp_migrator` raises (trigger, not just grants).

## Task 4 — verification

Run with `POSTGRES_DSN`/`MIGRATOR_DSN` exported (qp_app/qp_migrator against
the docker DB):

```
go test -count=1 -race ./...
make db-check && make db-check-negative   # role-model assertions still pass
go vet ./... && gofmt -l .
```

Also run `goose -dir db/migrations postgres "$MIGRATOR_DSN" down-to 1 && up`
once locally to prove Down works.

## Do not touch

- `apps/ai-engine/**`, `apps/web/**`, `docs/adr/0006*`, RFC files, Resend/
  Cloudflare config — Marcelo and Amanda's areas.
- `sqlc.yaml`, `db/queries/**`, `db/sqlc_schemas.sql`, generated `inboxdb` —
  no new queries in this PR; catalog changes come with repository PRs.
- `AGENTS.md`, `CLAUDE.md`, CI workflows.
- `inbound_messages` and its webhook code — kept as the receipt table.
