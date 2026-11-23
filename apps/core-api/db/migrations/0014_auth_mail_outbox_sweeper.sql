-- 0014: app.sweep_auth_mail_outbox — the dead-row sweeper that
-- docs/adr-rfc-plan.md's durable-send entry recorded as the contemplated
-- follow-up: a send that exhausts River's retries discards the job while
-- its outbox row persists as dead weight until token expiry. Expired rows
-- are only the second leak — the first is structural: read_mail_outbox
-- filters consumed tokens before the worker can receipt them, so a spent
-- link's row has no path to deletion at all.
--
-- Dead-row semantics: an outbox row is dead when its auth_login_tokens row
-- is consumed (the single-use link was already used — it can never send
-- again) or expired (the link can never be used). Both are safe to delete:
-- nothing deliverable is lost and no audit trail goes with it — the token
-- row stays; only the pending-send copy goes. The join covers login and
-- password_reset rows alike: purpose lives on the token, and a dead
-- credential is dead regardless of which template it rode.
--
-- Grant split mirrors the 0004 family — qp_app only, nothing for qp_admin
-- or PUBLIC: the hourly worker runs on the same app credential as the
-- sender this follows up.

-- +goose Up

-- sweep_auth_mail_outbox: the periodic worker's whole job in one
-- statement — delete every dead pending send and return how many died so
-- the caller can log it. count(*) forces the bigint return the Store
-- scans; expires_at <= now() matches consume_login_token's > now()
-- boundary exactly, so a token and its outbox row agree on the instant of
-- death.
-- +goose StatementBegin
CREATE FUNCTION app.sweep_auth_mail_outbox() RETURNS bigint
    LANGUAGE sql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
    WITH deleted AS (
        DELETE FROM app.auth_mail_outbox AS m
        USING app.auth_login_tokens AS t
        WHERE m.login_token_id = t.id
          AND (t.consumed_at IS NOT NULL OR t.expires_at <= now())
        RETURNING 1
    )
    SELECT count(*) FROM deleted;
$fn$;

REVOKE EXECUTE ON FUNCTION app.sweep_auth_mail_outbox() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.sweep_auth_mail_outbox() TO qp_app;
-- +goose StatementEnd

-- =========================================================================
-- +goose Down
-- =========================================================================
-- +goose StatementBegin
DROP FUNCTION app.sweep_auth_mail_outbox();
-- +goose StatementEnd
