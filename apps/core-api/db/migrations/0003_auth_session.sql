-- Auth/session schema (docs/specs/2026-11-10-tenant-resolution-api.md sections
-- 3 and 5; ADR-005 sections 1-4): magic-link login tokens and rotating refresh
-- sessions behind the Layer A function family.
--
-- Applied by qp_migrator (object owner). Both tables are Layer A: they are read
-- before a tenant context exists, so they carry no organization_id and no RLS —
-- REVOKE ALL undoes the migrator default grant and qp_app reaches them only
-- through the SECURITY DEFINER functions below. The grant split is the inverse
-- of the platform_* path: every function here runs on a request path where the
-- only credential is qp_app, so qp_admin receives nothing (platform-functions.sql
-- assertion [N] fails CI if a qp_admin grant ever appears).
--
-- Only token hashes are stored (spec section 4): the 32-byte token itself lives
-- in the email link or the cookie; Postgres keeps its SHA-256 hex.

-- +goose Up

-- =========================================================================
-- 1. Layer A auth tables (spec section 3.2). No RLS: they are read before a
-- tenant context exists. REVOKE ALL undoes the migrator default grant; qp_app
-- reaches them only through the functions below.
-- =========================================================================
-- +goose StatementBegin
CREATE TABLE app.auth_login_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES app.users (id),   -- NO ACTION: audit preservation, data-model spec §9
    token_hash  text NOT NULL UNIQUE,                      -- SHA-256 hex of the 32-byte token
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,                               -- NULL until the single use
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE app.auth_sessions (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            uuid NOT NULL REFERENCES app.users (id),
    family_id          uuid NOT NULL,                      -- one family per login; rotation descends
    refresh_token_hash text NOT NULL UNIQUE,               -- SHA-256 hex of the current generation
    expires_at         timestamptz NOT NULL,               -- sliding 30 days, reset on each rotation
    rotated_at         timestamptz,                        -- set when this generation was exchanged
    revoked_at         timestamptz,                        -- logout or family kill on reuse
    created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX auth_sessions_family ON app.auth_sessions (family_id);

REVOKE ALL ON app.auth_login_tokens, app.auth_sessions FROM qp_app;
-- +goose StatementEnd

-- =========================================================================
-- 2. The auth/session function family (spec section 5): all SECURITY DEFINER
-- owned by qp_migrator with a pinned search_path, all granted to qp_app and
-- only qp_app. LANGUAGE sql where a single statement carries the contract;
-- plpgsql where the contract needs FOR UPDATE, FOUND or several statements.
-- =========================================================================

-- find_user_by_email: the only email -> id lookup — there is no self-signup
-- and users are seeded by qp_migrator, so this must stay the sole path (spec
-- section 3.2). lower(btrim()) mirrors the users.email CHECK, which stores
-- lowercase only; an unknown email is NULL, indistinguishable from no answer.
-- +goose StatementBegin
CREATE FUNCTION app.find_user_by_email(p_email text) RETURNS uuid
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = app
AS $fn$
    SELECT u.id
    FROM app.users AS u
    WHERE u.email = lower(btrim(p_email));
$fn$;

REVOKE EXECUTE ON FUNCTION app.find_user_by_email(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.find_user_by_email(text) TO qp_app;
-- +goose StatementEnd

-- create_login_token: insert only — the caller computed the SHA-256 hash and
-- the 15-minute expiry (spec section 5). An unknown user_id fails the FK; the
-- row never stores the token itself.
-- +goose StatementBegin
CREATE FUNCTION app.create_login_token(p_user_id uuid, p_token_hash text, p_expires_at timestamptz)
    RETURNS uuid
    LANGUAGE sql
    SECURITY DEFINER
    SET search_path = app
AS $fn$
    INSERT INTO app.auth_login_tokens AS t (user_id, token_hash, expires_at)
    VALUES (p_user_id, p_token_hash, p_expires_at)
    RETURNING t.id;
$fn$;

REVOKE EXECUTE ON FUNCTION app.create_login_token(uuid, text, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.create_login_token(uuid, text, timestamptz) TO qp_app;
-- +goose StatementEnd

-- consume_login_token: the single-use guarantee is the row lock of one atomic
-- UPDATE, not a read-then-write (ADR-005 section 2). NULL covers unknown,
-- consumed and expired hashes indistinguishably — the endpoint cannot oracle
-- which case it was.
-- +goose StatementBegin
CREATE FUNCTION app.consume_login_token(p_token_hash text) RETURNS uuid
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

REVOKE EXECUTE ON FUNCTION app.consume_login_token(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.consume_login_token(text) TO qp_app;
-- +goose StatementEnd

-- create_session: generation 0 roots the family at its own id (spec section
-- 5). The id is generated in the SELECT so family_id = id lands in one
-- statement — no update-after-insert window.
-- +goose StatementBegin
CREATE FUNCTION app.create_session(p_user_id uuid, p_refresh_token_hash text, p_expires_at timestamptz)
    RETURNS uuid
    LANGUAGE sql
    SECURITY DEFINER
    SET search_path = app
AS $fn$
    INSERT INTO app.auth_sessions AS s (id, user_id, family_id, refresh_token_hash, expires_at)
    SELECT g.id, p_user_id, g.id, p_refresh_token_hash, p_expires_at
    FROM (SELECT gen_random_uuid() AS id) AS g
    RETURNING s.id;
$fn$;

REVOKE EXECUTE ON FUNCTION app.create_session(uuid, text, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.create_session(uuid, text, timestamptz) TO qp_app;
-- +goose StatementEnd

-- rotate_session: the whole exchange is one call, and every mutation of a
-- family serializes on a per-family advisory lock (pg_advisory_xact_lock
-- keyed by family_id) so a concurrent rotation cannot commit a successor
-- inside a revoker's statement window and let it escape the family kill
-- (Devin Review). Lock order everywhere is family-advisory THEN row FOR
-- UPDATE — taking the advisory after a row lock would deadlock against a
-- revoker doing family-first.
-- Exactly one outcome per call: empty set for unknown, revoked or expired
-- hashes; rotated=false with NULL ids when the presented generation was
-- already exchanged (reuse — the family is killed in the same call so a
-- stolen token cannot outlive it); rotated=true with the successor's id and
-- the user. plpgsql because the contract needs FOR UPDATE plus conditional
-- statements.
-- +goose StatementBegin
CREATE FUNCTION app.rotate_session(p_presented_hash text, p_new_hash text, p_new_expires_at timestamptz)
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

-- revoke_session: single-row revoke — the JWT's sid is the family's live
-- generation, so one row is enough (spec section 5). Retained for future
-- per-session admin operations; it is NOT the logout interface — logout holds
-- an opaque refresh token, not a session id (revoke_session_family_by_hash
-- below).
-- No family advisory here: one named row races a concurrent rotate
-- consistently under EPQ — either the rotate committed first (this revoke
-- stamps an already-rotated row, still id-correct) or it re-reads the row
-- revoked and returns the empty set.
-- +goose StatementBegin
CREATE FUNCTION app.revoke_session(p_session_id uuid) RETURNS boolean
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = app
AS $fn$
BEGIN
    UPDATE app.auth_sessions AS s
    SET revoked_at = now()
    WHERE s.id = p_session_id
      AND s.revoked_at IS NULL;
    RETURN FOUND;
END;
$fn$;

REVOKE EXECUTE ON FUNCTION app.revoke_session(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.revoke_session(uuid) TO qp_app;
-- +goose StatementEnd

-- revoke_session_family_by_hash: logout's revocation interface (spec sections
-- 5 and 8): the opaque refresh cookie resolves to its whole family in one
-- call, so every generation dies — live and already-rotated alike. The
-- boolean feeds caller metrics only; the endpoint always answers 204, so a
-- false (unknown hash or already-dead family) is not an oracle.
-- The family advisory lock — same key and same order as rotate_session — is
-- why a successor can never be born inside the kill window: a concurrent
-- rotate waits here until this UPDATE commits, then re-reads its row
-- revoked and returns the empty set (Devin Review).
-- +goose StatementBegin
CREATE FUNCTION app.revoke_session_family_by_hash(p_refresh_token_hash text) RETURNS boolean
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = app
AS $fn$
DECLARE
    v_family uuid;
BEGIN
    SELECT s.family_id INTO v_family
    FROM app.auth_sessions AS s
    WHERE s.refresh_token_hash = p_refresh_token_hash;

    IF NOT FOUND THEN
        RETURN false;
    END IF;

    PERFORM pg_advisory_xact_lock(hashtextextended(v_family::text, 0));

    UPDATE app.auth_sessions AS s
    SET revoked_at = now()
    WHERE s.family_id = v_family
      AND s.revoked_at IS NULL;
    RETURN FOUND;
END;
$fn$;

REVOKE EXECUTE ON FUNCTION app.revoke_session_family_by_hash(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.revoke_session_family_by_hash(text) TO qp_app;
-- +goose StatementEnd

-- get_membership: the per-request tenant check (ADR-005 section 5). NULL is
-- the security property, not an error case: non-member and nonexistent
-- organization are indistinguishable, so the endpoint cannot oracle org
-- existence.
-- +goose StatementBegin
CREATE FUNCTION app.get_membership(p_user_id uuid, p_organization_id uuid)
    RETURNS app.membership_role
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = app
AS $fn$
    SELECT m.role
    FROM app.organization_memberships AS m
    WHERE m.user_id = p_user_id
      AND m.organization_id = p_organization_id;
$fn$;

REVOKE EXECUTE ON FUNCTION app.get_membership(uuid, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.get_membership(uuid, uuid) TO qp_app;
-- +goose StatementEnd

-- get_user_profile: GET /me's identity lookup (spec section 5); minimal
-- columns under the same argument-trust model as get_membership — unknown id
-- is an empty set, not an error.
-- +goose StatementBegin
CREATE FUNCTION app.get_user_profile(p_user_id uuid)
    RETURNS TABLE (id uuid, email text, full_name text)
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = app
AS $fn$
    SELECT u.id, u.email, u.full_name
    FROM app.users AS u
    WHERE u.id = p_user_id;
$fn$;

REVOKE EXECUTE ON FUNCTION app.get_user_profile(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.get_user_profile(uuid) TO qp_app;
-- +goose StatementEnd

-- =========================================================================
-- 3. list_memberships_for_email extended to the org-switcher shape (spec
-- section 3.3 — DECIDED 2026-11-10): name and slug join the membership row so
-- the picker renders in one lookup. DROP + recreate because CREATE OR REPLACE
-- cannot change a return type; the grant is re-issued with the recreate.
-- =========================================================================
-- +goose StatementBegin
DROP FUNCTION app.list_memberships_for_email(text);

-- p_email, not email: in a LANGUAGE sql function a column name wins over a
-- parameter name, so an "email" parameter would bind to u.email and the
-- predicate would collapse to a tautology, leaking every membership row.
CREATE FUNCTION app.list_memberships_for_email(p_email text)
    RETURNS TABLE (organization_id uuid, name text, slug text, role app.membership_role)
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = app
AS $fn$
    SELECT m.organization_id, o.name, o.slug, m.role
    FROM app.organization_memberships AS m
    JOIN app.users AS u ON u.id = m.user_id
    JOIN app.organizations AS o ON o.id = m.organization_id
    WHERE lower(u.email) = lower(btrim(p_email));
$fn$;

REVOKE EXECUTE ON FUNCTION app.list_memberships_for_email(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.list_memberships_for_email(text) TO qp_app;
-- +goose StatementEnd

-- =========================================================================
-- +goose Down
-- Reverse order: functions first (their bodies depend on the auth tables),
-- then the tables, then the original two-column list_memberships_for_email
-- restored exactly as 0002 created it.
-- =========================================================================
-- +goose StatementBegin
DROP FUNCTION app.get_user_profile(uuid);
DROP FUNCTION app.get_membership(uuid, uuid);
DROP FUNCTION app.revoke_session_family_by_hash(text);
DROP FUNCTION app.revoke_session(uuid);
DROP FUNCTION app.rotate_session(text, text, timestamptz);
DROP FUNCTION app.create_session(uuid, text, timestamptz);
DROP FUNCTION app.consume_login_token(text);
DROP FUNCTION app.create_login_token(uuid, text, timestamptz);
DROP FUNCTION app.find_user_by_email(text);
DROP FUNCTION app.list_memberships_for_email(text);
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE app.auth_sessions;
DROP TABLE app.auth_login_tokens;
-- +goose StatementEnd

-- Restored exactly as 0002 created it, grant included.
-- +goose StatementBegin
-- p_email, not email: in a LANGUAGE sql function a column name wins over a
-- parameter name, so an "email" parameter would bind to u.email and the
-- predicate would collapse to a tautology, leaking every membership row.
CREATE FUNCTION app.list_memberships_for_email(p_email text)
    RETURNS TABLE (organization_id uuid, role app.membership_role)
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = app
AS $fn$
    SELECT m.organization_id, m.role
    FROM app.organization_memberships AS m
    JOIN app.users AS u ON u.id = m.user_id
    WHERE lower(u.email) = lower(btrim(p_email));
$fn$;

REVOKE EXECUTE ON FUNCTION app.list_memberships_for_email(text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.list_memberships_for_email(text) TO qp_app;
-- +goose StatementEnd
