-- 0011: app.fx_rates — the pinned FX rate table the S3b pricing worker
-- feeds pricing.Input.FXRates from (spec 2026-11-10-pricing-engine-v1.3
-- section 6, rule 9).
--
-- Platform-global market data, like connectors/public_snapshots: a PTAX
-- bulletin or a carrier rate sheet is the same fact for every tenant, so
-- the table carries no organization_id and no RLS — it joins the
-- platformTablesNoRLS lint list instead of tenant_isolation_policy.
--
-- Sources mirror the engine's (source, currency, date) key:
--   CARRIER  carrier-published rate sheet for QuoteDate (spread applies)
--   PTAX     Banco Central bulletin, sell side, previous business day
--   CUSTOMS  the Siscomex rate behind the import CIF goods value
--
-- Rule 9 pinning: a quote records the exact rates it applied in
-- fx_rates_applied_json, so the row a quote resolved must never change
-- under it — a newer bulletin is a new (source, currency, rate_date)
-- row, never an edit. UPDATE and DELETE raise for every role through
-- fx_rates_immutable, mirroring public_snapshots_immutable (0002 §7);
-- re-syncs converge by INSERT ... ON CONFLICT DO NOTHING, which is why
-- the unique key is the pin key.
--
-- Grants follow the append-only convention (0002 comments 11/14): the
-- migrator default gives qp_app SELECT/INSERT/UPDATE/DELETE; the worker
-- needs SELECT to pin rates into Input and INSERT to sync PTAX, so
-- UPDATE and DELETE are revoked. The trigger still covers qp_migrator —
-- grants cannot explain a failure the lint test asserts (same shape as
-- TestSchemaEnforcementSnapshotImmutability).

-- +goose Up
-- +goose StatementBegin
CREATE TABLE app.fx_rates (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- varchar, not an enum: the S3b worker syncs PTAX today and the
    -- CARRIER/CUSTOMS feeds land as data later — a new source is an
    -- INSERT, not a type rebuild (same call as 0010's CHECKs).
    source varchar(7) NOT NULL,
    currency char(3) NOT NULL,
    -- BRL per one unit of currency, sell side (spec §6).
    rate numeric(10, 6) NOT NULL,
    rate_date date NOT NULL,
    fetched_at timestamptz NOT NULL DEFAULT now(),
    -- Provenance of the pin, e.g. 'bcb-ptax 2025-11-14 13:11:28.202' for
    -- the closing bulletin the row was taken from.
    source_ref text,
    CHECK (source IN ('CARRIER', 'PTAX', 'CUSTOMS')),
    CHECK (currency ~ '^[A-Z]{3}$'),
    CHECK (rate > 0),
    UNIQUE (source, currency, rate_date)
);

REVOKE UPDATE, DELETE ON app.fx_rates FROM qp_app;

-- Plain (non-DEFINER) trigger function like block_snapshot_mutation:
-- raising needs no privilege and fires for every role, qp_migrator
-- included — a grant can never explain a survived UPDATE/DELETE.
CREATE FUNCTION app.block_fx_rate_mutation() RETURNS trigger
    LANGUAGE plpgsql
AS $fn$
BEGIN
    RAISE EXCEPTION 'app.fx_rates is immutable: % is not allowed (rule 9)', TG_OP;
END;
$fn$;

CREATE TRIGGER fx_rates_immutable
    BEFORE UPDATE OR DELETE ON app.fx_rates
    FOR EACH ROW EXECUTE FUNCTION app.block_fx_rate_mutation();
-- +goose StatementEnd

-- =========================================================================
-- +goose Down
-- =========================================================================
-- Trigger first (it depends on the function), then function and table —
-- the same drop order 0002 uses.
-- +goose StatementBegin
DROP TRIGGER fx_rates_immutable ON app.fx_rates;
DROP FUNCTION app.block_fx_rate_mutation();
DROP TABLE app.fx_rates;
-- +goose StatementEnd
