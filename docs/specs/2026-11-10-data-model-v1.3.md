# Tech spec: v1.3 data model — `app` schema for `core-api`

Date: 2026-11-10 · Status: Proposed (pending Pedro's review) · Author: orchestrator session
Inputs: product spec §5.1–5.4 (v1.3), the resolved sections-4/5 comments
(`docs/notion-comments-sections-4-5.md` items 4–17), RFC-001 §9 data-model
deltas, RFC-002 step 3 (tariff versioning), ADR-003, ADR-005, AGENTS.md rules 3–4, 9.

## 1. Purpose and scope

Land the real multi-tenant domain schema on top of the Mission 3 spike
foundation, as a single additive goose migration plus enforcement tests. This
spec resolves every open schema question from the comments file and the two
RFCs; it does not implement repositories, the pricing engine, or pipeline
stages — those come after Pedro approves this model.

In scope: Camada A identity tables + platform functions, Camada B tenant
tables + RLS, Camada C global tables + platform write path, enum types,
immutability/append-only enforcement, the schema-lint test (comment 17a).

Out of scope: repositories/sqlc queries for the new tables (next PRs),
the pricing engine (RFC-001), pipeline stages (ADR-004), auth flows beyond
the two platform functions needed now (ADR-005 is still Proposed).

## 2. Layer model

| Layer | Tables | Tenant RLS? | `qp_app` access |
|---|---|---|---|
| A — Tenant Registry | `organizations`, `users`, `organization_memberships` | **No** — queried before tenant context exists (comment 9) | REVOKE ALL; access only via `SECURITY DEFINER` functions |
| B — Tenant data | everything with `organization_id` | **Yes** — ENABLE + FORCE + `tenant_isolation_policy` | SELECT/INSERT/UPDATE/DELETE minus per-table revokes |
| C — Platform-global | `connectors`, `public_snapshots`, `healing_sessions` | No (no `organization_id`) | SELECT only; writes via `SECURITY DEFINER` functions (comment 15) |

Layer A functions are owned by `qp_migrator` (the table owner, not superuser,
no BYPASSRLS). `qp_app` gets `EXECUTE` only on the functions it needs — the
init script already revokes default `EXECUTE` on `app` functions, so each
migration grants explicitly (ADR-005 platform path, comment 6).

## 3. Camada A — identity and routing

### `organizations`

| column | type | notes |
|---|---|---|
| `id` | `uuid` PK | `gen_random_uuid()` |
| `name` | `text` NOT NULL | |
| `slug` | `text` UNIQUE NOT NULL | used in `<slug>@in.quotepilot.app` |
| `cnpj` | `varchar(14)` UNIQUE NULL | optional at onboarding |
| `inbound_email_alias` | `text` UNIQUE NOT NULL | Resend Inbound address |
| `outbound_from_name` | `text` NOT NULL | |
| `outbound_reply_to` | `text` NOT NULL | |
| `branding_json` | `jsonb` NOT NULL | colors, logo, legal footer |
| `created_at` | `timestamptz` NOT NULL DEFAULT now() | |

### `users`

`id` uuid PK · `email` text UNIQUE NOT NULL · `full_name` text ·
`password_hash_or_magic_sub` text · `is_platform_admin` boolean NOT NULL
DEFAULT false · `created_at` timestamptz NOT NULL DEFAULT now().

### `organization_memberships`

`id` uuid PK · `organization_id` uuid FK→organizations · `user_id` uuid
FK→users · `role` `app.membership_role` NOT NULL ·
`UNIQUE (organization_id, user_id)` · `created_at`.

### Platform functions (SECURITY DEFINER, EXECUTE → `qp_app`)

- `app.resolve_inbound_org(recipient text) returns uuid` — resolve the
  Resend `to` address to an organization via `inbound_email_alias`, then
  `<slug>@in.%` fallback. Runs before any tenant context exists (comment 4).
- `app.list_memberships_for_email(email text) returns table(organization_id uuid, role app.membership_role)` —
  post-login org picker, without exposing the full registry.
- `app.get_current_organization() returns organizations` — tenant-scoped
  accessor: returns the row for
  `NULLIF(current_setting('app.current_organization_id', true),'')::uuid`
  and NULL/no row otherwise, so workers can read branding inside
  `InTenantTx` without breaking the function-gated rule.

Layer A grants: `REVOKE ALL ON organizations, users, organization_memberships
FROM qp_app` after creation (the migrator default privileges would otherwise
grant DML).

## 4. Camada B — tenant tables (RLS enforced)

Every table: `organization_id uuid NOT NULL REFERENCES organizations(id)`,
a composite index starting with `organization_id`, `ENABLE` + `FORCE` RLS,
and the shared policy:

```sql
CREATE POLICY tenant_isolation_policy ON <table>
  USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
  WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
```

### `pricing_rules`

Spec §5.2.3 plus: `min_markup_usd numeric(14,2)` (FCL per-container floor,
RFC-001 §9). Precedence is **route > modal > ALL**; uniqueness per comment 10:

```sql
UNIQUE NULLS NOT DISTINCT (organization_id, modal, origin_locode, dest_locode)
```

so one rule per (org, modal, route-or-global). `modal` uses
`app.pricing_modal` ('FCL','LCL','AIR','ALL').

### `private_tariff_batches`

Spec §5.2.4 plus `status app.batch_status NOT NULL DEFAULT 'PENDING_REVIEW'`
('PENDING_REVIEW','ACTIVE') and `activated_at timestamptz` — written once at
activation (RFC-002 step 3).

### `private_tariffs`

Spec §5.2.4 with comment 11 applied:

- add `charge_category app.charge_category NOT NULL` and `charge_code text NOT NULL`
- `embedding vector(1024)` + `embedding_model text` (replaces the spike's
  `private_tariff_embeddings`, which this migration drops)
- `is_removed boolean NOT NULL DEFAULT false` — tombstone versioning; the
  natural key is `(carrier_or_agent, modal, origin_locode, dest_locode,
  equipment_or_unit, charge_category, charge_code)` per RFC-002 step 3
- `CHECK (valid_from <= valid_until)`
- **append-only**: `REVOKE UPDATE, DELETE FROM qp_app`; corrections land as
  new rows in a new batch, never as edits (comment 11, rule 9)
- **no vector index in the MVP** — after the org+modal filter the row count
  is small; exact scan gives perfect recall (comment 11)
- indexes: the spec §5.4 route index `(organization_id, modal,
  origin_locode, dest_locode, valid_until)` and a natural-key index
  `(organization_id, batch_id)` for selection audits

### `rfq_requests`

Spec §5.2.5 plus comment 12: `kind app.rfq_kind NOT NULL DEFAULT 'RFQ'`
('RFQ','TARIFF_UPDATE','OTHER'), status enum gains `'IGNORED'`, and
`rfc_message_id text` (RFC 5322 Message-ID, for threading) separate from
`resend_message_id` (idempotency, `UNIQUE (organization_id,
resend_message_id)`). `raw_html_body` stays a column; rendering it is an
app-layer sanitization concern, noted for the frontend.

### `rfq_attachments`

Spec §5.2.5 unchanged. `storage_key` under
`tenants/{organization_id}/rfqs/{rfq_id}/...`.

### `carrier_offers` (new, v1.3)

Pinned Hapag offer for an RFQ — the ai-engine connector's validated output,
persisted before pricing so a quote is reproducible (rule 9, plan doc):

`id` uuid PK · `organization_id` · `rfq_id` uuid FK→rfq_requests ·
`carrier` text NOT NULL · `offer_reference` text · `offer_payload_json`
jsonb NOT NULL (validated offer: legs, equipment prices, validity) ·
`fetched_at` timestamptz NOT NULL · `expires_at` timestamptz.
Index `(organization_id, rfq_id)`.

### `org_quote_counters`

Per-company sequential `quote_number` without `MAX()+1` (comment 13):
`organization_id` uuid PK FK→organizations · `next_seq` integer NOT NULL
DEFAULT 1. Allocation happens inside `InTenantTx` via
`INSERT ... ON CONFLICT (organization_id) DO UPDATE SET next_seq =
org_quote_counters.next_seq + 1 RETURNING next_seq` — the row lock
serializes allocation per tenant.

### `quotes`

Spec §5.2.6 plus:

- `status app.quote_status NOT NULL DEFAULT 'DRAFT'`
  ('DRAFT','SUPERSEDED','APPROVED_SENT') — required for the immutability
  rule (comment 13)
- `priced_at timestamptz` — the pricing instant; tariff selection compares
  `activated_at <= priced_at` (RFC-002 step 3)
- `inputs_hash text` — hash of the exact pricing inputs for replay/audit
  (RFC-001 §9)
- `carrier_offer_id uuid FK→carrier_offers NULL` — the pinned offer the
  price was computed from (v1.3)
- `UNIQUE (organization_id, quote_number)` and
  `UNIQUE (organization_id, rfq_id, version)`
- **immutability**: trigger `block_approved_quote_mutation` raises on
  UPDATE/DELETE when `OLD.status = 'APPROVED_SENT'`; corrections are new
  versions (rule 9)
- schema comment on `final_price_brl`: rounding rule — sell lines are
  rounded per line and the price is the sum of rounded lines (RFC-001 §8)

### `quote_line_items`

Spec §5.2.6 plus `carrier_offer_id uuid FK→carrier_offers NULL` (line-level
pin alongside `private_tariff_id` and `public_snapshot_id`) and
`flags jsonb NOT NULL DEFAULT '[]'` (engine annotations such as
`MIN_MARKUP_APPLIED`, RFC-001 §9). `category` adds `'CARRIER_OFFER'` to
`source_type` so Hapag-offer lines are distinguishable from private tariffs.
Test invariant: `sum(sell_amount_brl) = quotes.final_price_brl` per quote.

### `tenant_audit_logs`

Spec §5.2.7. `REVOKE UPDATE, DELETE FROM qp_app` makes "immutable" real
(comment 14). FK keeps `ON DELETE CASCADE` — decision: deleting a company
deletes its audit trail; company deletion is a deliberate admin act and the
alternative (dangling audits) is worse.

### `organization_connector_preferences`

Moved to Camada B (it has `organization_id`, comment 15): `id` uuid PK ·
`organization_id` · `connector_id` uuid FK→connectors · `enabled` boolean
NOT NULL DEFAULT true · `settings_json` jsonb NOT NULL DEFAULT '{}' ·
`UNIQUE (organization_id, connector_id)` · tenant RLS like every Camada B
table.

### `suppression_list` (RFC-002 §9)

`id` uuid PK · `organization_id` · `email` text NOT NULL · `reason` text ·
`created_at` · `UNIQUE (organization_id, email)`. Tenant-scoped default;
whether Resend-level global suppression is also needed is an open item for
Amanda (RFC-002).

### `inbound_messages` (kept from the spike)

Unchanged — it is the webhook receipt table: the Svix-verified idempotent
insert lands here, then the fetch job materializes `rfq_requests`
(comment 4 flow). `resend_message_id` uniqueness in both places is
intentional defense in depth.

### dropped: `private_tariff_embeddings`

Folded into `private_tariffs.embedding`/`embedding_model` (comment 11). The
Down migration recreates it for reversibility.

## 5. Camada C — platform-global tables

`qp_app` gets `SELECT` only (`REVOKE INSERT, UPDATE, DELETE`). Writes go
through `SECURITY DEFINER` functions owned by `qp_migrator` (comment 15).

- `connectors` — spec §5.3 (`status app.connector_status`,
  `confidence_score numeric(5,4)`, `last_scraped_at`)
- `public_snapshots` — spec §5.3 plus `block_snapshot_mutation` trigger
  raising on UPDATE/DELETE for every role; snapshots are immutable
  (comment 15)
- `healing_sessions` — spec §5.3 replacing `failing_snapshot_payload` with
  `failing_payload_hash text` + `failing_payload_storage_key text`
  (comment 15 — big payloads live in storage, not Postgres)

Platform write functions (EXECUTE → `qp_app`): `platform_insert_public_snapshot`,
`platform_update_connector_status`, `platform_insert_healing_session`.
Workers running as `qp_app` can therefore publish snapshots without ever
holding table-level write grants.

## 6. Enum types (`CREATE TYPE` in `app`)

`membership_role` ('OWNER','ADMIN','PRICING_ANALYST') · `freight_modal`
('FCL','LCL','AIR') · `pricing_modal` ('FCL','LCL','AIR','ALL') ·
`tariff_source_type` ('CSV_UPLOAD','RESEND_INBOUND') · `batch_status`
('PENDING_REVIEW','ACTIVE') · `rfq_kind` ('RFQ','TARIFF_UPDATE','OTHER') ·
`rfq_status` ('RECEIVED','PARSING','PRICING','NEEDS_REVIEW','READY',
'APPROVED_SENT','FAILED','IGNORED') · `quote_status`
('DRAFT','SUPERSEDED','APPROVED_SENT') · `quote_line_category`
('INTL_FREIGHT','ORIGIN_CHARGES','TERMINAL_STORAGE_THC','CUSTOMS_DISPATCH',
'SURCHARGE') · `line_source_type` ('PRIVATE_TARIFF','FIRECRAWL_PUBLIC',
'ORG_FIXED_RULE','MANUAL_OVERRIDE','CARRIER_OFFER') · `charge_category`
('OCEAN_FREIGHT','AIR_FREIGHT','SURCHARGE','ORIGIN_CHARGE',
'DESTINATION_CHARGE','TERMINAL','CUSTOMS','OTHER') · `connector_status`
('HEALTHY','DEGRADED','HEALING') · `healing_status`
('OPEN','PR_READY','MERGED','FAILED').

`charge_code` stays free `text` (carrier-specific codes like `BAF`, `THC`
arrive from tariffs we don't control; `charge_category` is our coarse
grouping).

## 7. Enforcement and tests

1. **Schema lint test** (comment 17a): a Go integration test fails CI when
   any `app` table has an `organization_id` column but lacks ENABLE+FORCE
   RLS or a tenant policy — with an explicit allowlist for
   `organization_memberships` (platform-path table, comment 9). Also asserts
   Camada C tables grant only SELECT to `qp_app`, and
   `private_tariffs`/`tenant_audit_logs` lack UPDATE/DELETE for `qp_app`.
2. **Isolation suite** keeps running; the identical-vectors test moves from
   the dropped `private_tariff_embeddings` to `private_tariffs.embedding`.
3. Quote immutability trigger and append-only revokes each get a test
   (UPDATE as `qp_app` must fail).
4. Existing spike tests unchanged except where the dropped table forces it.

## 8. Decisions register

| # | Comment/RFC item | Resolution |
|---|---|---|
| 4 | webhook pre-tenant resolution | `resolve_inbound_org` SECURITY DEFINER + `in.` subdomain; receipt → fetch job → `rfq_requests` |
| 6 | platform path | Layer A revoked from `qp_app`, two lookup functions now, rest with ADR-005 |
| 9 | Layer A not under RLS | implemented; function-gated access + `get_current_organization` |
| 10 | pricing_rules NULLs | `UNIQUE NULLS NOT DISTINCT`; precedence route > modal > ALL |
| 11 | private_tariffs | +`charge_category`/`charge_code`, append-only revoke, `CHECK` dates, `VECTOR(1024)`+`embedding_model`, no ANN index, `is_removed` tombstones |
| 12 | rfq_requests | +`kind`, +`IGNORED`, +`rfc_message_id`; HTML sanitization is app-layer |
| 13 | quotes | counter table + row lock, immutability trigger, `UNIQUE(org,rfq,version)`, sum-of-lines invariant test |
| 14 | audit immutability | `REVOKE UPDATE,DELETE`; CASCADE kept (documented) |
| 15 | Camada C | SELECT-only + platform functions; snapshot immutability trigger; payload → hash+storage key; connector prefs → Camada B |
| 16 | NULLIF policy | already in force (Mission 3) |
| 17a | schema lint | test in §7.1 |
| RFC-001 §9 | v1.3 deltas | `min_markup_usd`, `inputs_hash`, line `flags`, `carrier_offer_id` pins, `priced_at` |
| RFC-002 §3 | versioning | batch `status`/`activated_at`, row `is_removed`, natural-key index, quote `priced_at`, replay via recorded `private_tariff_id` |

## 9. Open items (recorded, not blocking)

- ADR-005 remains Proposed: only the two lookup functions are committed
  now; auth/session functions arrive with the auth mission.
- `suppression_list` semantics vs Resend account-level suppression — Amanda
  (RFC-002).
- Per-tenant send caps (RFC-002 §9) — app config, not this schema.
- Export-direction rules and `INCOMPLETE` offer semantics — RFC-001 open
  questions; the schema carries the columns either way.
- ANN index on `private_tariffs.embedding` — deliberately deferred (comment
  11); revisit with real volumes.
- `inbound_messages` vs `rfq_requests` merge — revisit once the real
  pipeline lands; keeping both is correct for the comment-4 flow.
