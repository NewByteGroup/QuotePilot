-- 0015: app.quote_send_claims — the claim row behind the S5 redesign that
-- retires lock-across-provider mutual exclusion (issue #65, decision:
-- claim row). Before it, one InTenantTx wrapped lockSendQuote's FOR UPDATE
-- through Storage.Get and the seconds-long Resend call; the held row lock
-- was the only thing stopping two concurrent quote_send jobs from
-- double-sending, and its quote → rfq_requests update order inverted
-- against the reprocess gate's RFQ → quote probe.
--
-- The claim replaces the held lock: a quote_send job inserts it inside the
-- short claim tx, calls the provider with no transaction open, and deletes
-- it in the same commit that records the send (or the refusal). The PK on
-- quote_id gives one claimant per quote version; ON CONFLICT DO NOTHING
-- turns the insert race into a read of the winner's claimed_by. claimed_by
-- is the owning river_job.id — liveness is checkable against
-- river.river_job.state (live: available, pending, retryable, running,
-- scheduled; terminal or missing is a stale claim the next job steals), so
-- the mutual exclusion needs no timers and no heartbeat writes.
--
-- Layer B tenant table: ENABLE + FORCE RLS + tenant_isolation_policy like
-- every organization_id-bearing table; qp_app's default-privilege DML
-- grant covers the insert/read/delete the worker needs, so no explicit
-- GRANT. organization_id carries no organizations FK by design — the RLS
-- policy supplies the tenant scope and the claim's lifetime is the send
-- attempt's, not the org's.

-- +goose Up

-- +goose StatementBegin
CREATE TABLE app.quote_send_claims (
    -- One claimant per quote version: the PK itself is the exclusion.
    quote_id        uuid PRIMARY KEY REFERENCES app.quotes (id),
    organization_id uuid NOT NULL,
    -- river.river_job.id of the claiming job — deliberately no FK:
    -- river_job is shared-schema state outside the tenant model.
    claimed_by      bigint NOT NULL,
    claimed_at      timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE app.quote_send_claims ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.quote_send_claims FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON app.quote_send_claims
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- =========================================================================
-- +goose Down
-- =========================================================================
-- +goose StatementBegin
DROP TABLE app.quote_send_claims;
-- +goose StatementEnd
