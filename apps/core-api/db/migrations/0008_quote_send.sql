-- Quote send audit index (spec 2026-11-11-staged-quote-pipeline section 9,
-- ratified by Pedro 2026-11-16): the provider message id the S5 quote_send
-- worker stores after a successful Resend send must live in an INDEXED
-- column on the quote version, never only inside audit JSON — so a later
-- outbound_deliveries table backfills with INSERT ... SELECT.
--
-- UNIQUE and partial: only non-NULL values collide. The index is
-- deliberately NOT prefixed by organization_id — provider message ids are
-- globally unique, so cross-tenant uniqueness is itself the double-send
-- guard: a retried send that somehow produced a second provider id cannot
-- be written. Scope warning recorded in the spec stands: this does not
-- enable bounce handling — a bounce webhook arrives tenantless and cannot
-- look this column up under RLS.

-- +goose Up
-- +goose StatementBegin
CREATE UNIQUE INDEX quotes_sent_outbound_message_id_uidx
    ON app.quotes (sent_outbound_message_id)
    WHERE sent_outbound_message_id IS NOT NULL;
-- +goose StatementEnd

-- =========================================================================
-- +goose Down
-- =========================================================================
-- +goose StatementBegin
DROP INDEX app.quotes_sent_outbound_message_id_uidx;
-- +goose StatementEnd
