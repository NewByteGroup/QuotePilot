-- platform-functions.sql — asserts the post-migration function-privilege
-- surface: the qp_admin/qp_app EXECUTE split on the app.* functions. It is a
-- separate file from roles.sql because CI runs it only after migrations exist;
-- roles.sql alone gates the pre-migration (init-phase) shape.
--
-- Every hard-signature reference below is gated on to_regprocedure, so running
-- it before migrations exist is a harmless pass (vacuous-true — the same
-- tolerance roles.sql [M] shows on empty catalogs), never an error.

-- (N) The function-path split (spec 2026-11-10-data-model-v1.3 section 5,
-- spec 2026-11-10-tenant-resolution-api sections 3-5, ADR-005):
-- qp_admin — and only qp_admin — EXECUTEs the platform_* SECURITY DEFINER
-- functions (Layer C writes and tenant teardown). qp_app keeps the
-- tenant-facing functions — the original three plus the 0003 auth/session
-- family — and must not reach the platform five: an EXECUTE grant to qp_app
-- left a single in-function scope check between the exposed API role and
-- tenant deletion (Devin Review). qp_admin's whole surface is schema-app
-- USAGE plus these EXECUTEs — no relation or column privileges, no default
-- privileges in app/river, and no grant on the tenant family — so a grant to
-- it anywhere else means the minimal-surface contract drifted. qp_app's own
-- EXECUTE set in app is pinned to the tenant allowlist, and the Layer A
-- tables stay at zero table privileges for it.
DO $$
DECLARE
  bad text;
  eff constant text := $q$
    SELECT n.nspname AS nsp, d.defaclobjtype::text AS objtype, a.grantee, a.privilege_type AS priv, a.is_grantable
    FROM pg_default_acl d
    JOIN pg_namespace n ON n.nspname IN ('app','river') AND d.defaclnamespace IN (0, n.oid)
    CROSS JOIN LATERAL aclexplode(d.defaclacl) AS a
    WHERE d.defaclrole = (SELECT oid FROM pg_roles WHERE rolname = 'qp_migrator')
      AND a.grantee <> d.defaclrole $q$;
BEGIN
  -- qp_admin must EXECUTE exactly the platform five — none missing (skipped
  -- entirely when a signature is absent pre-migration).
  SELECT string_agg(f.fn, ', ') INTO bad
  FROM (VALUES
    ('app.platform_insert_public_snapshot(text, text, text, text, text, jsonb, numeric, jsonb, timestamptz)'),
    ('app.platform_update_connector_status(text, app.connector_status, numeric)'),
    ('app.platform_insert_healing_session(text, uuid, text, text, text, text)'),
    ('app.platform_update_healing_session(uuid, app.healing_status, text, boolean, text)'),
    ('app.platform_delete_organization(uuid)')
  ) AS f(fn)
  WHERE to_regprocedure(f.fn) IS NOT NULL
    AND NOT has_function_privilege('qp_admin', to_regprocedure(f.fn), 'EXECUTE');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[N] qp_admin must EXECUTE every platform_* function; missing: %', bad;
  END IF;

  -- ...and nothing beyond them in app or river (a tenant-facing function
  -- granted to qp_admin, a river helper, or a new unlisted platform entry
  -- point, is drift). The exclusion list resolves through to_regprocedure so
  -- absent signatures contribute no oid instead of an error.
  SELECT string_agg(p.oid::regprocedure::text, ', ') INTO bad
  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
  WHERE n.nspname IN ('app','river') AND has_function_privilege('qp_admin', p.oid, 'EXECUTE')
    AND p.oid <> ALL (ARRAY(
      SELECT to_regprocedure(f.fn) FROM (VALUES
        ('app.platform_insert_public_snapshot(text, text, text, text, text, jsonb, numeric, jsonb, timestamptz)'),
        ('app.platform_update_connector_status(text, app.connector_status, numeric)'),
        ('app.platform_insert_healing_session(text, uuid, text, text, text, text)'),
        ('app.platform_update_healing_session(uuid, app.healing_status, text, boolean, text)'),
        ('app.platform_delete_organization(uuid)')
      ) AS f(fn) WHERE to_regprocedure(f.fn) IS NOT NULL));
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[N] qp_admin EXECUTE surface must equal the platform five; extra: %', bad;
  END IF;

  -- qp_app must not EXECUTE any of the platform five: the tenant credential
  -- physically cannot reach teardown or Layer C writes.
  SELECT string_agg(f.fn, ', ') INTO bad
  FROM (VALUES
    ('app.platform_insert_public_snapshot(text, text, text, text, text, jsonb, numeric, jsonb, timestamptz)'),
    ('app.platform_update_connector_status(text, app.connector_status, numeric)'),
    ('app.platform_insert_healing_session(text, uuid, text, text, text, text)'),
    ('app.platform_update_healing_session(uuid, app.healing_status, text, boolean, text)'),
    ('app.platform_delete_organization(uuid)')
  ) AS f(fn)
  WHERE to_regprocedure(f.fn) IS NOT NULL
    AND has_function_privilege('qp_app', to_regprocedure(f.fn), 'EXECUTE');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[N] qp_app must not EXECUTE platform_* functions; offending: %', bad;
  END IF;

  -- No relation or column ACL entries for qp_admin in app/river (any priv type,
  -- grant option included): it executes functions, it never touches tables.
  SELECT string_agg(DISTINCT o.what, ', ') INTO bad FROM (
    SELECT a.privilege_type || ' on ' || n.nspname || '.' || c.relname AS what
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    CROSS JOIN LATERAL aclexplode(c.relacl) AS a
    WHERE n.nspname IN ('app','river') AND c.relkind IN ('r','p','v','m','S','f')
      AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = 'qp_admin')
    UNION ALL
    SELECT a.privilege_type || ' on column ' || n.nspname || '.' || c.relname || '.' || t.attname
    FROM pg_attribute t JOIN pg_class c ON c.oid = t.attrelid
    JOIN pg_namespace n ON n.oid = c.relnamespace
    CROSS JOIN LATERAL aclexplode(t.attacl) AS a
    WHERE n.nspname IN ('app','river') AND c.relkind IN ('r','p','v','m','S','f')
      AND t.attnum > 0 AND NOT t.attisdropped
      AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = 'qp_admin')
  ) AS o;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[N] qp_admin must hold no relation or column privileges in app/river; offending: %', bad;
  END IF;

  -- No effective default privileges for qp_admin either: every object it needs
  -- arrives as an explicit GRANT in the migration that creates it.
  EXECUTE 'WITH eff AS (' || eff || $s$)
    SELECT string_agg(v.nsp || ' type ' || v.objtype || ' grants ' || v.priv || ' (grantable=' || v.is_grantable || ')', ', ')
    FROM eff v WHERE v.grantee = (SELECT oid FROM pg_roles WHERE rolname = 'qp_admin') $s$ INTO bad;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[N] qp_admin must receive no default privileges in app/river; offending: %', bad;
  END IF;

  -- The tenant-facing side of the split (migration 0003, tenant-resolution
  -- spec section 5): qp_app must EXECUTE every tenant function — the original
  -- three plus the auth/session family (skipped per-signature when absent
  -- pre-migration, same to_regprocedure gating).
  SELECT string_agg(f.fn, ', ') INTO bad
  FROM (VALUES
    ('app.resolve_inbound_org(text)'),
    ('app.list_memberships_for_email(text)'),
    ('app.get_current_organization()'),
    ('app.find_user_by_email(text)'),
    ('app.create_login_token(uuid, text, timestamptz)'),
    ('app.consume_login_token(text)'),
    ('app.create_session(uuid, text, timestamptz)'),
    ('app.rotate_session(text, text, timestamptz)'),
    ('app.revoke_session(uuid)'),
    ('app.revoke_session_family_by_hash(text)'),
    ('app.get_membership(uuid, uuid)'),
    ('app.get_user_profile(uuid)')
  ) AS f(fn)
  WHERE to_regprocedure(f.fn) IS NOT NULL
    AND NOT has_function_privilege('qp_app', to_regprocedure(f.fn), 'EXECUTE');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[N] qp_app must EXECUTE every tenant-facing function; missing: %', bad;
  END IF;

  -- ...and nothing beyond them in schema app (river is out of scope: init
  -- grants qp_app EXECUTE on river functions by default privilege). A new
  -- unlisted tenant entry point granted to qp_app is drift, the same way an
  -- unlisted platform entry point is for qp_admin.
  SELECT string_agg(p.oid::regprocedure::text, ', ') INTO bad
  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
  WHERE n.nspname = 'app' AND has_function_privilege('qp_app', p.oid, 'EXECUTE')
    AND p.oid <> ALL (ARRAY(
      SELECT to_regprocedure(f.fn) FROM (VALUES
        ('app.resolve_inbound_org(text)'),
        ('app.list_memberships_for_email(text)'),
        ('app.get_current_organization()'),
        ('app.find_user_by_email(text)'),
        ('app.create_login_token(uuid, text, timestamptz)'),
        ('app.consume_login_token(text)'),
        ('app.create_session(uuid, text, timestamptz)'),
        ('app.rotate_session(text, text, timestamptz)'),
        ('app.revoke_session(uuid)'),
        ('app.revoke_session_family_by_hash(text)'),
        ('app.get_membership(uuid, uuid)'),
        ('app.get_user_profile(uuid)')
      ) AS f(fn) WHERE to_regprocedure(f.fn) IS NOT NULL));
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[N] qp_app EXECUTE surface in app must equal the tenant-function allowlist; extra: %', bad;
  END IF;

  -- Layer A tables are function-gated for qp_app: no table privilege at all
  -- (the Go lint's layerATables is the same list; to_regclass keeps the check
  -- vacuous for tables a migration has not created yet).
  SELECT string_agg(t.tbl || ' ' || p.priv, ', ') INTO bad
  FROM (VALUES
    ('app.organizations'),
    ('app.users'),
    ('app.organization_memberships'),
    ('app.auth_login_tokens'),
    ('app.auth_sessions')
  ) AS t(tbl)
  CROSS JOIN (VALUES ('SELECT'),('INSERT'),('UPDATE'),('DELETE')) AS p(priv)
  WHERE to_regclass(t.tbl) IS NOT NULL
    AND has_table_privilege('qp_app', to_regclass(t.tbl), p.priv);
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[N] qp_app must hold no table privilege on Layer A tables; offending: %', bad;
  END IF;
END $$;

SELECT 'platform-functions.sql: all assertions passed' AS result;
