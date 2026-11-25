-- 0010: carrier_offers becomes the S3a fetch checkpoint (spec
-- 2026-11-11-staged-quote-pipeline S3a, P3 scope decided 2026-11-16).
--
-- One row per fetch-attempt outcome, not one row per found offer:
--   outcome       OFFER_FOUND | NO_OFFER | FETCH_FAILED | NOT_APPLICABLE
--   request_hash  digest of the normalized request (route, equipment,
--                 container count, quote date) — half the idempotency key
--   source        LIVE | MIRROR — which connector produced the row; the UI
--                 labels MIRROR rows simulated by reading this column
--                 (RFC-003), never by looking at the environment.
--
-- CHECKs, not enums: the vocabulary is expected to change while the stage
-- lands (a fourth outcome or source is a migration, not a type rebuild),
-- and the Down below stays fully reversible.
--
-- RLS is untouched: the table already ships ENABLE + FORCE +
-- tenant_isolation_policy (0002) and joins the ADR-008 isolation suite
-- through the catalog-scanned lint (no per-table roster exists).
--
-- Backfill note: every pre-migration row carries a payload by definition
-- (offer_payload_json was NOT NULL), so OFFER_FOUND is the only truthful
-- outcome for them. request_hash is synthesized as 'legacy:<id>' — the
-- original request is not recoverable from the row, and a synthesized
-- hash can never collide with a real request digest: a legacy row must
-- never silently satisfy a re-fetch's idempotency key.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE app.carrier_offers ADD COLUMN outcome text;
ALTER TABLE app.carrier_offers ADD COLUMN request_hash text;
ALTER TABLE app.carrier_offers ADD COLUMN source text NOT NULL DEFAULT 'LIVE';
-- +goose StatementEnd

-- The backfill runs THROUGH the tenant policy like 0009: under FORCE RLS
-- qp_migrator sees only the rows app.current_organization_id names, so the
-- loop points the GUC at each org in turn. The table is empty in dev; the
-- loop is defensive for any database that already carries offers.
-- +goose StatementBegin
DO $backfill$
DECLARE
    o record;
BEGIN
    FOR o IN SELECT id FROM app.organizations LOOP
        PERFORM set_config('app.current_organization_id', o.id::text, true);
        UPDATE app.carrier_offers AS c
        SET outcome = 'OFFER_FOUND',
            request_hash = 'legacy:' || c.id::text
        WHERE c.outcome IS NULL;
    END LOOP;
END
$backfill$;

ALTER TABLE app.carrier_offers ALTER COLUMN outcome SET NOT NULL;
ALTER TABLE app.carrier_offers ALTER COLUMN request_hash SET NOT NULL;
-- +goose StatementEnd

-- carrier goes nullable: a NOT_APPLICABLE checkpoint means no carrier call
-- was owed (AIR, or the Incoterm puts ocean freight on the other party).
-- offer_payload_json goes nullable behind the CHECK below: only
-- OFFER_FOUND rows may carry offer content.
-- +goose StatementBegin
ALTER TABLE app.carrier_offers ALTER COLUMN carrier DROP NOT NULL;
ALTER TABLE app.carrier_offers ALTER COLUMN offer_payload_json DROP NOT NULL;

ALTER TABLE app.carrier_offers
    ADD CONSTRAINT carrier_offers_outcome_check
    CHECK (outcome IN ('OFFER_FOUND', 'NO_OFFER', 'FETCH_FAILED', 'NOT_APPLICABLE'));

ALTER TABLE app.carrier_offers
    ADD CONSTRAINT carrier_offers_source_check
    CHECK (source IN ('LIVE', 'MIRROR'));

-- OFFER_FOUND <=> payload present: a found offer always carries the
-- validated payload it pins; every other outcome never carries content.
ALTER TABLE app.carrier_offers
    ADD CONSTRAINT carrier_offers_payload_outcome_check
    CHECK ((outcome = 'OFFER_FOUND') = (offer_payload_json IS NOT NULL));

-- NULLS NOT DISTINCT so NULL-carrier NOT_APPLICABLE rows still dedupe
-- re-runs (same pattern pricing_rules uses for its NULL locodes, 0002).
ALTER TABLE app.carrier_offers
    ADD CONSTRAINT carrier_offers_rfq_carrier_request_key
    UNIQUE NULLS NOT DISTINCT (rfq_id, carrier, request_hash);
-- +goose StatementEnd

-- =========================================================================
-- +goose Down
-- =========================================================================
-- Lossy: checkpoint rows that cannot exist under the restored NOT NULLs
-- (every non-OFFER_FOUND outcome and any NULL-carrier row) are deleted,
-- again through the tenant policy — an OFFER_FOUND row restores verbatim
-- apart from the dropped checkpoint columns.
-- +goose StatementBegin
DO $down$
DECLARE
    o record;
BEGIN
    FOR o IN SELECT id FROM app.organizations LOOP
        PERFORM set_config('app.current_organization_id', o.id::text, true);
        DELETE FROM app.carrier_offers
        WHERE outcome IS DISTINCT FROM 'OFFER_FOUND' OR carrier IS NULL;
    END LOOP;
END
$down$;

ALTER TABLE app.carrier_offers DROP CONSTRAINT carrier_offers_rfq_carrier_request_key;
ALTER TABLE app.carrier_offers DROP CONSTRAINT carrier_offers_payload_outcome_check;
ALTER TABLE app.carrier_offers DROP CONSTRAINT carrier_offers_source_check;
ALTER TABLE app.carrier_offers DROP CONSTRAINT carrier_offers_outcome_check;

ALTER TABLE app.carrier_offers ALTER COLUMN offer_payload_json SET NOT NULL;
ALTER TABLE app.carrier_offers ALTER COLUMN carrier SET NOT NULL;

ALTER TABLE app.carrier_offers DROP COLUMN outcome;
ALTER TABLE app.carrier_offers DROP COLUMN request_hash;
ALTER TABLE app.carrier_offers DROP COLUMN source;
-- +goose StatementEnd
