-- ADR-005 section 36 alignment for the whole live SECURITY DEFINER surface:
-- every definer function still carrying the 0002/0003/0004-era
-- SET search_path = app moves to the pinned pg_catalog, pg_temp path that
-- 0006 established for get_user_auth_by_email and 0007 extended over the
-- password-reset family. A definer function runs with qp_migrator's
-- privileges, so any name it resolves through a caller-influenced
-- search_path is a hijack vector — a shadow object in pg_temp, or in a
-- schema a future ALTER adds ahead of app, would execute as the owner.
-- Pinning the path to pg_catalog, pg_temp removes it; that is why an
-- unqualified app.* reference is forbidden inside these bodies.
--
-- ALTER FUNCTION … SET changes only the proconfig entry: bodies, owners,
-- volatility and EXECUTE grants stay exactly as their source migrations
-- left them, so this migration cannot drift a body and carries no verbatim
-- copies. It is also idempotent convergence — a dev database whose 0006
-- applied the earlier `= app` text of get_user_auth_by_email (amended in
-- d1b5fa6 after those databases had already run it) gets re-pinned without
-- re-stating its canonical body.
--
-- Out of scope, deliberately untouched:
--   * The 0006/0007 family already created pinned:
--     get_user_auth_by_email's siblings consume_login_token,
--     create_password_reset_token, consume_password_reset_token,
--     read_mail_outbox, rotate_session, set_user_password,
--     revoke_user_pending_tokens, revoke_user_sessions.
--   * 0002/0003's earlier list_memberships_for_email definitions — 0009's
--     is the live one and is hardened below.
--   * Plain trigger functions (block_*_mutation, enforce_batch_activation)
--     — not SECURITY DEFINER, so the path is caller-owned and harmless.

-- +goose Up

-- The 0002 tenant-registry and platform functions.
ALTER FUNCTION app.resolve_inbound_org(text)
    SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION app.get_current_organization()
    SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION app.platform_insert_public_snapshot(text, text, text, text, text, jsonb, numeric, jsonb, timestamp with time zone)
    SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION app.platform_update_connector_status(text, app.connector_status, numeric)
    SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION app.platform_insert_healing_session(text, uuid, text, text, text, text)
    SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION app.platform_update_healing_session(uuid, app.healing_status, text, boolean, text)
    SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION app.platform_delete_organization(uuid)
    SET search_path = pg_catalog, pg_temp;

-- The 0003 auth/session family.
ALTER FUNCTION app.find_user_by_email(text)
    SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION app.create_login_token(uuid, text, timestamp with time zone)
    SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION app.create_session(uuid, text, timestamp with time zone)
    SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION app.revoke_session(uuid)
    SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION app.revoke_session_family_by_hash(text)
    SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION app.get_membership(uuid, uuid)
    SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION app.get_user_profile(uuid)
    SET search_path = pg_catalog, pg_temp;

-- The 0004 mail-outbox pair.
ALTER FUNCTION app.create_mail_outbox(uuid, text)
    SET search_path = pg_catalog, pg_temp;
ALTER FUNCTION app.delete_mail_outbox(uuid)
    SET search_path = pg_catalog, pg_temp;

-- 0009's live membership lookup.
ALTER FUNCTION app.list_memberships_for_email(text)
    SET search_path = pg_catalog, pg_temp;

-- Drift convergence only: canonical 0006 already pins this path, so on a
-- correctly migrated schema this is a no-op; on a database that applied
-- the pre-amendment `= app` text it repairs the pin.
ALTER FUNCTION app.get_user_auth_by_email(text)
    SET search_path = pg_catalog, pg_temp;

-- +goose Down

-- Restore the 17 changed functions to their pre-0012 `= app` path — the
-- same ALTER back, bodies still untouched. get_user_auth_by_email is not
-- restored: its canonical pre-0012 state is already the pinned path, so
-- leaving it pinned IS the faithful rollback.
ALTER FUNCTION app.resolve_inbound_org(text)
    SET search_path = app;
ALTER FUNCTION app.get_current_organization()
    SET search_path = app;
ALTER FUNCTION app.platform_insert_public_snapshot(text, text, text, text, text, jsonb, numeric, jsonb, timestamp with time zone)
    SET search_path = app;
ALTER FUNCTION app.platform_update_connector_status(text, app.connector_status, numeric)
    SET search_path = app;
ALTER FUNCTION app.platform_insert_healing_session(text, uuid, text, text, text, text)
    SET search_path = app;
ALTER FUNCTION app.platform_update_healing_session(uuid, app.healing_status, text, boolean, text)
    SET search_path = app;
ALTER FUNCTION app.platform_delete_organization(uuid)
    SET search_path = app;
ALTER FUNCTION app.find_user_by_email(text)
    SET search_path = app;
ALTER FUNCTION app.create_login_token(uuid, text, timestamp with time zone)
    SET search_path = app;
ALTER FUNCTION app.create_session(uuid, text, timestamp with time zone)
    SET search_path = app;
ALTER FUNCTION app.revoke_session(uuid)
    SET search_path = app;
ALTER FUNCTION app.revoke_session_family_by_hash(text)
    SET search_path = app;
ALTER FUNCTION app.get_membership(uuid, uuid)
    SET search_path = app;
ALTER FUNCTION app.get_user_profile(uuid)
    SET search_path = app;
ALTER FUNCTION app.create_mail_outbox(uuid, text)
    SET search_path = app;
ALTER FUNCTION app.delete_mail_outbox(uuid)
    SET search_path = app;
ALTER FUNCTION app.list_memberships_for_email(text)
    SET search_path = app;
