-- v1.3 domain schema (docs/specs/2026-11-10-data-model-v1.3.md).
-- Layer A: identity and routing, no tenant RLS, function-gated (comment 9).
-- Layer B: tenant tables, ENABLE + FORCE RLS + tenant_isolation_policy.
-- Layer C: platform-global tables, SELECT-only for qp_app, writes through
-- SECURITY DEFINER functions granted to qp_admin only (comment 15).
--
-- Applied by qp_migrator (object owner). qp_app receives SELECT/INSERT/UPDATE/
-- DELETE on new tables via ALTER DEFAULT PRIVILEGES in the init script, so the
-- restrictive cases of the spec are explicit REVOKEs below. Functions get no
-- EXECUTE by default (init script revokes PUBLIC EXECUTE); each function below
-- grants EXECUTE explicitly — tenant-facing functions to qp_app, the
-- platform_* write/teardown path to qp_admin only (ADR-003 section 1,
-- ADR-005 section 2, spec section 5).
--
-- The tenant policy copies the 0001 pattern (AGENTS.md rule 4):
--   organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid
-- With no tenant context the expression is NULL, so an unscoped connection
-- yields zero rows instead of raising on ''::uuid. The v1.3 canonical policy
-- name is tenant_isolation_policy; inbound_messages is renamed to match so the
-- schema lint sees one convention.

-- +goose Up

-- =========================================================================
-- 1. Enum types (spec section 6), all in schema app.
-- =========================================================================
-- +goose StatementBegin
CREATE TYPE app.membership_role AS ENUM ('OWNER', 'ADMIN', 'PRICING_ANALYST');
CREATE TYPE app.freight_modal AS ENUM ('FCL', 'LCL', 'AIR');
CREATE TYPE app.pricing_modal AS ENUM ('FCL', 'LCL', 'AIR', 'ALL');
CREATE TYPE app.tariff_source_type AS ENUM ('CSV_UPLOAD', 'RESEND_INBOUND');
CREATE TYPE app.batch_status AS ENUM ('PENDING_REVIEW', 'ACTIVE');
CREATE TYPE app.rfq_kind AS ENUM ('RFQ', 'TARIFF_UPDATE', 'OTHER');
CREATE TYPE app.rfq_status AS ENUM ('RECEIVED', 'PARSING', 'PRICING', 'NEEDS_REVIEW', 'READY', 'APPROVED_SENT', 'FAILED', 'IGNORED');
CREATE TYPE app.quote_status AS ENUM ('DRAFT', 'SUPERSEDED', 'APPROVED_SENT');
CREATE TYPE app.quote_line_category AS ENUM ('INTL_FREIGHT', 'ORIGIN_CHARGES', 'TERMINAL_STORAGE_THC', 'CUSTOMS_DISPATCH', 'SURCHARGE');
CREATE TYPE app.line_source_type AS ENUM ('PRIVATE_TARIFF', 'FIRECRAWL_PUBLIC', 'ORG_FIXED_RULE', 'MANUAL_OVERRIDE', 'CARRIER_OFFER');
CREATE TYPE app.charge_category AS ENUM ('OCEAN_FREIGHT', 'AIR_FREIGHT', 'SURCHARGE', 'ORIGIN_CHARGE', 'DESTINATION_CHARGE', 'TERMINAL', 'CUSTOMS', 'OTHER');
CREATE TYPE app.connector_status AS ENUM ('HEALTHY', 'DEGRADED', 'HEALING');
CREATE TYPE app.healing_status AS ENUM ('OPEN', 'PR_READY', 'MERGED', 'FAILED');
-- +goose StatementEnd

-- =========================================================================
-- 2. Camada A — identity and routing (spec section 3).
-- No RLS: they are read before a tenant context exists. REVOKE ALL undoes the
-- migrator default grant; qp_app reaches them only through the SECURITY
-- DEFINER functions at the bottom of this file.
-- =========================================================================
-- +goose StatementBegin
CREATE TABLE app.organizations (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name text NOT NULL,
    slug text NOT NULL UNIQUE,
    cnpj varchar(14) UNIQUE,
    inbound_email_alias text NOT NULL UNIQUE,
    outbound_from_name text NOT NULL,
    outbound_reply_to text NOT NULL,
    branding_json jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- Slug and alias are resolved case-insensitively (resolve_inbound_org
    -- lower()s the recipient), so a case-variant "duplicate" would be an
    -- ambiguity the UNIQUE cannot see. Only lowercase may be stored; the
    -- Down drops these with the table.
    CHECK (slug = lower(slug)),
    CHECK (inbound_email_alias = lower(inbound_email_alias))
);

CREATE TABLE app.users (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    email text NOT NULL UNIQUE,
    full_name text,
    password_hash_or_magic_sub text,
    is_platform_admin boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    -- list_memberships_for_email compares lower(email): an uppercase variant
    -- could shadow another account's lowercase address.
    CHECK (email = lower(email))
);

-- Carries organization_id but is Layer A: queried before tenant context exists
-- (comment 9). It is allowlisted in roles.sql [M], tenantdb.assertRLSForced and
-- the schema lint test instead of carrying a tenant policy.
CREATE TABLE app.organization_memberships (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES app.organizations (id) ON DELETE CASCADE,
    user_id uuid NOT NULL REFERENCES app.users (id),
    role app.membership_role NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, user_id)
);

REVOKE ALL ON app.organizations, app.users, app.organization_memberships FROM qp_app;
-- +goose StatementEnd

-- =========================================================================
-- 3. Camada C — platform-global tables (spec section 5).
-- No organization_id and no RLS; qp_app keeps SELECT from the default grant
-- and loses INSERT/UPDATE/DELETE. Writes go through the platform functions.
-- =========================================================================
-- +goose StatementBegin
CREATE TABLE app.connectors (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    slug text NOT NULL UNIQUE,
    display_name text NOT NULL,
    target_url text NOT NULL,
    status app.connector_status NOT NULL DEFAULT 'HEALTHY',
    confidence_score numeric(5, 4),
    last_scraped_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CHECK (confidence_score BETWEEN 0 AND 1)
);

-- Immutable: the block_snapshot_mutation trigger at the bottom raises on every
-- UPDATE/DELETE for every role (comment 15). Quotes pin rows by id (rule 9).
-- The raw fetched bytes live in storage under the platform prefix; the row
-- keeps the parsed rates, the SHA-256 of the source markdown and the check
-- results that produced S_conf (RFC-003 sections 3-4, v1.3 follow-ups:
-- parser_version, per-snapshot check results).
CREATE TABLE app.public_snapshots (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    connector_id uuid NOT NULL REFERENCES app.connectors (id),
    parser_version text NOT NULL,
    raw_markdown_hash text,
    extracted_rates_json jsonb NOT NULL,
    source_url text,
    storage_key text,
    confidence_score numeric(5, 4),
    check_results_json jsonb NOT NULL DEFAULT '[]'::jsonb,
    scraped_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (confidence_score BETWEEN 0 AND 1)
);

CREATE TABLE app.healing_sessions (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    connector_id uuid NOT NULL REFERENCES app.connectors (id),
    status app.healing_status NOT NULL DEFAULT 'OPEN',
    failing_snapshot_id uuid REFERENCES app.public_snapshots (id),
    -- Big payloads live in storage, not Postgres (comment 15).
    failing_payload_hash text,
    failing_payload_storage_key text,
    devin_session_id text,
    devin_session_url text,
    github_pr_url text,
    pytest_passed boolean,
    created_at timestamptz NOT NULL DEFAULT now(),
    resolved_at timestamptz
);

REVOKE INSERT, UPDATE, DELETE ON app.connectors, app.public_snapshots, app.healing_sessions FROM qp_app;
-- +goose StatementEnd

-- =========================================================================
-- 4. Camada B — tenant tables (spec section 4), in FK order.
-- Every table gets organization_id -> organizations, an org-first index,
-- ENABLE + FORCE RLS and tenant_isolation_policy.
-- =========================================================================

-- pricing_rules (spec 5.2.3 + comment 10, RFC-001 section 9):
-- route > modal > ALL precedence; NULL locodes mean the org-global rule, so a
-- plain UNIQUE could not deduplicate — NULLS NOT DISTINCT treats them as equal.
-- route_target_margin applies to route-scoped rules, default_target_margin to
-- modal/ALL-scoped rules; margins are applied on freight cost (RFC-001).
-- +goose StatementBegin
CREATE TABLE app.pricing_rules (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES app.organizations (id) ON DELETE CASCADE,
    modal app.pricing_modal NOT NULL,
    default_target_margin numeric(5, 4) NOT NULL,
    -- Extra FX safety buffer applied to carrier FX rates (RFC-001 section 7).
    fx_safety_spread numeric(5, 4) NOT NULL DEFAULT 0,
    -- Minimum confidence for express auto-approval.
    auto_approve_threshold numeric(5, 4) DEFAULT 1.0000,
    fixed_fees_json jsonb NOT NULL DEFAULT '[]'::jsonb,
    quote_validity_days integer NOT NULL DEFAULT 7,
    origin_locode varchar(10),
    dest_locode varchar(10),
    route_target_margin numeric(5, 4),
    -- FCL per-container floor, extra per RFC-001 section 9 (no spec rename).
    min_markup_usd numeric(14, 2),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE NULLS NOT DISTINCT (organization_id, modal, origin_locode, dest_locode),
    -- Ratios/confidences are bounded [0,1]; NULL passes (column is optional).
    CHECK (default_target_margin BETWEEN 0 AND 1),
    CHECK (route_target_margin BETWEEN 0 AND 1),
    CHECK (auto_approve_threshold BETWEEN 0 AND 1)
);

ALTER TABLE app.pricing_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.pricing_rules FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON app.pricing_rules
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- private_tariff_batches (spec 5.2.4 + RFC-002 step 3): a batch lands as
-- PENDING_REVIEW; activation writes status/activated_at once.
-- +goose StatementBegin
CREATE TABLE app.private_tariff_batches (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES app.organizations (id) ON DELETE CASCADE,
    source_type app.tariff_source_type NOT NULL,
    -- Upload filename or Resend message id, for audit.
    source_filename text,
    uploaded_by uuid REFERENCES app.users (id),
    status app.batch_status NOT NULL DEFAULT 'PENDING_REVIEW',
    activated_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX private_tariff_batches_org_status_idx ON app.private_tariff_batches (organization_id, status);

ALTER TABLE app.private_tariff_batches ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.private_tariff_batches FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON app.private_tariff_batches
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- private_tariffs (spec 5.2.4 + comment 11, ADR-006 section 3 contract):
-- append-only (UPDATE/DELETE revoked below); corrections are new rows in a new
-- batch, removed prices are is_removed tombstones under the same natural key.
-- No ANN index on embedding: after the org+modal filter an exact scan has
-- perfect recall at MVP volumes (comment 11).
-- +goose StatementBegin
CREATE TABLE app.private_tariffs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES app.organizations (id) ON DELETE CASCADE,
    batch_id uuid NOT NULL REFERENCES app.private_tariff_batches (id),
    carrier_or_agent text NOT NULL,
    modal app.freight_modal NOT NULL,
    origin_locode varchar(10) NOT NULL,
    dest_locode varchar(10) NOT NULL,
    equipment_or_unit varchar(20) NOT NULL,
    charge_category app.charge_category NOT NULL,
    -- Free text on purpose: carrier codes (BAF, THC) are not ours (spec section 6).
    charge_code text NOT NULL,
    currency varchar(3) NOT NULL,
    net_amount numeric(14, 4) NOT NULL,
    min_amount numeric(14, 4),
    free_time_days integer,
    transit_time_days integer,
    valid_from date NOT NULL,
    valid_until date NOT NULL,
    -- Canonical text for semantic route/service search; required (spec 5.2.4).
    search_description text NOT NULL,
    -- pgvector lives in schema extensions (ADR-003 section 4), qualified as in
    -- 0001. v1.3 spec fixes VECTOR(1024) + embedding_model (comment 11); the
    -- product spec's 1536 predates the 1024-dim embedding decision.
    embedding extensions.vector(1024),
    embedding_model text,
    is_removed boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (valid_from <= valid_until),
    CHECK (embedding IS NULL OR embedding_model IS NOT NULL)
);

-- Route lookup for tariff selection (spec 5.4).
CREATE INDEX private_tariffs_route_idx ON app.private_tariffs
    (organization_id, modal, origin_locode, dest_locode, valid_until);
-- Batch lookup for selection audits: which rows came in with a given batch
-- (RFC-002 step 3); the natural key itself lives in the application layer.
CREATE INDEX private_tariffs_org_batch_idx ON app.private_tariffs (organization_id, batch_id);

ALTER TABLE app.private_tariffs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.private_tariffs FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON app.private_tariffs
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- rfq_requests (spec 5.2.5 + comment 12, RFC-002): kind splits RFQ / tariff
-- update / noise; rfc_message_id threads, resend_message_id is the idempotency
-- key; raw_html_body is sanitized at render time (app-layer concern).
-- +goose StatementBegin
CREATE TABLE app.rfq_requests (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES app.organizations (id) ON DELETE CASCADE,
    kind app.rfq_kind NOT NULL DEFAULT 'RFQ',
    status app.rfq_status NOT NULL DEFAULT 'RECEIVED',
    resend_message_id text NOT NULL,
    -- RFC 5322 Message-ID, kept separately from the Resend id (comment 12);
    -- the full In-Reply-To/References thread context sits in thread_headers_json.
    rfc_message_id text,
    thread_headers_json jsonb,
    customer_email text NOT NULL,
    customer_name text,
    subject text NOT NULL DEFAULT '',
    received_at timestamptz NOT NULL DEFAULT now(),
    raw_text_body text,
    raw_html_body text,
    -- Stage 3 checkpoint: the validated ExtractionResult (ADR-004).
    extracted_payload_json jsonb,
    -- Minimum calibrated auditor confidence across required fields (RFC-004 section 5).
    extraction_confidence numeric(5, 4),
    failure_reason text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, resend_message_id),
    CHECK (extraction_confidence BETWEEN 0 AND 1)
);

CREATE INDEX rfq_requests_org_status_idx ON app.rfq_requests (organization_id, status);

ALTER TABLE app.rfq_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.rfq_requests FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON app.rfq_requests
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- rfq_attachments (spec 5.2.5): bytes live in storage under
-- tenants/{organization_id}/rfqs/{rfq_id}/ (RFC-002 stage 1).
-- +goose StatementBegin
CREATE TABLE app.rfq_attachments (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES app.organizations (id) ON DELETE CASCADE,
    -- Attachment rows are meaningless without their RFQ; cascade lets
    -- platform_delete_organization reach the whole subtree (spec section 4).
    rfq_id uuid NOT NULL REFERENCES app.rfq_requests (id) ON DELETE CASCADE,
    storage_key text NOT NULL,
    filename text NOT NULL,
    mime_type text,
    -- Markdown text parsed off the attachment for the RFQ parser (spec 5.2.5).
    parsed_markdown text,
    size_bytes bigint,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX rfq_attachments_org_rfq_idx ON app.rfq_attachments (organization_id, rfq_id);

ALTER TABLE app.rfq_attachments ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.rfq_attachments FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON app.rfq_attachments
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- carrier_offers (v1.3): the validated Hapag offer pinned before pricing so a
-- quote is reproducible (rule 9); validated by core, written under tenant RLS.
-- +goose StatementBegin
CREATE TABLE app.carrier_offers (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES app.organizations (id) ON DELETE CASCADE,
    rfq_id uuid NOT NULL REFERENCES app.rfq_requests (id) ON DELETE CASCADE,
    carrier text NOT NULL,
    offer_reference text,
    offer_payload_json jsonb NOT NULL,
    fetched_at timestamptz NOT NULL,
    expires_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX carrier_offers_org_rfq_idx ON app.carrier_offers (organization_id, rfq_id);

ALTER TABLE app.carrier_offers ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.carrier_offers FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON app.carrier_offers
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- org_quote_counters (comment 13): per-company sequence feeding quote_number.
-- Allocation happens inside InTenantTx via
-- INSERT ... ON CONFLICT (organization_id) DO UPDATE SET next_seq =
-- org_quote_counters.next_seq + 1 RETURNING next_seq — the row lock
-- serializes allocation per tenant; the app then formats the TEXT
-- quote_number ('{slug}-{year}-{seq:04d}', e.g. COMEXA-2026-0042). RLS
-- applies like any Camada B table.
-- +goose StatementBegin
CREATE TABLE app.org_quote_counters (
    organization_id uuid PRIMARY KEY REFERENCES app.organizations (id) ON DELETE CASCADE,
    next_seq integer NOT NULL DEFAULT 1
);

ALTER TABLE app.org_quote_counters ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.org_quote_counters FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON app.org_quote_counters
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- quotes (spec 5.2.6 + comment 13, RFC-001 section 9, RFC-002 step 3):
-- reproducible — pinned carrier offer, FX rates and inputs hash; recalculation
-- creates a new version, APPROVED_SENT is immutable via trigger (rule 9).
-- +goose StatementBegin
CREATE TABLE app.quotes (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES app.organizations (id) ON DELETE CASCADE,
    rfq_id uuid NOT NULL REFERENCES app.rfq_requests (id) ON DELETE CASCADE,
    -- Formatted '{slug}-{year}-{seq:04d}' (e.g. COMEXA-2026-0042), sequential
    -- per company off org_quote_counters.next_seq (spec 5.2.6, comment 13).
    quote_number text NOT NULL,
    version integer NOT NULL DEFAULT 1,
    status app.quote_status NOT NULL DEFAULT 'DRAFT',
    quote_date date,
    -- The pricing instant; tariff selection compares activated_at <= priced_at.
    priced_at timestamptz,
    -- Hash of the exact pricing inputs, for replay/audit (RFC-001 section 9).
    inputs_hash text,
    carrier_offer_id uuid REFERENCES app.carrier_offers (id),
    cargo_cif_brl numeric(14, 2),
    total_cost_brl numeric(14, 2) NOT NULL,
    -- Margin applied by the engine for this quote (spec 5.2.6).
    target_margin numeric(5, 4) NOT NULL,
    final_price_brl numeric(14, 2) NOT NULL,
    -- Every FX rate applied, keyed by (source, currency, date) (RFC-001 section 7).
    fx_rates_applied_json jsonb NOT NULL DEFAULT '[]'::jsonb,
    -- Minimum calibrated auditor confidence carried into approval (spec 5.2.6).
    overall_confidence numeric(5, 4) NOT NULL,
    -- Engine output envelope: warnings, assumptions, COMPLETE/INCOMPLETE (RFC-001 section 3).
    pricing_result_json jsonb,
    pdf_storage_key text,
    approved_at timestamptz,
    approved_by_user_id uuid REFERENCES app.users (id),
    approved_via text,
    sent_outbound_message_id text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, quote_number),
    UNIQUE (organization_id, rfq_id, version),
    CHECK (target_margin BETWEEN 0 AND 1),
    CHECK (overall_confidence BETWEEN 0 AND 1)
);

ALTER TABLE app.quotes ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.quotes FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON app.quotes
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- quote_line_items (spec 5.2.6 + RFC-001 section 3 line shape):
-- line-level pins to the private tariff, public snapshot or carrier offer the
-- amount came from; flags carry engine annotations like MIN_MARKUP_APPLIED.
-- +goose StatementBegin
CREATE TABLE app.quote_line_items (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES app.organizations (id) ON DELETE CASCADE,
    quote_id uuid NOT NULL REFERENCES app.quotes (id) ON DELETE CASCADE,
    sort_order integer NOT NULL DEFAULT 0,
    category app.quote_line_category NOT NULL,
    description text NOT NULL DEFAULT '',
    source_type app.line_source_type NOT NULL,
    private_tariff_id uuid REFERENCES app.private_tariffs (id),
    public_snapshot_id uuid REFERENCES app.public_snapshots (id),
    carrier_offer_id uuid REFERENCES app.carrier_offers (id),
    original_currency varchar(3) NOT NULL,
    original_amount numeric(14, 4) NOT NULL,
    fx_rate_used numeric(10, 6) NOT NULL,
    cost_amount_brl numeric(14, 2) NOT NULL,
    sell_amount_brl numeric(14, 2) NOT NULL,
    -- Per-line calibrated confidence feeding quotes.overall_confidence.
    line_confidence numeric(5, 4) NOT NULL,
    flags jsonb NOT NULL DEFAULT '[]'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (line_confidence BETWEEN 0 AND 1)
);

CREATE INDEX quote_line_items_org_quote_idx ON app.quote_line_items (organization_id, quote_id);

ALTER TABLE app.quote_line_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.quote_line_items FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON app.quote_line_items
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- tenant_audit_logs (spec 5.2.7 + comment 14): UPDATE/DELETE revoked below.
-- ON DELETE CASCADE kept on purpose: deleting a company is a deliberate admin
-- act and deletes its audit trail (spec section 4 decision).
-- +goose StatementBegin
CREATE TABLE app.tenant_audit_logs (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES app.organizations (id) ON DELETE CASCADE,
    actor_user_id uuid REFERENCES app.users (id),
    action text NOT NULL,
    entity_type text,
    entity_id uuid,
    -- Spec leaves metadata_json nullable; not every action carries context.
    metadata_json jsonb,
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX tenant_audit_logs_org_created_idx ON app.tenant_audit_logs (organization_id, created_at);

ALTER TABLE app.tenant_audit_logs ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.tenant_audit_logs FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON app.tenant_audit_logs
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- organization_connector_preferences (comment 15): tenant-scoped, so Camada B.
-- +goose StatementBegin
CREATE TABLE app.organization_connector_preferences (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES app.organizations (id) ON DELETE CASCADE,
    connector_id uuid NOT NULL REFERENCES app.connectors (id),
    enabled boolean NOT NULL DEFAULT true,
    settings_json jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, connector_id)
);

ALTER TABLE app.organization_connector_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.organization_connector_preferences FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON app.organization_connector_preferences
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- suppression_list (RFC-002 section 9): tenant-scoped complaint/bounce list.
-- +goose StatementBegin
CREATE TABLE app.suppression_list (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL REFERENCES app.organizations (id) ON DELETE CASCADE,
    email text NOT NULL,
    reason text,
    created_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, email)
);

ALTER TABLE app.suppression_list ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.suppression_list FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON app.suppression_list
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- =========================================================================
-- 5. Camada B finalization — append-only revokes, the bookkeeping revoke,
-- the canonical policy rename, and the folded embeddings table.
-- =========================================================================

-- Append-only revokes (comments 11 and 14, RFC-002): SELECT and INSERT stay.
-- carrier_offers joins the set — a pinned offer is an audit record; a fresh
-- fetch is a new row, never an edit in place.
-- +goose StatementBegin
REVOKE UPDATE, DELETE ON app.private_tariffs, app.tenant_audit_logs, app.carrier_offers FROM qp_app;
-- +goose StatementEnd

-- Batches are audit records too: PENDING_REVIEW -> ACTIVE goes through UPDATE
-- (enforce_batch_activation below), but a batch row is never deleted — a bad
-- batch is superseded by a new one, so DELETE is revoked outright.
-- +goose StatementBegin
REVOKE DELETE ON app.private_tariff_batches FROM qp_app;
-- +goose StatementEnd

-- inbound_messages predates Layer A (0001 could not reference a table that
-- did not exist yet). It is a tenant row like every other Camada B table, so
-- it gets the same org FK — and the cascade lets platform_delete_organization
-- reach it.
--
-- Upgrade guard (Devin Review): on a database that already ran 0001 with
-- data, ADD CONSTRAINT validates every existing receipt against the new
-- organizations registry, so orphans abort the upgrade — with a raw FK
-- error if we let it get that far. A v1 receipt's organization_id is
-- unverifiable against a registry that did not exist when it was written:
-- backfilling the org or quarantining the receipt is an explicit operator
-- decision, never the migration's. NOT VALID would leave orphans
-- permanently unenforced, and deleting the rows would silently drop
-- receipts.
--
-- The validation must see real rows: FORCE RLS applies to qp_migrator
-- too, and FK validation runs under the invoker's row security — with no
-- tenant context it sees zero rows and would stamp convalidated=true over
-- hidden orphans (verified live). So this DO block disables RLS
-- transaction-locally, adds the constraint under a real validation scan
-- (translating a failure into the deliberate error below), then restores
-- ENABLE + FORCE before the statement ends. Any raise rolls the whole
-- migration transaction back, taking the temporary disable with it.
-- +goose StatementBegin
DO $$
BEGIN
    EXECUTE 'ALTER TABLE app.inbound_messages DISABLE ROW LEVEL SECURITY';
    BEGIN
        EXECUTE 'ALTER TABLE app.inbound_messages
            ADD CONSTRAINT inbound_messages_organization_id_fkey
            FOREIGN KEY (organization_id) REFERENCES app.organizations (id) ON DELETE CASCADE';
    EXCEPTION WHEN foreign_key_violation THEN
        RAISE EXCEPTION 'inbound_messages holds receipts for organizations that do not exist; reconcile them (backfill organizations or quarantine the receipts) before upgrading to v1.3 — refusing to upgrade rather than silently drop receipts';
    END;
    EXECUTE 'ALTER TABLE app.inbound_messages ENABLE ROW LEVEL SECURITY';
    EXECUTE 'ALTER TABLE app.inbound_messages FORCE ROW LEVEL SECURITY';
END $$;
-- +goose StatementEnd

-- Pre-existing spike hole closed here: goose_db_version is created by goose
-- itself as qp_migrator, so the migrator default privileges granted qp_app
-- SELECT/INSERT/UPDATE/DELETE on the bookkeeping table. qp_app needs nothing
-- beyond reads of its own schema bookkeeping, so writes are revoked.
-- +goose StatementBegin
REVOKE INSERT, UPDATE, DELETE ON app.goose_db_version FROM qp_app;
-- +goose StatementEnd

-- One policy convention across all Camada B tables: the spike's
-- tenant_isolation becomes the v1.3 canonical tenant_isolation_policy, so the
-- schema lint can assert a single name (plan task 3).
-- +goose StatementBegin
ALTER POLICY tenant_isolation ON app.inbound_messages RENAME TO tenant_isolation_policy;
-- +goose StatementEnd

-- Folded into private_tariffs.embedding/embedding_model (comment 11). The Down
-- migration recreates it exactly as 0001 had it.
-- +goose StatementBegin
DROP TABLE app.private_tariff_embeddings;
-- +goose StatementEnd

-- =========================================================================
-- 6. Platform functions (spec sections 3 and 5, ADR-005 section 2).
-- All SECURITY DEFINER owned by qp_migrator with a pinned search_path and
-- fully qualified object names; qp_app holds no table privilege on Layer A/C
-- and reaches them only through these. EXECUTE is granted explicitly per
-- function because the init script revokes default EXECUTE on app functions.
-- The grant splits by caller: the three tenant-facing functions stay with
-- qp_app; the five platform_* functions (Layer C writes and teardown) go to
-- qp_admin, the platform path's role, so the tenant credential physically
-- cannot reach them (Devin Review — the teardown trust model).
-- =========================================================================

-- resolve_inbound_org: normalize the Resend `to` address (lower case, plus-tag
-- removed — ADR-005 section 7), match inbound_email_alias, then the
-- <slug>@in.quotepilot.app fallback. The exact-alias match stays unrestricted:
-- the alias IS the org's ownership declaration (it is set per org at
-- onboarding). The slug fallback is pinned to the canonical inbound domain so
-- a lookalike host (in.evil.test) can never resolve a tenant by slug.
-- +goose StatementBegin
CREATE FUNCTION app.resolve_inbound_org(recipient text) RETURNS uuid
    LANGUAGE plpgsql
    STABLE
    SECURITY DEFINER
    SET search_path = app
AS $fn$
DECLARE
    addr text;
    local_part text;
    domain_part text;
    org_id uuid;
BEGIN
    addr := lower(btrim(recipient));
    IF position('<' IN addr) > 0 THEN
        addr := lower(btrim(substring(addr FROM '<([^<>]*)>')));
    END IF;
    local_part := split_part(addr, '@', 1);
    domain_part := split_part(addr, '@', 2);
    IF local_part = '' OR domain_part = '' THEN
        RETURN NULL;
    END IF;
    -- A second '@' makes the recipient malformed; without this, split_part
    -- truncates silently and 'victim@in.quotepilot.app@evil.test' resolves as
    -- 'victim@in.quotepilot.app'. Refused before either lookup so the
    -- reconstructed local@domain can never alias- or slug-match a victim org.
    IF split_part(addr, '@', 3) <> '' THEN
        RETURN NULL;
    END IF;
    local_part := split_part(local_part, '+', 1);
    addr := local_part || '@' || domain_part;

    SELECT o.id INTO org_id
    FROM app.organizations AS o
    WHERE lower(o.inbound_email_alias) = addr;
    IF org_id IS NOT NULL THEN
        RETURN org_id;
    END IF;

    SELECT o.id INTO org_id
    FROM app.organizations AS o
    WHERE o.slug = local_part
      AND domain_part = 'in.quotepilot.app';
    RETURN org_id;
END;
$fn$;

REVOKE EXECUTE ON FUNCTION app.resolve_inbound_org(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.resolve_inbound_org(text) TO qp_app;
-- +goose StatementEnd

-- list_memberships_for_email: the post-login organization picker; returns only
-- (organization_id, role), never the registry itself (comment 9).
-- +goose StatementBegin
-- p_email, not email: in a LANGUAGE sql function a column name wins over a
-- parameter name, so an "email" parameter would bind to u.email and the
-- predicate would collapse to a tautology, leaking every membership row.
CREATE FUNCTION app.list_memberships_for_email(p_email text)
    RETURNS TABLE (organization_id uuid, role app.membership_role)
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = app
AS $fn$
    SELECT m.organization_id, m.role
    FROM app.organization_memberships AS m
    JOIN app.users AS u ON u.id = m.user_id
    WHERE lower(u.email) = lower(btrim(p_email));
$fn$;

REVOKE EXECUTE ON FUNCTION app.list_memberships_for_email(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.list_memberships_for_email(text) TO qp_app;
-- +goose StatementEnd

-- get_current_organization: the tenant-scoped accessor used inside InTenantTx;
-- returns the row for the current tenant GUC and NULL/no row otherwise, so
-- workers read branding without a direct Layer A grant (comment 9).
-- +goose StatementBegin
CREATE FUNCTION app.get_current_organization() RETURNS app.organizations
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = app
AS $fn$
    SELECT o.*
    FROM app.organizations AS o
    WHERE o.id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid;
$fn$;

REVOKE EXECUTE ON FUNCTION app.get_current_organization() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.get_current_organization() TO qp_app;
-- +goose StatementEnd

-- platform_insert_public_snapshot: the only write path into immutable
-- snapshots, called by the platform path as qp_admin (comment 15); also
-- refreshes connectors.last_scraped_at.
-- +goose StatementBegin
CREATE FUNCTION app.platform_insert_public_snapshot(
    p_connector_slug text,
    p_parser_version text,
    p_source_url text,
    p_raw_markdown_hash text,
    p_storage_key text,
    p_extracted_rates_json jsonb,
    p_confidence_score numeric,
    p_check_results_json jsonb,
    p_scraped_at timestamptz
) RETURNS uuid
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = app
AS $fn$
DECLARE
    v_connector_id uuid;
    v_snapshot_id uuid;
BEGIN
    SELECT c.id INTO v_connector_id FROM app.connectors AS c WHERE c.slug = p_connector_slug;
    IF v_connector_id IS NULL THEN
        RAISE EXCEPTION 'platform_insert_public_snapshot: unknown connector slug %', p_connector_slug;
    END IF;

    INSERT INTO app.public_snapshots AS s (
        connector_id, parser_version, raw_markdown_hash, extracted_rates_json,
        source_url, storage_key, confidence_score, check_results_json, scraped_at
    ) VALUES (
        v_connector_id, p_parser_version, p_raw_markdown_hash, p_extracted_rates_json,
        p_source_url, p_storage_key, p_confidence_score,
        coalesce(p_check_results_json, '[]'::jsonb), p_scraped_at
    )
    RETURNING s.id INTO v_snapshot_id;

    UPDATE app.connectors AS c
    SET last_scraped_at = greatest(coalesce(c.last_scraped_at, p_scraped_at), p_scraped_at),
        updated_at = now()
    WHERE c.id = v_connector_id;

    RETURN v_snapshot_id;
END;
$fn$;

REVOKE EXECUTE ON FUNCTION app.platform_insert_public_snapshot(text, text, text, text, text, jsonb, numeric, jsonb, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.platform_insert_public_snapshot(text, text, text, text, text, jsonb, numeric, jsonb, timestamptz) TO qp_admin;
-- +goose StatementEnd

-- platform_update_connector_status: HEALTHY -> DEGRADED -> HEALING -> HEALTHY
-- transitions driven by the deterministic checks (RFC-003 section 4).
-- +goose StatementBegin
CREATE FUNCTION app.platform_update_connector_status(
    p_connector_slug text,
    p_status app.connector_status,
    p_confidence_score numeric DEFAULT NULL
) RETURNS void
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = app
AS $fn$
DECLARE
    v_current app.connector_status;
BEGIN
    SELECT c.status INTO v_current
    FROM app.connectors AS c
    WHERE c.slug = p_connector_slug;
    IF v_current IS NULL THEN
        RAISE EXCEPTION 'platform_update_connector_status: unknown connector slug %', p_connector_slug;
    END IF;
    -- One-way cycle only: HEALTHY -> DEGRADED -> HEALING -> HEALTHY. Any
    -- other edge (skipping a stage, going backwards, same-status) is refused
    -- so a connector cannot leave DEGRADED without a healing pass.
    IF NOT ((v_current = 'HEALTHY'  AND p_status = 'DEGRADED')
         OR (v_current = 'DEGRADED' AND p_status = 'HEALING')
         OR (v_current = 'HEALING'  AND p_status = 'HEALTHY')) THEN
        RAISE EXCEPTION 'platform_update_connector_status: % -> % is not an allowed transition', v_current, p_status;
    END IF;

    UPDATE app.connectors AS c
    SET status = p_status,
        confidence_score = coalesce(p_confidence_score, c.confidence_score),
        updated_at = now()
    WHERE c.slug = p_connector_slug;
END;
$fn$;

REVOKE EXECUTE ON FUNCTION app.platform_update_connector_status(text, app.connector_status, numeric) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.platform_update_connector_status(text, app.connector_status, numeric) TO qp_admin;
-- +goose StatementEnd

-- platform_insert_healing_session: opens a repair session for a DEGRADED
-- connector (RFC-003 section 6); the failing payload is referenced by hash and
-- storage key, never stored inline (comment 15).
-- +goose StatementBegin
CREATE FUNCTION app.platform_insert_healing_session(
    p_connector_slug text,
    p_failing_snapshot_id uuid DEFAULT NULL,
    p_failing_payload_hash text DEFAULT NULL,
    p_failing_payload_storage_key text DEFAULT NULL,
    p_devin_session_id text DEFAULT NULL,
    p_devin_session_url text DEFAULT NULL
) RETURNS uuid
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = app
AS $fn$
DECLARE
    v_connector_id uuid;
    v_session_id uuid;
BEGIN
    SELECT c.id INTO v_connector_id FROM app.connectors AS c WHERE c.slug = p_connector_slug;
    IF v_connector_id IS NULL THEN
        RAISE EXCEPTION 'platform_insert_healing_session: unknown connector slug %', p_connector_slug;
    END IF;

    -- The pinned failing snapshot must belong to the connector being healed:
    -- healing_sessions has no composite FK, so without this join the session
    -- could pin another connector's snapshot (review finding).
    IF p_failing_snapshot_id IS NOT NULL AND NOT EXISTS (
        SELECT 1
        FROM app.public_snapshots AS s
        WHERE s.id = p_failing_snapshot_id
          AND s.connector_id = v_connector_id
    ) THEN
        RAISE EXCEPTION 'platform_insert_healing_session: snapshot % does not belong to connector %', p_failing_snapshot_id, p_connector_slug;
    END IF;

    INSERT INTO app.healing_sessions AS h (
        connector_id, failing_snapshot_id, failing_payload_hash,
        failing_payload_storage_key, devin_session_id, devin_session_url
    ) VALUES (
        v_connector_id, p_failing_snapshot_id, p_failing_payload_hash,
        p_failing_payload_storage_key, p_devin_session_id, p_devin_session_url
    )
    RETURNING h.id INTO v_session_id;

    RETURN v_session_id;
END;
$fn$;

REVOKE EXECUTE ON FUNCTION app.platform_insert_healing_session(text, uuid, text, text, text, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.platform_insert_healing_session(text, uuid, text, text, text, text) TO qp_admin;
-- +goose StatementEnd

-- platform_update_healing_session: reports session progress (PR opened,
-- tests green, merged or failed). Without it a healing_sessions row could
-- never leave OPEN — qp_admin holds no table write grant by design
-- (comment 15). MERGED/FAILED stamp resolved_at once.
-- +goose StatementBegin
CREATE FUNCTION app.platform_update_healing_session(
    p_session_id uuid,
    p_status app.healing_status,
    p_github_pr_url text DEFAULT NULL,
    p_pytest_passed boolean DEFAULT NULL,
    p_devin_session_url text DEFAULT NULL
) RETURNS void
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = app
AS $fn$
DECLARE
    v_current app.healing_status;
BEGIN
    SELECT h.status INTO v_current
    FROM app.healing_sessions AS h
    WHERE h.id = p_session_id;
    IF v_current IS NULL THEN
        RAISE EXCEPTION 'platform_update_healing_session: unknown session %', p_session_id;
    END IF;
    -- OPEN -> PR_READY -> MERGED|FAILED (OPEN may resolve directly); MERGED
    -- and FAILED are terminal. The map is what keeps a resolved session from
    -- reopening with resolved_at still stamped (review finding).
    IF NOT ((v_current = 'OPEN'     AND p_status IN ('PR_READY', 'MERGED', 'FAILED'))
         OR (v_current = 'PR_READY' AND p_status IN ('MERGED', 'FAILED'))) THEN
        RAISE EXCEPTION 'platform_update_healing_session: % -> % is not an allowed transition', v_current, p_status;
    END IF;

    UPDATE app.healing_sessions AS h
    SET status = p_status,
        github_pr_url = coalesce(p_github_pr_url, h.github_pr_url),
        pytest_passed = coalesce(p_pytest_passed, h.pytest_passed),
        devin_session_url = coalesce(p_devin_session_url, h.devin_session_url),
        resolved_at = CASE
            WHEN p_status IN ('MERGED', 'FAILED') THEN coalesce(h.resolved_at, now())
            ELSE h.resolved_at
        END
    WHERE h.id = p_session_id;
END;
$fn$;

REVOKE EXECUTE ON FUNCTION app.platform_update_healing_session(uuid, app.healing_status, text, boolean, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.platform_update_healing_session(uuid, app.healing_status, text, boolean, text) TO qp_admin;
-- +goose StatementEnd

-- platform_delete_organization: controlled tenant teardown (spec section 4).
-- The ONLY delete path for an organization — qp_app holds no privilege on
-- Layer A and nothing else can reach the registry. EXECUTE goes to qp_admin,
-- never qp_app: the exposed tenant credential cannot delete an org at all
-- (Devin Review — granting qp_app EXECUTE left a single in-function scope
-- check between the API role and tenant deletion). Teardown deletes only the
-- tenant the caller is already scoped into: the guard at the top of the body
-- requires an InTenantTx pinned to p_org_id, so qp_admin's EXECUTE grant can
-- no longer reach an arbitrary org (Devin Review — the API opens that tenant
-- transaction only after admin authorization). A deliberately malicious
-- qp_admin could still pin any context itself; that residual is the accepted
-- GUC trust model documented in the spec — authorizing WHO may call the
-- platform path stays the API layer's job. Two transaction-local GUCs make
-- the cascade work under the role model:
--   app.current_organization_id — pinned by the caller and verified here;
--     the cascaded deletes need it because the function runs as
--     qp_migrator, which is NOBYPASSRLS and subject to FORCE RLS like
--     everyone: without a tenant context the USING clause hides every
--     tenant row and the org row could not be removed (verified in the
--     lint suite: an unscoped migrator sees zero tenant rows).
--   app.tenant_teardown — tells block_approved_quote_mutation and
--     block_sent_quote_line_mutation this is the deliberate teardown path, so
--     the sent-quote immutability rules do not block the cascade. The flag
--     alone is forgeable (any role can set_config), so both triggers also
--     require current_user <> session_user — true only inside a SECURITY
--     DEFINER body like this one, where the effective user is qp_migrator.
--     qp_app and qp_admin hold no role memberships (init section 2,
--     roles.sql [B]), so SET ROLE cannot fake the difference either.
-- set_config(..., true) is transaction-local, so neither flag can leak into
-- the pooled session the call rode in on.
-- +goose StatementBegin
CREATE FUNCTION app.platform_delete_organization(p_org_id uuid) RETURNS void
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = app
AS $fn$
BEGIN
    -- Scope first, before the teardown flag is raised: an InTenantTx pinned
    -- to a different org (or no tenant context at all) must not delete.
    IF NULLIF(current_setting('app.current_organization_id', true), '')::uuid IS DISTINCT FROM p_org_id THEN
        RAISE EXCEPTION 'platform_delete_organization must run inside an InTenantTx scoped to organization %', p_org_id;
    END IF;
    PERFORM set_config('app.tenant_teardown', 'on', true);
    DELETE FROM app.organizations AS o WHERE o.id = p_org_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'platform_delete_organization: unknown organization %', p_org_id;
    END IF;
END;
$fn$;

REVOKE EXECUTE ON FUNCTION app.platform_delete_organization(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.platform_delete_organization(uuid) TO qp_admin;
-- +goose StatementEnd

-- qp_admin also needs USAGE on the enum types in the platform signatures:
-- EXECUTE on the function alone does not cover argument/return types, so a
-- call typed with app.connector_status or app.healing_status would raise
-- permission denied for type. Types carry PUBLIC USAGE by default; the
-- explicit grant keeps the qp_admin contract working even if a later
-- migration revokes that default.
-- +goose StatementBegin
GRANT USAGE ON TYPE app.healing_status, app.connector_status TO qp_admin;
-- +goose StatementEnd

-- =========================================================================
-- 7. Immutability triggers (comments 13 and 15, rule 9).
-- Plain (non-DEFINER) trigger functions: raising needs no privilege. The
-- trigger fires for every role, qp_migrator included — that is what makes the
-- lint test's owner-side UPDATE raise.
-- =========================================================================
-- +goose StatementBegin
CREATE FUNCTION app.block_snapshot_mutation() RETURNS trigger
    LANGUAGE plpgsql
AS $fn$
BEGIN
    RAISE EXCEPTION 'app.public_snapshots is immutable: % is not allowed (rule 9, comment 15)', TG_OP;
END;
$fn$;

CREATE TRIGGER public_snapshots_immutable
    BEFORE UPDATE OR DELETE ON app.public_snapshots
    FOR EACH ROW EXECUTE FUNCTION app.block_snapshot_mutation();

CREATE FUNCTION app.block_approved_quote_mutation() RETURNS trigger
    LANGUAGE plpgsql
AS $fn$
BEGIN
    -- Teardown first, but the flag alone is forgeable: qp_app can run
    -- set_config('app.tenant_teardown', 'on', true) in its own transaction.
    -- The bypass therefore also requires current_user <> session_user, which
    -- holds only while executing inside a SECURITY DEFINER function
    -- (platform_delete_organization, owned by qp_migrator and executable by
    -- qp_admin only). A direct qp_app write — qp_app is the only role with
    -- tenant table writes; qp_admin holds none — has
    -- current_user = session_user = qp_app and can satisfy neither side of
    -- the difference.
    IF current_setting('app.tenant_teardown', true) = 'on'
       AND current_user <> session_user THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;
    -- DRAFT rows stay fully editable. INSERT is not covered at all: a quote
    -- can be born APPROVED_SENT and the trigger (UPDATE OR DELETE only) lets
    -- it through on purpose — the state machine reaches the status only via
    -- the approval endpoint anyway. DELETE ignores NEW and UPDATE stores the
    -- row it returns, so the allowed path must return the right record.
    IF OLD.status NOT IN ('APPROVED_SENT', 'SUPERSEDED') THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;
    -- The only permitted mutation on a sent quote is status -> SUPERSEDED,
    -- alone: a corrected quote arrives as the next version and this row is
    -- retired to supersede it (rule 9). The jsonb comparison requires every
    -- other column to be identical, so no field edit rides along.
    IF TG_OP = 'UPDATE'
       AND OLD.status = 'APPROVED_SENT'
       AND NEW.status = 'SUPERSEDED'
       AND (to_jsonb(NEW) - 'status') = (to_jsonb(OLD) - 'status') THEN
        RETURN NEW;
    END IF;
    RAISE EXCEPTION 'quote % is %: % is not allowed; create a new quote version (rule 9)', OLD.id, OLD.status, TG_OP;
END;
$fn$;

CREATE TRIGGER quotes_approved_immutable
    BEFORE UPDATE OR DELETE ON app.quotes
    FOR EACH ROW EXECUTE FUNCTION app.block_approved_quote_mutation();

-- block_sent_quote_line_mutation: a sent (or retired) quote's lines are as
-- immutable as the header — the quote must be reproducible exactly as sent
-- (rule 9). Guarded by the same teardown flag.
CREATE FUNCTION app.block_sent_quote_line_mutation() RETURNS trigger
    LANGUAGE plpgsql
AS $fn$
DECLARE
    old_parent_status app.quote_status;
    new_parent_status app.quote_status;
BEGIN
    -- Same teardown contract as block_approved_quote_mutation: the flag must
    -- ride inside a SECURITY DEFINER context (current_user <> session_user)
    -- or qp_app — the only role that writes tenant rows directly — could
    -- forge it with a bare set_config.
    IF current_setting('app.tenant_teardown', true) = 'on'
       AND current_user <> session_user THEN
        IF TG_OP = 'DELETE' THEN
            RETURN OLD;
        END IF;
        RETURN NEW;
    END IF;
    -- UPDATE/DELETE is governed by the OLD parent: a sent (or retired)
    -- quote's lines can never leave it. Checking coalesce(NEW, OLD) here
    -- would let UPDATE repoint a sent quote's line to a DRAFT parent and
    -- detach it into mutability (review finding).
    IF TG_OP IN ('UPDATE', 'DELETE') THEN
        SELECT q.status INTO old_parent_status
        FROM app.quotes AS q
        WHERE q.id = OLD.quote_id;
        IF old_parent_status IN ('APPROVED_SENT', 'SUPERSEDED') THEN
            RAISE EXCEPTION 'quote % is %: line % is not allowed (rule 9)',
                OLD.quote_id, old_parent_status, TG_OP;
        END IF;
        IF TG_OP = 'DELETE' THEN
            -- A NULL parent means the cascade already removed it — the
            -- delete is legitimate teardown of a DRAFT subtree, not a
            -- mutation.
            RETURN OLD;
        END IF;
    END IF;
    -- INSERT/UPDATE is governed by the NEW parent: a line can neither attach
    -- to a sent quote nor to one the tenant cannot see (a missing row under
    -- RLS means a cross-tenant quote_id).
    SELECT q.status INTO new_parent_status
    FROM app.quotes AS q
    WHERE q.id = NEW.quote_id;
    IF new_parent_status IN ('APPROVED_SENT', 'SUPERSEDED') THEN
        RAISE EXCEPTION 'quote % is %: line % is not allowed (rule 9)',
            NEW.quote_id, new_parent_status, TG_OP;
    END IF;
    IF new_parent_status IS NULL THEN
        RAISE EXCEPTION 'quote % is not visible to this tenant: % refused',
            NEW.quote_id, TG_OP;
    END IF;
    RETURN NEW;
END;
$fn$;

CREATE TRIGGER quote_line_items_sent_immutable
    BEFORE INSERT OR UPDATE OR DELETE ON app.quote_line_items
    FOR EACH ROW EXECUTE FUNCTION app.block_sent_quote_line_mutation();

-- enforce_batch_activation: a batch lands PENDING_REVIEW, activation is the
-- single one-way transition, and ACTIVE is terminal (rule 9 — quotes may
-- already pin the batch's tariffs). activated_at is stamped once by this
-- trigger; writing it in any other UPDATE raises.
CREATE FUNCTION app.enforce_batch_activation() RETURNS trigger
    LANGUAGE plpgsql
AS $fn$
BEGIN
    IF OLD.status = 'ACTIVE' THEN
        RAISE EXCEPTION 'batch % is ACTIVE: UPDATE is not allowed; corrections are new batches', OLD.id;
    END IF;
    -- OLD is PENDING_REVIEW (the only other enum value). A PENDING_REVIEW ->
    -- PENDING_REVIEW edit stays allowed but may not touch the stamp.
    IF NEW.status = 'PENDING_REVIEW' THEN
        IF NEW.activated_at IS DISTINCT FROM OLD.activated_at THEN
            RAISE EXCEPTION 'activated_at is stamped by the PENDING_REVIEW -> ACTIVE transition only';
        END IF;
        RETURN NEW;
    END IF;
    -- PENDING_REVIEW -> ACTIVE: keep a stamp the caller pre-set inside this
    -- same transition, otherwise stamp now; a stamp that already exists can
    -- never be rewritten.
    IF OLD.activated_at IS NOT NULL AND NEW.activated_at IS DISTINCT FROM OLD.activated_at THEN
        RAISE EXCEPTION 'activated_at is immutable once stamped';
    END IF;
    NEW.activated_at := coalesce(NEW.activated_at, OLD.activated_at, now());
    RETURN NEW;
END;
$fn$;

CREATE TRIGGER private_tariff_batches_activation
    BEFORE UPDATE ON app.private_tariff_batches
    FOR EACH ROW EXECUTE FUNCTION app.enforce_batch_activation();
-- +goose StatementEnd

-- Rounding rule (RFC-001 sections 6 and 8): sell lines are rounded half-up per
-- line and the price is the sum of the rounded lines; the lines-sum invariant
-- is tested, so the column documents where the rule lives.
-- +goose StatementBegin
COMMENT ON COLUMN app.quotes.final_price_brl IS
    'Sum of the rounded line sell amounts (half-up per line; RFC-001 section 8). Invariant under test: sum(quote_line_items.sell_amount_brl) = final_price_brl.';
-- +goose StatementEnd

-- =========================================================================
-- +goose Down
-- Reverse order: triggers first (they depend on the trigger functions),
-- then functions (their signatures depend on table composite and enum
-- types), tables (FK-safe), the spike embeddings table restored, the
-- policy rename reverted, then the enum types.
-- =========================================================================
-- +goose StatementBegin
DROP TRIGGER private_tariff_batches_activation ON app.private_tariff_batches;
DROP TRIGGER quote_line_items_sent_immutable ON app.quote_line_items;
DROP TRIGGER quotes_approved_immutable ON app.quotes;
DROP TRIGGER public_snapshots_immutable ON app.public_snapshots;
-- +goose StatementEnd

-- +goose StatementBegin
DROP FUNCTION app.enforce_batch_activation();
DROP FUNCTION app.block_sent_quote_line_mutation();
DROP FUNCTION app.block_approved_quote_mutation();
DROP FUNCTION app.block_snapshot_mutation();
DROP FUNCTION app.platform_delete_organization(uuid);
DROP FUNCTION app.platform_update_healing_session(uuid, app.healing_status, text, boolean, text);
DROP FUNCTION app.platform_insert_healing_session(text, uuid, text, text, text, text);
DROP FUNCTION app.platform_update_connector_status(text, app.connector_status, numeric);
DROP FUNCTION app.platform_insert_public_snapshot(text, text, text, text, text, jsonb, numeric, jsonb, timestamptz);
DROP FUNCTION app.get_current_organization();
DROP FUNCTION app.list_memberships_for_email(text);
DROP FUNCTION app.resolve_inbound_org(text);
-- +goose StatementEnd

-- Dropped before the tables: the FK references app.organizations, and
-- restoring the 0001 shape means inbound_messages must lose it explicitly.
-- +goose StatementBegin
ALTER TABLE app.inbound_messages DROP CONSTRAINT inbound_messages_organization_id_fkey;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE app.suppression_list;
DROP TABLE app.organization_connector_preferences;
DROP TABLE app.tenant_audit_logs;
DROP TABLE app.quote_line_items;
DROP TABLE app.quotes;
DROP TABLE app.org_quote_counters;
DROP TABLE app.carrier_offers;
DROP TABLE app.rfq_attachments;
DROP TABLE app.rfq_requests;
DROP TABLE app.private_tariffs;
DROP TABLE app.private_tariff_batches;
DROP TABLE app.pricing_rules;
DROP TABLE app.healing_sessions;
DROP TABLE app.public_snapshots;
DROP TABLE app.connectors;
DROP TABLE app.organization_memberships;
DROP TABLE app.users;
DROP TABLE app.organizations;
-- +goose StatementEnd

-- Restored exactly as 0001 created it, including its original policy name.
-- +goose StatementBegin
CREATE TABLE app.private_tariff_embeddings (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    embedding extensions.vector(1024) NOT NULL,
    embedding_model text NOT NULL
);

ALTER TABLE app.private_tariff_embeddings ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.private_tariff_embeddings FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON app.private_tariff_embeddings
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- Back to the 0001 policy name (the canonical rename above is reverted).
-- +goose StatementBegin
ALTER POLICY tenant_isolation_policy ON app.inbound_messages RENAME TO tenant_isolation;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TYPE app.healing_status;
DROP TYPE app.connector_status;
DROP TYPE app.charge_category;
DROP TYPE app.line_source_type;
DROP TYPE app.quote_line_category;
DROP TYPE app.quote_status;
DROP TYPE app.rfq_status;
DROP TYPE app.rfq_kind;
DROP TYPE app.batch_status;
DROP TYPE app.tariff_source_type;
DROP TYPE app.pricing_modal;
DROP TYPE app.freight_modal;
DROP TYPE app.membership_role;
-- +goose StatementEnd
