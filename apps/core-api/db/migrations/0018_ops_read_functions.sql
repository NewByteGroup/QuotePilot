-- 0018: the read side of the ops console (ADR-005 platform path). The
-- surface so far gave qp_app exactly one way into each Layer A table it
-- needs — a SECURITY DEFINER function per operation — and the ops reads
-- extend the same model instead of opening table grants:
--
--   * app.list_inbound_quarantine is the only SELECT into
--     app.inbound_quarantine. 0016 revoked every table privilege from
--     qp_app on purpose: the drop path writes through
--     record_inbound_quarantine and "reaches nothing back out", so a
--     hammered webhook could never probe whether a given message landed
--     in quarantine. This function relaxes exactly that read for the ops
--     console — behind RequirePlatformAdmin, which is what makes
--     exposing it acceptable: the gate proves users.is_platform_admin on
--     every request before a query is emitted. The threat model shifts
--     accordingly: a compromised qp_app can now READ triage metadata
--     (attacker-controlled mail fields) it could not before. That is the
--     accepted residual of a readable quarantine — the table holds no
--     credential class and no tenant content, and the function still
--     carries no write, so the write surface is unchanged.
--   * app.is_platform_admin is the gate's own lookup. app.users is
--     Layer A (REVOKE ALL, function-gated), and the flag deliberately
--     rides no JWT claim — ADR-005 section 5's rule that
--     authorization-relevant state is re-read per request, never cached
--     in the token. The function returns one boolean; an unknown user id
--     is false, not an error, so "exists vs not" stays unobservable.
--
-- Both functions ship pinned to the post-0012 search_path convention
-- (pg_catalog, pg_temp — every reference fully app-qualified), EXECUTE
-- revoked from PUBLIC and granted to qp_app only. qp_admin receives
-- nothing: its surface stays the platform five (platform-functions.sql
-- assertion [N]); the ops console runs as qp_app like every other
-- request path.
--
-- list_inbound_quarantine RETURNS TABLE rather than SETOF
-- app.inbound_quarantine because the ops contract paginates with a
-- total: the count(*) OVER () column rides every row, so one function
-- call — one round trip — carries page and total together (an empty
-- page simply has no row to read it from). p_limit is clamped to
-- [1, 200] and p_offset to [0, ∞): a read function degrades a bad page
-- request to the nearest valid one rather than raising — the handler
-- still bounds the wire parameter, this is the second fence.
-- received_at DESC, id DESC is the stable order: received_at can tie
-- (two drops inside one timestamp tick), so id is the deterministic
-- tiebreak — pagination over an unstable order replays or skips rows.

-- +goose Up

-- =========================================================================
-- 1. is_platform_admin: the RequirePlatformAdmin middleware's per-request
-- check. SECURITY DEFINER because app.users is Layer A; STABLE like the
-- rest of the read family. The coalesce collapses "unknown user" and
-- "not admin" into one false — the gate cannot oracle user existence.
-- =========================================================================
-- +goose StatementBegin
CREATE FUNCTION app.is_platform_admin(p_user_id uuid) RETURNS boolean
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
    SELECT coalesce(
        (SELECT u.is_platform_admin FROM app.users AS u WHERE u.id = p_user_id),
        false);
$fn$;

REVOKE EXECUTE ON FUNCTION app.is_platform_admin(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.is_platform_admin(uuid) TO qp_app;
-- +goose StatementEnd

-- =========================================================================
-- 2. list_inbound_quarantine: the ops console's only read into 0016's
-- table. Page bounds are clamped, not rejected — see the header comment.
-- The recipients payload passes through verbatim (jsonb, the `to` array
-- exactly as received); the handler owns the camelCase mapping.
-- =========================================================================
-- +goose StatementBegin
CREATE FUNCTION app.list_inbound_quarantine(p_limit integer, p_offset integer)
    RETURNS TABLE (
        id uuid,
        resend_message_id text,
        reason text,
        recipients jsonb,
        matched_org_ids uuid[],
        subject text,
        from_addr text,
        received_at timestamptz,
        quarantined_at timestamptz,
        total bigint
    )
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
    SELECT q.id, q.resend_message_id, q.reason, q.recipients, q.matched_org_ids,
           q.subject, q.from_addr, q.received_at, q.quarantined_at,
           count(*) OVER () AS total
    FROM app.inbound_quarantine AS q
    ORDER BY q.received_at DESC, q.id DESC
    LIMIT LEAST(GREATEST(coalesce(p_limit, 50), 1), 200)
    OFFSET GREATEST(coalesce(p_offset, 0), 0);
$fn$;

REVOKE EXECUTE ON FUNCTION app.list_inbound_quarantine(integer, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.list_inbound_quarantine(integer, integer) TO qp_app;
-- +goose StatementEnd

-- =========================================================================
-- +goose Down
-- =========================================================================
-- +goose StatementBegin
DROP FUNCTION app.list_inbound_quarantine(integer, integer);
DROP FUNCTION app.is_platform_admin(uuid);
-- +goose StatementEnd
