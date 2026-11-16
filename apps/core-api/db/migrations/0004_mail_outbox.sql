-- Mail outbox for durable magic-link delivery (ADR-005, durable-send plan):
-- the login endpoint's detached-goroutine Resend call becomes a
-- same-transaction outbox insert plus a River job a worker drains later, so
-- a crash between token mint and send can no longer strand a login link.
--
-- app.auth_mail_outbox is Layer A like the 0003 auth tables: it is written
-- before any tenant context exists, so it carries no organization_id and no
-- RLS — REVOKE ALL undoes the migrator default grant and qp_app reaches it
-- only through the two SECURITY DEFINER functions below. qp_admin receives
-- nothing, the inverse of the platform_* path.
--
-- link_url is a live credential: the URL embeds the unhashed login token,
-- so reading this table is equivalent to minting a session. That is the
-- same exposure class qp_app already holds via EXECUTE on
-- app.create_login_token — it can mint a token for any user; honestly,
-- EXECUTE on create_session is strictly stronger — and the at-rest copy
-- is bounded two ways: the embedded token dies with
-- auth_login_tokens.expires_at (the login TTL), and delete-on-send keeps
-- the table holding only pending sends. A River job arg carries the
-- outbox id, never the link: river_job args are qp_app-readable at rest,
-- so a URL there would be a second live-credential copy outside this
-- retention story. A compromised qp_app could still harvest pending links
-- through read_mail_outbox — the honest residual, bounded by TTL and
-- pending-send volume.

-- +goose Up

-- =========================================================================
-- 1. The outbox table. No RLS and no qp_app grant — functions only, the
-- Layer A pattern. login_token_id is NO ACTION like the 0003 FK: the token
-- row is the audit record, and an undelivered outbox row blocks its
-- deletion rather than orphaning. No index — delete-on-send keeps the
-- table at pending-send size, so a sweeper's sequential scan costs
-- nothing.
-- =========================================================================
-- +goose StatementBegin
CREATE TABLE app.auth_mail_outbox (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    login_token_id uuid NOT NULL REFERENCES app.auth_login_tokens (id),
    link_url       text NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

REVOKE ALL ON app.auth_mail_outbox FROM qp_app;
-- +goose StatementEnd

-- =========================================================================
-- 2. The outbox function family: create, read, delete-on-send. Same grant
-- split as the 0003 auth family — qp_app only, nothing for qp_admin or
-- PUBLIC. Delivery is at-least-once by design: the worker reads the link,
-- sends, then deletes — River retries a failed send naturally because the
-- row only disappears after Resend accepts. The one residual is a crash
-- between send and delete, which resends the SAME link — harmless, the
-- link is identical and the token is single-use on consume. The
-- alternatives were rejected: delete-on-take loses the link silently on a
-- post-take crash (a NULL take completes the job), and job args carrying
-- the URL would put a second live credential in river_job.
-- =========================================================================

-- create_mail_outbox: the login path's write — executed in the same
-- transaction as create_login_token so a token is never minted without its
-- send queued. The link rides as a parameterized argument (never a River
-- job arg); an empty link would queue an undeliverable send, so it is
-- refused here rather than at the worker.
-- +goose StatementBegin
CREATE FUNCTION app.create_mail_outbox(p_login_token_id uuid, p_link_url text)
    RETURNS uuid
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = app
AS $fn$
DECLARE
    v_id uuid;
BEGIN
    IF p_link_url IS NULL OR btrim(p_link_url) = '' THEN
        RAISE EXCEPTION 'create_mail_outbox: link_url must be a non-empty string, got ''%''', p_link_url;
    END IF;

    INSERT INTO app.auth_mail_outbox AS m (login_token_id, link_url)
    VALUES (p_login_token_id, p_link_url)
    RETURNING m.id INTO v_id;
    RETURN v_id;
END;
$fn$;

REVOKE EXECUTE ON FUNCTION app.create_mail_outbox(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.create_mail_outbox(uuid, text) TO qp_app;
-- +goose StatementEnd

-- read_mail_outbox: the worker's claim. It joins through the login token
-- to users.email because the job carries only the outbox id and
-- SendMagicLink needs a recipient (Devin Review 🔴). expires_at rides
-- along so the WORKER owns the expiry policy — a past-TTL row means
-- delete and done, not a mailed dead link. A consumed token is already
-- dead (someone held the link), so its row is skipped as if absent: the
-- job completes either way and the row costs only dead weight. An empty
-- result means the job is done — already sent, never queued, or dead —
-- and says nothing else by design.
-- +goose StatementBegin
CREATE FUNCTION app.read_mail_outbox(p_id uuid)
    RETURNS TABLE(link_url text, recipient_email text, expires_at timestamptz)
    LANGUAGE sql
    SECURITY DEFINER
    SET search_path = app
AS $fn$
    SELECT m.link_url, u.email, t.expires_at
    FROM app.auth_mail_outbox AS m
    JOIN app.auth_login_tokens AS t ON t.id = m.login_token_id
    JOIN app.users AS u ON u.id = t.user_id
    WHERE m.id = p_id
      AND t.consumed_at IS NULL;
$fn$;

REVOKE EXECUTE ON FUNCTION app.read_mail_outbox(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.read_mail_outbox(uuid) TO qp_app;
-- +goose StatementEnd

-- delete_mail_outbox: the delivery receipt, executed only after a
-- successful send. Returns whether a row existed — a missing row is
-- already the goal, so the caller treats false as done, not error.
-- +goose StatementBegin
CREATE FUNCTION app.delete_mail_outbox(p_id uuid) RETURNS boolean
    LANGUAGE sql
    SECURITY DEFINER
    SET search_path = app
AS $fn$
    DELETE FROM app.auth_mail_outbox AS m
    WHERE m.id = p_id
    RETURNING true;
$fn$;

REVOKE EXECUTE ON FUNCTION app.delete_mail_outbox(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.delete_mail_outbox(uuid) TO qp_app;
-- +goose StatementEnd

-- =========================================================================
-- +goose Down
-- Functions first (their bodies depend on the table), then the table.
-- =========================================================================
-- +goose StatementBegin
DROP FUNCTION app.delete_mail_outbox(uuid);
DROP FUNCTION app.read_mail_outbox(uuid);
DROP FUNCTION app.create_mail_outbox(uuid, text);
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE app.auth_mail_outbox;
-- +goose StatementEnd
