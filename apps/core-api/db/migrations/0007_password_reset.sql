-- Password reset on the shipped magic-link machinery (ADR-005 amendment
-- 2026-11-16): "forgot password" reuses the platform outbox + token path —
-- same hash-at-rest, single-use, short expiry — and the link lands on a
-- set-password page rather than starting a session directly.
-- app.auth_login_tokens gains a purpose discriminator so the reset flow
-- shares the table: the auth_mail_outbox FK, the delete-on-send retention
-- and the expiry logic all apply verbatim to reset rows. The two purposes
-- are deliberately NOT interchangeable — the consume functions filter on
-- purpose, so a reset link can never mint a session and a magic link can
-- never set a password. The reset also adds a per-user session barrier:
-- pg_advisory_xact_lock keyed on user_id, held by revoke_user_sessions and
-- by every session issuer (rotate_session below; the exchange and
-- password-login paths take it in Go before minting) — so a concurrent
-- issuer either commits before the revoke's scan or waits for the reset
-- commit and re-validates (Devin Review: concurrent sessions surviving
-- password resets).
--
-- Applied by qp_migrator (object owner). Every function below takes the
-- ADR-005 section 36 posture that migration 0006 established for the
-- credential lookup — SECURITY DEFINER with
-- search_path = pg_catalog, pg_temp and fully schema-qualified names —
-- instead of the 0003 auth family's older SET search_path = app.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE app.auth_login_tokens
    ADD COLUMN purpose text NOT NULL DEFAULT 'login'
    CHECK (purpose IN ('login', 'password_reset'));

COMMENT ON COLUMN app.auth_login_tokens.purpose IS
    'Credential class: ''login'' tokens consume at POST /auth/session for a session, ''password_reset'' tokens consume at POST /auth/password for a credential write. The consumes filter on it so the two classes can never exchange (ADR-005 amendment 2026-11-16).';
-- +goose StatementEnd

-- consume_login_token gains the purpose predicate: the token that mints a
-- session must be a LOGIN token. Without this filter a password_reset
-- token presented to /auth/session would consume as a magic link and mint
-- a session — and symmetrically a stolen login link could set a password,
-- a persistent takeover out of a 15-minute credential. CREATE OR REPLACE
-- keeps the signature and the grant; the body is 0003's verbatim apart
-- from the predicate and the section 36 search_path upgrade — the
-- single-use atomic-UPDATE row lock is unchanged.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.consume_login_token(p_token_hash text) RETURNS uuid
    LANGUAGE sql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
    UPDATE app.auth_login_tokens AS t
    SET consumed_at = now()
    WHERE t.token_hash = p_token_hash
      AND t.purpose = 'login'
      AND t.consumed_at IS NULL
      AND t.expires_at > now()
    RETURNING t.user_id;
$fn$;
-- +goose StatementEnd

-- create_password_reset_token is create_login_token's reset twin: the
-- caller computed the SHA-256 hash and the LoginTTL expiry, the row
-- records purpose='password_reset' so it can never route to /auth/session.
-- +goose StatementBegin
CREATE FUNCTION app.create_password_reset_token(p_user_id uuid, p_token_hash text, p_expires_at timestamptz)
    RETURNS uuid
    LANGUAGE sql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
    INSERT INTO app.auth_login_tokens AS t (user_id, token_hash, expires_at, purpose)
    VALUES (p_user_id, p_token_hash, p_expires_at, 'password_reset')
    RETURNING t.id;
$fn$;

REVOKE EXECUTE ON FUNCTION app.create_password_reset_token(uuid, text, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.create_password_reset_token(uuid, text, timestamptz) TO qp_app;
-- +goose StatementEnd

-- consume_password_reset_token is consume_login_token's reset twin: the
-- same atomic UPDATE row lock is the single-use guarantee and NULL covers
-- unknown, consumed, expired — and now wrong-purpose — hashes
-- indistinguishably. A magic-link token presented here is a miss, so a
-- login link can never set a password.
-- +goose StatementBegin
CREATE FUNCTION app.consume_password_reset_token(p_token_hash text) RETURNS uuid
    LANGUAGE sql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
    UPDATE app.auth_login_tokens AS t
    SET consumed_at = now()
    WHERE t.token_hash = p_token_hash
      AND t.purpose = 'password_reset'
      AND t.consumed_at IS NULL
      AND t.expires_at > now()
    RETURNING t.user_id;
$fn$;

REVOKE EXECUTE ON FUNCTION app.consume_password_reset_token(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.consume_password_reset_token(text) TO qp_app;
-- +goose StatementEnd

-- set_user_password: the set-password transaction's credential write — the
-- only qp_app-side path that ever touches users.password_hash. A NULL or
-- blank hash is refused the way create_mail_outbox refuses an empty link:
-- committing an empty credential would silently lock the account, so a bug
-- upstream fails loud inside the transaction rather than committing an
-- unusable password.
-- +goose StatementBegin
CREATE FUNCTION app.set_user_password(p_user_id uuid, p_password_hash text) RETURNS void
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    IF p_password_hash IS NULL OR btrim(p_password_hash) = '' THEN
        RAISE EXCEPTION 'set_user_password: password_hash must be a non-empty string';
    END IF;

    UPDATE app.users AS u
    SET password_hash = p_password_hash
    WHERE u.id = p_user_id;
END;
$fn$;

REVOKE EXECUTE ON FUNCTION app.set_user_password(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.set_user_password(uuid, text) TO qp_app;
-- +goose StatementEnd

-- revoke_user_sessions runs in the same set-password transaction (OWASP):
-- a credential reset invalidates every live session of the user, so a
-- session minted on a stolen password cannot outlive the credential.
-- Already-revoked rows are left alone.
--
-- The UPDATE alone cannot see a session another transaction mints
-- concurrently — a racing password login, magic-link exchange or refresh
-- rotation could commit a new session after the revoke's scan and keep
-- renewable access post-recovery (Devin Review). pg_advisory_xact_lock
-- keyed on user_id is the barrier: every session issuer takes the same
-- lock before writing, so an issuer is either already committed — hence
-- inside the UPDATE's candidate set — or blocked on this lock until the
-- reset commits. LOCK ORDER IS THE CONTRACT everywhere: user → family →
-- row, matching rotate_session below — a path that took family or row
-- locks before the user lock would deadlock against this one.
-- +goose StatementBegin
CREATE FUNCTION app.revoke_user_sessions(p_user_id uuid) RETURNS void
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    -- Held to transaction end (xact scope): the caller's issue-vs-revoke
    -- critical section covers the whole set-password commit, not just
    -- this statement.
    PERFORM pg_advisory_xact_lock(hashtextextended(p_user_id::text, 0));
    UPDATE app.auth_sessions AS s
    SET revoked_at = now()
    WHERE s.user_id = p_user_id
      AND s.revoked_at IS NULL;
END;
$fn$;

REVOKE EXECUTE ON FUNCTION app.revoke_user_sessions(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.revoke_user_sessions(uuid) TO qp_app;
-- +goose StatementEnd

-- rotate_session gains the same user lock the reset path holds — a
-- refresh rotation is a session issuer too, and without it a rotation
-- could commit a successor after revoke_user_sessions' scan, letting a
-- session on the pre-reset credential outlive the reset (Devin Review).
-- LOCK ORDER IS THE CONTRACT: user → family → row, in that order on every
-- path. The reset path takes ONLY the user lock, so taking the family or
-- row lock before the user lock here would deadlock against a concurrent
-- reset; taking them after cannot. CREATE OR REPLACE keeps the signature
-- and the grant; the body is 0003's verbatim after the resolution select
-- now also reads user_id and the two locks run before the FOR UPDATE row
-- read — the single-use atomic machinery is unchanged. The function also
-- moves to the section 36 search_path the rest of this migration uses.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.rotate_session(p_presented_hash text, p_new_hash text, p_new_expires_at timestamptz)
    RETURNS TABLE (session_id uuid, user_id uuid, rotated boolean)
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_family uuid;
    v_user uuid;
    v_session app.auth_sessions%ROWTYPE;
    v_new_id uuid;
BEGIN
    -- Resolve family and user with a plain read first: rows are never
    -- deleted, so the ids stay valid, and the locked read below is what
    -- must observe the post-serialization state.
    SELECT s.family_id, s.user_id INTO v_family, v_user
    FROM app.auth_sessions AS s
    WHERE s.refresh_token_hash = p_presented_hash;

    IF NOT FOUND THEN
        RETURN;
    END IF;

    -- LOCK ORDER: user → family → row. The user lock serializes this
    -- rotation against revoke_user_sessions' reset barrier; the family
    -- lock serializes against every other mutation of this family (the
    -- reuse kill below and revoke_session_family_by_hash). A racing
    -- mutation is either committed — hence visible to the FOR UPDATE
    -- read — or still waiting here and will see this call's committed
    -- result.
    PERFORM pg_advisory_xact_lock(hashtextextended(v_user::text, 0));
    PERFORM pg_advisory_xact_lock(hashtextextended(v_family::text, 0));

    SELECT s.* INTO v_session
    FROM app.auth_sessions AS s
    WHERE s.refresh_token_hash = p_presented_hash
    FOR UPDATE;

    -- Unknown, revoked and expired are one shape: no row back, nothing
    -- revealed about which case it was. NOT FOUND only covers a delete
    -- mid-resolution; nothing deletes today but the check costs nothing.
    IF NOT FOUND
       OR v_session.revoked_at IS NOT NULL
       OR v_session.expires_at <= now() THEN
        RETURN;
    END IF;

    -- Reuse: presenting an already-exchanged generation means the token was
    -- stolen or replayed, so the whole family dies in the same atomic call.
    -- The advisory locks make this UPDATE airtight: a racing successor is
    -- either still blocked on the advisory (unborn) or already committed
    -- and inside this statement's candidate set. NULL ids keep the answer
    -- as uniform as the empty set.
    IF v_session.rotated_at IS NOT NULL THEN
        UPDATE app.auth_sessions AS s
        SET revoked_at = now()
        WHERE s.family_id = v_session.family_id;
        RETURN QUERY SELECT NULL::uuid, NULL::uuid, false;
        RETURN;
    END IF;

    UPDATE app.auth_sessions AS s
    SET rotated_at = now()
    WHERE s.id = v_session.id;

    INSERT INTO app.auth_sessions AS s (user_id, family_id, refresh_token_hash, expires_at)
    VALUES (v_session.user_id, v_session.family_id, p_new_hash, p_new_expires_at)
    RETURNING s.id INTO v_new_id;

    RETURN QUERY SELECT v_new_id, v_session.user_id, true;
END;
$fn$;
-- +goose StatementEnd

-- revoke_user_pending_tokens: the reset transaction's token kill — every
-- unconsumed auth_login_tokens row of the user dies with the credential
-- reset, whatever its purpose. Outstanding magic links and reset links
-- are the same credential class as the password: leaving them alive
-- would let a pre-reset link mint a session on the recovered account.
-- The caller holds the user advisory lock already, so no additional
-- serialization lives here.
-- +goose StatementBegin
CREATE FUNCTION app.revoke_user_pending_tokens(p_user_id uuid) RETURNS void
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
BEGIN
    UPDATE app.auth_login_tokens AS t
    SET consumed_at = now()
    WHERE t.user_id = p_user_id
      AND t.consumed_at IS NULL;
END;
$fn$;

REVOKE EXECUTE ON FUNCTION app.revoke_user_pending_tokens(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.revoke_user_pending_tokens(uuid) TO qp_app;
-- +goose StatementEnd

-- read_mail_outbox gains the token's purpose: the auth_mail_send worker
-- picks the mail template from it — a password_reset row mails the
-- set-password link, a login row the magic link. DROP + recreate because
-- CREATE OR REPLACE cannot change a return type; the grant is re-issued
-- with the recreate (0003's list_memberships_for_email precedent).
-- +goose StatementBegin
DROP FUNCTION app.read_mail_outbox(uuid);

CREATE FUNCTION app.read_mail_outbox(p_id uuid)
    RETURNS TABLE(link_url text, recipient_email text, expires_at timestamptz, purpose text)
    LANGUAGE sql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
    SELECT m.link_url, u.email, t.expires_at, t.purpose
    FROM app.auth_mail_outbox AS m
    JOIN app.auth_login_tokens AS t ON t.id = m.login_token_id
    JOIN app.users AS u ON u.id = t.user_id
    WHERE m.id = p_id
      AND t.consumed_at IS NULL;
$fn$;

REVOKE EXECUTE ON FUNCTION app.read_mail_outbox(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.read_mail_outbox(uuid) TO qp_app;
-- +goose StatementEnd

-- =========================================================================
-- +goose Down
-- Reverse order: drop the new functions, restore read_mail_outbox (0004),
-- consume_login_token and rotate_session (both 0003) to their original
-- bodies verbatim — purpose filter and user lock gone, original
-- SET search_path = app — then drop the column last so the restored
-- functions still resolve.
-- =========================================================================
-- +goose StatementBegin
DROP FUNCTION app.revoke_user_pending_tokens(uuid);
DROP FUNCTION app.revoke_user_sessions(uuid);
DROP FUNCTION app.set_user_password(uuid, text);
DROP FUNCTION app.consume_password_reset_token(text);
DROP FUNCTION app.create_password_reset_token(uuid, text, timestamptz);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.rotate_session(p_presented_hash text, p_new_hash text, p_new_expires_at timestamptz)
    RETURNS TABLE (session_id uuid, user_id uuid, rotated boolean)
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = app
AS $fn$
DECLARE
    v_family uuid;
    v_session app.auth_sessions%ROWTYPE;
    v_new_id uuid;
BEGIN
    -- Resolve the family with a plain read first: rows are never deleted,
    -- so the id stays valid, and the locked read below is what must observe
    -- the post-serialization state.
    SELECT s.family_id INTO v_family
    FROM app.auth_sessions AS s
    WHERE s.refresh_token_hash = p_presented_hash;

    IF NOT FOUND THEN
        RETURN;
    END IF;

    -- Serializes against every other mutation of this family (the reuse
    -- kill below and revoke_session_family_by_hash): a racing mutation is
    -- either committed — hence visible to the FOR UPDATE read — or still
    -- waiting here and will see this call's committed result.
    PERFORM pg_advisory_xact_lock(hashtextextended(v_family::text, 0));

    SELECT s.* INTO v_session
    FROM app.auth_sessions AS s
    WHERE s.refresh_token_hash = p_presented_hash
    FOR UPDATE;

    -- Unknown, revoked and expired are one shape: no row back, nothing
    -- revealed about which case it was. NOT FOUND only covers a delete
    -- mid-resolution; nothing deletes today but the check costs nothing.
    IF NOT FOUND
       OR v_session.revoked_at IS NOT NULL
       OR v_session.expires_at <= now() THEN
        RETURN;
    END IF;

    -- Reuse: presenting an already-exchanged generation means the token was
    -- stolen or replayed, so the whole family dies in the same atomic call.
    -- The advisory lock makes this UPDATE airtight: a racing successor is
    -- either still blocked on the advisory (unborn) or already committed and
    -- inside this statement's candidate set. NULL ids keep the answer as
    -- uniform as the empty set.
    IF v_session.rotated_at IS NOT NULL THEN
        UPDATE app.auth_sessions AS s
        SET revoked_at = now()
        WHERE s.family_id = v_session.family_id;
        RETURN QUERY SELECT NULL::uuid, NULL::uuid, false;
        RETURN;
    END IF;

    UPDATE app.auth_sessions AS s
    SET rotated_at = now()
    WHERE s.id = v_session.id;

    INSERT INTO app.auth_sessions AS s (user_id, family_id, refresh_token_hash, expires_at)
    VALUES (v_session.user_id, v_session.family_id, p_new_hash, p_new_expires_at)
    RETURNING s.id INTO v_new_id;

    RETURN QUERY SELECT v_new_id, v_session.user_id, true;
END;
$fn$;

REVOKE EXECUTE ON FUNCTION app.rotate_session(text, text, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.rotate_session(text, text, timestamptz) TO qp_app;
-- +goose StatementEnd

-- +goose StatementBegin
DROP FUNCTION app.read_mail_outbox(uuid);

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

-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.consume_login_token(p_token_hash text) RETURNS uuid
    LANGUAGE sql
    SECURITY DEFINER
    SET search_path = app
AS $fn$
    UPDATE app.auth_login_tokens AS t
    SET consumed_at = now()
    WHERE t.token_hash = p_token_hash
      AND t.consumed_at IS NULL
      AND t.expires_at > now()
    RETURNING t.user_id;
$fn$;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE app.auth_login_tokens DROP COLUMN purpose;
-- +goose StatementEnd
