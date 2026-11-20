-- users.password_hash + app.get_user_auth_by_email (ADR-005 amendment
-- 2026-11-16): the pilot meeting decided MVP login is email + password
-- verified against a stored argon2id PHC hash — the magic-link pipeline is
-- retained as the reset mechanism and a fallback, not removed. The column is
-- nullable on purpose: provisioned accounts without a set password still
-- cannot log in, and the handler maps a NULL hash to the same uniform 401 a
-- wrong password earns.
--
-- Applied by qp_migrator (object owner). app.users is Layer A — qp_app holds
-- no table privilege on it (0002's REVOKE ALL) — so the credential lookup is
-- one more SECURITY DEFINER function in the auth family, not a direct
-- SELECT: the new column must never cross the function boundary except as
-- this lookup's return value.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE app.users ADD COLUMN password_hash text;

COMMENT ON COLUMN app.users.password_hash IS
    'Argon2id PHC string for password login (ADR-005 amendment 2026-11-16). NULL means the account has no password set — the password path cannot authenticate it, by design. Written by qp_migrator-side provisioning until the set-password flow lands; qp_app only reads it through app.get_user_auth_by_email.';
-- +goose StatementEnd

-- get_user_auth_by_email: the password path's single credential lookup —
-- one call returns the id the session binds to AND the stored hash, so the
-- indistinguishable-401 flow (unknown email, NULL hash, wrong password)
-- reads exactly one row before its argon2id verify. lower(btrim()) mirrors
-- find_user_by_email and the users.email CHECK, which stores lowercase
-- only; an unknown email is an empty set, indistinguishable from no answer.
-- +goose StatementBegin
CREATE FUNCTION app.get_user_auth_by_email(p_email text)
    RETURNS TABLE (id uuid, password_hash text)
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = app
AS $fn$
    SELECT u.id, u.password_hash
    FROM app.users AS u
    WHERE u.email = lower(btrim(p_email));
$fn$;

REVOKE EXECUTE ON FUNCTION app.get_user_auth_by_email(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.get_user_auth_by_email(text) TO qp_app;
-- +goose StatementEnd

-- =========================================================================
-- +goose Down
-- =========================================================================
-- +goose StatementBegin
DROP FUNCTION app.get_user_auth_by_email(text);
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE app.users DROP COLUMN password_hash;
-- +goose StatementEnd
