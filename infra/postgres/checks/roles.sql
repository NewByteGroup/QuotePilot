\set ON_ERROR_STOP on
-- Role, schema, grant and extension assertions for the local database (ADR-003).
-- Plain SQL, no secrets. Every property is a DO block that RAISEs EXCEPTION naming
-- the offenders, so psql exits non-zero (ON_ERROR_STOP) and nothing passes vacuously.
-- NULL-safety: NOT IN is avoided; IS DISTINCT FROM and coalesce are used instead.
-- Run as the bootstrap superuser (make db-check), or see infra/README.md.
-- Each mutation of this file is proven to fail by infra/postgres/checks/negative-tests.sh.
-- This file is the base for the Mission 3 isolation spike and the ADR-003 provisioning check.
-- It leaves no session state (no temp objects) and needs no TEMPORARY privilege.

-- (1) Role-existence guard: fail first if init did not run.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(x.name, ', ') INTO bad
  FROM (VALUES ('qp_app'),('qp_migrator'),('qp_admin'),('anon'),('authenticated'),('service_role')) AS x(name)
  WHERE NOT EXISTS (SELECT 1 FROM pg_roles r WHERE r.rolname = x.name);
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[1] missing roles: %; init script did not run (see infra/README.md, make db-reset)', bad;
  END IF;
END $$;

-- (2) Schemas exist; app and river are owned by qp_migrator.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(s.name || ' (' || coalesce('owner: ' || pg_get_userbyid(n.nspowner), 'missing') || ')', ', ') INTO bad
  FROM (VALUES ('app', true),('river', true),('extensions', false)) AS s(name, must_be_migrator_owned)
  LEFT JOIN pg_namespace n ON n.nspname = s.name
  WHERE n.oid IS NULL OR (s.must_be_migrator_owned AND pg_get_userbyid(n.nspowner) IS DISTINCT FROM 'qp_migrator');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[2] schemas app, river, extensions must exist; app and river owned by qp_migrator; offending: %', bad;
  END IF;
END $$;

-- (A) Role attributes.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(x.name || ' [' || concat_ws(',',
      CASE WHEN p.rolsuper THEN 'SUPERUSER' END,
      CASE WHEN p.rolbypassrls THEN 'BYPASSRLS' END,
      CASE WHEN p.rolcreatedb THEN 'CREATEDB' END,
      CASE WHEN p.rolcreaterole THEN 'CREATEROLE' END,
      CASE WHEN p.rolreplication THEN 'REPLICATION' END,
      CASE WHEN NOT p.rolcanlogin THEN 'NOLOGIN' END) || ']', ', ') INTO bad
  FROM (VALUES ('qp_app'),('qp_migrator'),('qp_admin')) AS x(name)
  JOIN pg_roles p ON p.rolname = x.name
  WHERE p.rolsuper OR p.rolbypassrls OR p.rolcreatedb OR p.rolcreaterole OR p.rolreplication OR NOT p.rolcanlogin;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[A] role attributes wrong (want LOGIN and none of SUPERUSER, BYPASSRLS, CREATEDB, CREATEROLE, REPLICATION); offending: %', bad;
  END IF;
END $$;

-- (B) Memberships, in both directions.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(pg_get_userbyid(m.roleid), ', ') INTO bad
  FROM pg_auth_members m
  WHERE m.member = (SELECT oid FROM pg_roles WHERE rolname = 'qp_app');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[B1] qp_app must be a member of no role; member of: %', bad;
  END IF;

  -- B2 mirrors B1: qp_migrator is a member of no role (no pg_* role, no BYPASSRLS/CREATEROLE
  -- role, no Supabase role). On hosted Supabase the opposite direction (postgres a member of
  -- qp_migrator) is expected and is handled by B3's allowlist note below.
  SELECT string_agg(pg_get_userbyid(m.roleid), ', ') INTO bad
  FROM pg_auth_members m
  WHERE m.member = (SELECT oid FROM pg_roles WHERE rolname = 'qp_migrator');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[B2] qp_migrator must be a member of no role; member of: %', bad;
  END IF;

  -- B4 mirrors B1/B2 for qp_admin (platform path, init section 2): memberships
  -- would let SET ROLE bridge the definer-context check the teardown flag needs.
  SELECT string_agg(pg_get_userbyid(m.roleid), ', ') INTO bad
  FROM pg_auth_members m
  WHERE m.member = (SELECT oid FROM pg_roles WHERE rolname = 'qp_admin');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[B4] qp_admin must be a member of no role; member of: %', bad;
  END IF;

  -- On hosted Supabase the platform role postgres is expected to be a member of qp_migrator;
  -- B3 must then be relaxed with an allowlist (to be tracked in ADR-007, not yet written). Strict for now.
  SELECT string_agg(pg_get_userbyid(m.member) || ' in ' || pg_get_userbyid(m.roleid), ', ') INTO bad
  FROM pg_auth_members m
  WHERE m.roleid IN (SELECT oid FROM pg_roles WHERE rolname IN ('qp_app','qp_migrator','qp_admin'));
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[B3] nobody may be a member of qp_app, qp_migrator or qp_admin; offending: %', bad;
  END IF;
END $$;

-- (C) search_path pinned per role, and no per-database override may win over it.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(x.name, ', ') INTO bad
  FROM (VALUES ('qp_app'),('qp_migrator'),('qp_admin')) AS x(name)
  WHERE NOT EXISTS (
    SELECT 1 FROM pg_db_role_setting s CROSS JOIN LATERAL unnest(s.setconfig) AS c
    WHERE s.setrole = (SELECT oid FROM pg_roles WHERE rolname = x.name)
      AND s.setdatabase IN (0, (SELECT oid FROM pg_database WHERE datname = current_database()))
      AND c = 'search_path=app, extensions');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[C1] role setting search_path=app, extensions missing for: %', bad;
  END IF;

  SELECT string_agg(pg_get_userbyid(s.setrole) || ' (' || c || ')', ', ') INTO bad
  FROM pg_db_role_setting s CROSS JOIN LATERAL unnest(s.setconfig) AS c
  WHERE s.setrole IN (SELECT oid FROM pg_roles WHERE rolname IN ('qp_app','qp_migrator','qp_admin'))
    AND s.setdatabase IN (0, (SELECT oid FROM pg_database WHERE datname = current_database()))
    AND c LIKE 'search_path=%' AND c IS DISTINCT FROM 'search_path=app, extensions';
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[C2] conflicting search_path setting (global or this database) for: %', bad;
  END IF;
END $$;

-- (D) qp_app and qp_admin own nothing in this database (any schema), read from
-- pg_shdepend (deptype 'o'). An owner bypasses FORCE RLS for its own objects.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(pg_get_userbyid(d.refobjid) || ' owns ' || pg_describe_object(d.classid, d.objid, d.objsubid), ', ') INTO bad
  FROM pg_shdepend d
  WHERE d.refclassid = 'pg_authid'::regclass
    AND d.refobjid IN (SELECT oid FROM pg_roles WHERE rolname IN ('qp_app','qp_admin'))
    AND d.deptype = 'o'
    AND d.dbid = (SELECT oid FROM pg_database WHERE datname = current_database());
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[D] qp_app and qp_admin must own no object in this database; offending: %', bad;
  END IF;
END $$;

-- (E) No CREATE on the schemas or the database for the wrong roles.
-- E1 deliberately excludes schema public: it is not on search_path and is platform-managed on hosted Supabase.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(r.name || ' CREATE on schema ' || s.name, ', ') INTO bad
  FROM (VALUES ('qp_app'),('qp_admin'),('anon'),('authenticated'),('service_role')) AS r(name)
  CROSS JOIN (VALUES ('app'),('river'),('extensions')) AS s(name)
  WHERE has_schema_privilege(r.name, s.name, 'CREATE');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[E1] no CREATE on app/river/extensions for qp_app, qp_admin, anon, authenticated, service_role; offending: %', bad;
  END IF;

  -- qp_migrator owns app and river but only uses extensions.
  IF has_schema_privilege('qp_migrator', 'extensions', 'CREATE') THEN
    RAISE EXCEPTION '[E1] qp_migrator must have USAGE only on schema extensions, found CREATE';
  END IF;

  SELECT string_agg(r.name, ', ') INTO bad
  FROM (VALUES ('qp_app'),('qp_migrator'),('qp_admin'),('anon'),('authenticated'),('service_role')) AS r(name)
  WHERE has_database_privilege(r.name, current_database(), 'CREATE');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[E2] no database-level CREATE for qp_app, qp_migrator, qp_admin, anon, authenticated, service_role; offending: %', bad;
  END IF;
END $$;

-- (F) USAGE: closed to the Supabase roles, open to qp_app and qp_migrator.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(r.name || ' USAGE on ' || s.name, ', ') INTO bad
  FROM (VALUES ('anon'),('authenticated'),('service_role')) AS r(name) CROSS JOIN (VALUES ('app'),('river')) AS s(name)
  WHERE has_schema_privilege(r.name, s.name, 'USAGE');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[F1] anon, authenticated, service_role must not have USAGE on app/river; offending: %', bad;
  END IF;

  SELECT string_agg(r.name || ' lacks USAGE on ' || s.name, ', ') INTO bad
  FROM (VALUES ('qp_app'),('qp_migrator')) AS r(name) CROSS JOIN (VALUES ('app'),('river'),('extensions')) AS s(name)
  WHERE NOT has_schema_privilege(r.name, s.name, 'USAGE');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[F2] qp_app and qp_migrator need USAGE on app, river, extensions; offending: %', bad;
  END IF;

  -- qp_admin's surface is narrower still: USAGE on app and extensions (the
  -- pgx driver resolves vector at connect time) — never river, the platform
  -- path does not touch job tables.
  SELECT string_agg('qp_admin lacks USAGE on ' || s.name, ', ') INTO bad
  FROM (VALUES ('app'),('extensions')) AS s(name)
  WHERE NOT has_schema_privilege('qp_admin', s.name, 'USAGE');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[F3] qp_admin needs USAGE on app and extensions; offending: %', bad;
  END IF;
  IF has_schema_privilege('qp_admin', 'river', 'USAGE') THEN
    RAISE EXCEPTION '[F3] qp_admin must not have USAGE on schema river';
  END IF;
END $$;

-- (G) Database privileges: CONNECT true, TEMPORARY false.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(r.name || ' (connect=' || has_database_privilege(r.name, current_database(), 'CONNECT') || ', temp=' || has_database_privilege(r.name, current_database(), 'TEMPORARY') || ')', ', ') INTO bad
  FROM (VALUES ('qp_app'),('qp_migrator'),('qp_admin')) AS r(name)
  WHERE NOT has_database_privilege(r.name, current_database(), 'CONNECT') OR has_database_privilege(r.name, current_database(), 'TEMPORARY');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[G] qp_app, qp_migrator and qp_admin need CONNECT = true and TEMPORARY = false; offending: %', bad;
  END IF;
END $$;

-- (H) Relations in app/river: closed to the Supabase roles and to PUBLIC.
DO $$
DECLARE bad text;
BEGIN
  -- PUBLIC first: every other role inherits it, so these must be reported as PUBLIC.
  SELECT string_agg(a.privilege_type || ' on ' || n.nspname || '.' || c.relname, ', ') INTO bad
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace CROSS JOIN LATERAL aclexplode(c.relacl) AS a
  WHERE n.nspname IN ('app','river') AND c.relkind IN ('r','p','v','m','S','f') AND a.grantee = 0;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[H4] PUBLIC must hold no privileges on relations in app/river; offending: %', bad;
  END IF;

  SELECT string_agg(a.privilege_type || ' on ' || n.nspname || '.' || c.relname || '.' || t.attname, ', ') INTO bad
  FROM pg_attribute t JOIN pg_class c ON c.oid = t.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
  CROSS JOIN LATERAL aclexplode(t.attacl) AS a
  WHERE n.nspname IN ('app','river') AND c.relkind IN ('r','p','v','m','S','f')
    AND t.attnum > 0 AND NOT t.attisdropped AND a.grantee = 0;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[H5] PUBLIC must hold no column privileges in app/river; offending: %', bad;
  END IF;

  SELECT string_agg(r.name || ' ' || p.priv || ' on ' || n.nspname || '.' || c.relname, ', ') INTO bad
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
  CROSS JOIN (VALUES ('anon'),('authenticated'),('service_role')) AS r(name)
  CROSS JOIN (VALUES ('SELECT'),('INSERT'),('UPDATE'),('DELETE'),('TRUNCATE'),('REFERENCES'),('TRIGGER')) AS p(priv)
  WHERE n.nspname IN ('app','river') AND c.relkind IN ('r','p','v','m','f')
    AND has_table_privilege(r.name, c.oid, p.priv);
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[H1] anon, authenticated, service_role must have no table privileges in app/river; offending: %', bad;
  END IF;

  SELECT string_agg(r.name || ' column ' || p.priv || ' on ' || n.nspname || '.' || c.relname, ', ') INTO bad
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
  CROSS JOIN (VALUES ('anon'),('authenticated'),('service_role')) AS r(name)
  CROSS JOIN (VALUES ('SELECT'),('INSERT'),('UPDATE'),('REFERENCES')) AS p(priv)
  WHERE n.nspname IN ('app','river') AND c.relkind IN ('r','p','v','m','f')
    AND has_any_column_privilege(r.name, c.oid, p.priv);
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[H2] anon, authenticated, service_role must have no column privileges in app/river; offending: %', bad;
  END IF;

  SELECT string_agg(r.name || ' ' || p.priv || ' on sequence ' || n.nspname || '.' || c.relname, ', ') INTO bad
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
  CROSS JOIN (VALUES ('anon'),('authenticated'),('service_role')) AS r(name)
  CROSS JOIN (VALUES ('USAGE'),('SELECT'),('UPDATE')) AS p(priv)
  WHERE n.nspname IN ('app','river') AND c.relkind = 'S'
    AND has_sequence_privilege(r.name, c.oid, p.priv);
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[H3] anon, authenticated, service_role must have no sequence privileges in app/river; offending: %', bad;
  END IF;
END $$;

-- (I) Functions in app/river: the Supabase roles (and PUBLIC through them) cannot EXECUTE.
-- qp_app's own function access in app is not asserted here: a migration may grant it on
-- purpose (the platform_* split between qp_app and qp_admin IS asserted by [N] below).
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(r.name || ' EXECUTE on ' || n.nspname || '.' || p.proname, ', ') INTO bad
  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
  CROSS JOIN (VALUES ('anon'),('authenticated'),('service_role')) AS r(name)
  WHERE n.nspname IN ('app','river') AND has_function_privilege(r.name, p.oid, 'EXECUTE');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[I] anon, authenticated, service_role must not EXECUTE functions in app/river; offending: %', bad;
  END IF;
END $$;

-- (J) qp_app table hygiene: no TRUNCATE, TRIGGER or REFERENCES.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(p.priv || ' on ' || n.nspname || '.' || c.relname, ', ') INTO bad
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
  CROSS JOIN (VALUES ('TRUNCATE'),('TRIGGER'),('REFERENCES')) AS p(priv)
  WHERE n.nspname IN ('app','river') AND c.relkind IN ('r','p')
    AND has_table_privilege('qp_app', c.oid, p.priv);
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[J] qp_app must not hold TRUNCATE, TRIGGER or REFERENCES; offending: %', bad;
  END IF;
END $$;

-- (J2) qp_app holds no grant option on any relation, column, schema or function in app/river.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(o.what, ', ') INTO bad FROM (
    SELECT a.privilege_type || ' on ' || n.nspname || '.' || c.relname AS what
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace CROSS JOIN LATERAL aclexplode(c.relacl) AS a
    WHERE n.nspname IN ('app','river') AND a.is_grantable AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = 'qp_app')
    UNION ALL
    SELECT a.privilege_type || ' on schema ' || n.nspname
    FROM pg_namespace n CROSS JOIN LATERAL aclexplode(n.nspacl) AS a
    WHERE n.nspname IN ('app','river') AND a.is_grantable AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = 'qp_app')
    UNION ALL
    SELECT a.privilege_type || ' on column ' || n.nspname || '.' || c.relname || '.' || t.attname
    FROM pg_attribute t JOIN pg_class c ON c.oid = t.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
    CROSS JOIN LATERAL aclexplode(t.attacl) AS a
    WHERE n.nspname IN ('app','river') AND t.attnum > 0 AND NOT t.attisdropped
      AND a.is_grantable AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = 'qp_app')
    UNION ALL
    SELECT a.privilege_type || ' on function ' || n.nspname || '.' || p.proname
    FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace CROSS JOIN LATERAL aclexplode(p.proacl) AS a
    WHERE n.nspname IN ('app','river') AND a.is_grantable AND a.grantee = (SELECT oid FROM pg_roles WHERE rolname = 'qp_app')
  ) AS o;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[J2] qp_app must hold no WITH GRANT OPTION in app/river; offending: %', bad;
  END IF;
END $$;

-- (K) Default privileges of qp_migrator.
-- Global default-ACL rows of roles other than qp_migrator are deliberately not checked here
-- (platform-managed on hosted Supabase); K5 and X only look at app/river per-schema rows.
-- eff is the EFFECTIVE default ACL per schema in (app, river): the union of the global row
-- (defaclnamespace = 0) and the per-schema row, owner entry ignored. defaclacl is NOT NULL.
-- It is a CTE run through EXECUTE, so nothing is created in the session (no TEMPORARY needed).
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
  -- K1a: init creates the global function row (PUBLIC EXECUTE revoked); a missing row means the
  -- built-in default (EXECUTE to PUBLIC) is back.
  IF NOT EXISTS (SELECT 1 FROM pg_default_acl d WHERE d.defaclrole = (SELECT oid FROM pg_roles WHERE rolname = 'qp_migrator') AND d.defaclnamespace = 0 AND d.defaclobjtype = 'f') THEN
    RAISE EXCEPTION '[K1a] global function default ACL row for qp_migrator is missing; built-in default grants EXECUTE to PUBLIC';
  END IF;

  -- K1b: global rows (any object type) must equal the owner entry only.
  SELECT string_agg(d.defaclobjtype::text || ' grants ' || a.privilege_type || ' to ' || coalesce(g.rolname, 'PUBLIC'), ', ') INTO bad
  FROM pg_default_acl d CROSS JOIN LATERAL aclexplode(d.defaclacl) AS a
  LEFT JOIN pg_roles g ON g.oid = a.grantee
  WHERE d.defaclrole = (SELECT oid FROM pg_roles WHERE rolname = 'qp_migrator')
    AND d.defaclnamespace = 0 AND a.grantee <> d.defaclrole;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[K1b] global default ACLs of qp_migrator must grant nothing beyond the owner; offending: %', bad;
  END IF;

  -- K2: nothing effective for PUBLIC or the Supabase roles, on any object type.
  EXECUTE 'WITH eff AS (' || eff || $s$)
    SELECT string_agg(v.nsp || ' type ' || v.objtype || ' grants ' || v.priv || ' to ' || coalesce(g.rolname, 'PUBLIC'), ', ')
    FROM eff v LEFT JOIN pg_roles g ON g.oid = v.grantee
    WHERE v.grantee = 0 OR g.rolname IN ('anon','authenticated','service_role') $s$ INTO bad;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[K2] effective default ACLs of qp_migrator in app/river must not reach PUBLIC or the Supabase roles; offending: %', bad;
  END IF;

  -- K3: exact effective privileges for qp_app; an empty expected set also fails on extras,
  -- and a non-empty expected set fails when the row is absent.
  EXECUTE 'WITH eff AS (' || eff || $s$)
    SELECT string_agg(e.nsp || ' type ' || e.objtype || ' want {' || array_to_string(e.privs, ',') || '} got {' || array_to_string(act.privs, ',') || '}', '; ')
    FROM (VALUES
      ('app',   'r', ARRAY['DELETE','INSERT','SELECT','UPDATE']),
      ('river', 'r', ARRAY['DELETE','INSERT','SELECT','UPDATE']),
      ('app',   'S', ARRAY['SELECT','USAGE']),
      ('river', 'S', ARRAY['SELECT','USAGE']),
      ('app',   'f', ARRAY[]::text[]),
      ('river', 'f', ARRAY['EXECUTE']),
      ('app',   'T', ARRAY[]::text[]),
      ('river', 'T', ARRAY[]::text[]),
      ('app',   'n', ARRAY[]::text[]),
      ('river', 'n', ARRAY[]::text[])
    ) AS e(nsp, objtype, privs)
    CROSS JOIN LATERAL (
      SELECT coalesce(array_agg(v.priv ORDER BY v.priv), ARRAY[]::text[]) AS privs
      FROM eff v
      WHERE v.nsp = e.nsp AND v.objtype = e.objtype AND v.grantee = (SELECT oid FROM pg_roles WHERE rolname = 'qp_app')
    ) AS act
    WHERE act.privs IS DISTINCT FROM e.privs $s$ INTO bad;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[K3] effective default ACLs of qp_migrator for qp_app differ from expected; offending: %', bad;
  END IF;

  -- K4: qp_app must not receive a default privilege WITH GRANT OPTION.
  EXECUTE 'WITH eff AS (' || eff || $s$)
    SELECT string_agg(v.nsp || ' type ' || v.objtype || ' ' || v.priv, ', ')
    FROM eff v
    WHERE v.is_grantable AND v.grantee = (SELECT oid FROM pg_roles WHERE rolname = 'qp_app') $s$ INTO bad;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[K4] default privileges for qp_app must not carry WITH GRANT OPTION; offending: %', bad;
  END IF;

  -- K5: per-schema rows of ANY role in app/river must not reach PUBLIC or the Supabase roles.
  SELECT string_agg(pg_get_userbyid(d.defaclrole) || ' in ' || n.nspname || ' type ' || d.defaclobjtype::text || ' grants ' || a.privilege_type || ' to ' || coalesce(g.rolname, 'PUBLIC'), ', ') INTO bad
  FROM pg_default_acl d JOIN pg_namespace n ON n.oid = d.defaclnamespace
  CROSS JOIN LATERAL aclexplode(d.defaclacl) AS a LEFT JOIN pg_roles g ON g.oid = a.grantee
  WHERE n.nspname IN ('app','river') AND (a.grantee = 0 OR g.rolname IN ('anon','authenticated','service_role'));
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[K5] no role may default-grant to PUBLIC or the Supabase roles in app/river; offending: %', bad;
  END IF;
END $$;

-- (X) Allowlist: in schemas app and river the only grantees anywhere are the object owner,
-- qp_migrator, qp_app and qp_admin; PUBLIC never. Covers schema, relation (tables, sequences,
-- views, partitions), column and function ACLs, plus the default ACLs of ANY role for
-- app/river and the global qp_migrator rows. It runs after the denylist checks (F, H, I, K)
-- so those keep their more specific messages. On hosted Supabase a named exception list for
-- platform roles is expected later (ADR-007, to be written).
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(DISTINCT o.what || ' to ' || coalesce(g.rolname, 'PUBLIC'), ', ') INTO bad FROM (
    SELECT a.privilege_type || ' on schema ' || n.nspname AS what, n.nspowner AS owner, a.grantee
    FROM pg_namespace n CROSS JOIN LATERAL aclexplode(n.nspacl) AS a
    WHERE n.nspname IN ('app','river')
    UNION ALL
    SELECT a.privilege_type || ' on relation ' || n.nspname || '.' || c.relname, c.relowner, a.grantee
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace CROSS JOIN LATERAL aclexplode(c.relacl) AS a
    WHERE n.nspname IN ('app','river')
    UNION ALL
    SELECT a.privilege_type || ' on column ' || n.nspname || '.' || c.relname || '.' || t.attname, c.relowner, a.grantee
    FROM pg_attribute t JOIN pg_class c ON c.oid = t.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
    CROSS JOIN LATERAL aclexplode(t.attacl) AS a
    WHERE n.nspname IN ('app','river') AND t.attnum > 0 AND NOT t.attisdropped
    UNION ALL
    SELECT a.privilege_type || ' on function ' || n.nspname || '.' || p.proname, p.proowner, a.grantee
    FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace CROSS JOIN LATERAL aclexplode(p.proacl) AS a
    WHERE n.nspname IN ('app','river')
    UNION ALL
    SELECT a.privilege_type || ' by default (' || pg_get_userbyid(d.defaclrole) || ' in ' || coalesce(n.nspname, 'all schemas') || ' type ' || d.defaclobjtype::text || ')', d.defaclrole, a.grantee
    FROM pg_default_acl d LEFT JOIN pg_namespace n ON n.oid = d.defaclnamespace CROSS JOIN LATERAL aclexplode(d.defaclacl) AS a
    WHERE n.nspname IN ('app','river')
       OR (d.defaclnamespace = 0 AND d.defaclrole = (SELECT oid FROM pg_roles WHERE rolname = 'qp_migrator'))
  ) AS o LEFT JOIN pg_roles g ON g.oid = o.grantee
  WHERE o.grantee = 0 OR (o.grantee <> o.owner AND NOT (g.rolname = ANY (ARRAY['qp_migrator','qp_app','qp_admin'])));
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[X] in app/river only the owner, qp_migrator, qp_app and qp_admin may hold privileges, never PUBLIC; offending: %', bad;
  END IF;
END $$;

-- (L) Extensions in schema extensions; pgvector >= 0.8 (ADR-003 section 4).
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(x.name || ' (schema: ' || coalesce(n.nspname, 'not installed') || ')', ', ') INTO bad
  FROM (VALUES ('vector'),('pg_trgm')) AS x(name)
  LEFT JOIN pg_extension e ON e.extname = x.name
  LEFT JOIN pg_namespace n ON n.oid = e.extnamespace
  WHERE n.nspname IS DISTINCT FROM 'extensions';
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[L1] vector and pg_trgm must be installed in schema extensions; offending: %', bad;
  END IF;

  IF NOT EXISTS (
    SELECT 1 FROM pg_extension e
    WHERE e.extname = 'vector'
      AND string_to_array(regexp_replace(e.extversion, '[^0-9.].*$', ''), '.')::int[] >= ARRAY[0,8]) THEN
    RAISE EXCEPTION '[L2] pgvector >= 0.8 required, found: %', (SELECT extversion FROM pg_extension WHERE extname = 'vector');
  END IF;
END $$;

-- (M) FORCE RLS on every table in app with an organization_id column.
-- Allowlist (spec 2026-11-10-data-model-v1.3 section 3, ADR-005 section 1):
-- organization_memberships is a Layer A platform table read before a tenant
-- context exists, so it is function-gated instead of RLS-protected. The same
-- list lives in tenantdb.assertRLSForced and schema_lint_test.go — a new
-- entry must land in all three on purpose.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(n.nspname || '.' || c.relname || ' (rls=' || c.relrowsecurity || ', force=' || c.relforcerowsecurity || ')', ', ') INTO bad
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
  WHERE n.nspname = 'app' AND c.relkind IN ('r','p')
    AND EXISTS (SELECT 1 FROM pg_attribute t WHERE t.attrelid = c.oid AND t.attname = 'organization_id' AND t.attnum > 0 AND NOT t.attisdropped)
    AND NOT (c.relrowsecurity AND c.relforcerowsecurity)
    AND c.relname::text <> ALL (ARRAY['organization_memberships']);
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION '[M] tables with organization_id need ENABLE and FORCE ROW LEVEL SECURITY; offending: %', bad;
  END IF;
END $$;

-- (N) The qp_admin/qp_app function-EXECUTE surface moved to
-- platform-functions.sql: it asserts the post-migration state, so CI runs it
-- only after migrations exist (it is vacuous-true before them).

-- Informational only (the assertions above are what gate).
SELECT rolname, rolsuper, rolbypassrls, rolcreatedb, rolcreaterole, rolcanlogin FROM pg_roles WHERE rolname IN ('qp_app','qp_migrator','qp_admin') ORDER BY rolname;
SELECT e.extname, e.extversion, n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname IN ('vector','pg_trgm') ORDER BY e.extname;

SELECT 'roles.sql: all assertions passed' AS result;
