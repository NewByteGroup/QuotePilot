\set ON_ERROR_STOP on
-- Role, schema, grant and extension checks for the local database. Plain SQL, no
-- secrets. Also the base for the Mission 3 isolation spike and the ADR-003
-- provisioning check. Run as the bootstrap superuser:
--   docker compose exec -T db sh -c 'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' < infra/postgres/checks/roles.sql

-- Guard: fail loudly if init did not run, so the checks below cannot pass vacuously.
DO $$
BEGIN
  IF (SELECT count(*) FROM pg_roles WHERE rolname IN ('qp_app','qp_migrator','anon','authenticated','service_role')) < 5 THEN
    RAISE EXCEPTION 'expected roles qp_app, qp_migrator, anon, authenticated, service_role; init script did not run (see infra/README.md, make db-reset)';
  END IF;
END $$;

-- expect: qp_app and qp_migrator rows, rolsuper = f, rolbypassrls = f
SELECT rolname, rolsuper, rolbypassrls, rolcreatedb, rolcreaterole FROM pg_roles WHERE rolname IN ('qp_app','qp_migrator') ORDER BY rolname;

-- expect: app and river, both owned by qp_migrator
SELECT nspname, pg_get_userbyid(nspowner) AS owner FROM pg_namespace WHERE nspname IN ('app','river') ORDER BY nspname;

-- expect: zero rows (qp_app owns no relation, schema, function or type in app/river/extensions)
SELECT 'relation' AS kind, n.nspname AS schema, c.relname AS name FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname IN ('app','river','extensions') AND c.relowner = (SELECT oid FROM pg_roles WHERE rolname = 'qp_app')
UNION ALL
SELECT 'schema', n.nspname, n.nspname FROM pg_namespace n WHERE n.nspname IN ('app','river','extensions') AND n.nspowner = (SELECT oid FROM pg_roles WHERE rolname = 'qp_app')
UNION ALL
SELECT 'function', n.nspname, p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname IN ('app','river','extensions') AND p.proowner = (SELECT oid FROM pg_roles WHERE rolname = 'qp_app')
UNION ALL
SELECT 'type', n.nspname, t.typname FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace WHERE n.nspname IN ('app','river','extensions') AND t.typowner = (SELECT oid FROM pg_roles WHERE rolname = 'qp_app');

-- expect: zero rows (ADR-003 section 2 grants check, using has_table_privilege)
SELECT r.rolname, n.nspname, c.relname, p.priv FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace CROSS JOIN (VALUES ('anon'),('authenticated'),('service_role')) AS r(rolname) CROSS JOIN (VALUES ('SELECT'),('INSERT'),('UPDATE'),('DELETE'),('TRUNCATE'),('REFERENCES'),('TRIGGER')) AS p(priv) WHERE n.nspname IN ('app','river') AND c.relkind IN ('r','p','v','m') AND has_table_privilege(r.rolname, c.oid, p.priv);

-- expect: zero rows (no Supabase role can use the schemas)
SELECT r.rolname, s.schema FROM (VALUES ('anon'),('authenticated'),('service_role')) AS r(rolname) CROSS JOIN (VALUES ('app'),('river')) AS s(schema) WHERE has_schema_privilege(r.rolname, s.schema, 'USAGE');

-- expect: zero rows (PUBLIC, grantee 0, holds no privilege on tables or sequences in app/river)
SELECT n.nspname, c.relname, c.relkind, a.privilege_type FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace CROSS JOIN LATERAL aclexplode(c.relacl) AS a WHERE n.nspname IN ('app','river') AND c.relkind IN ('r','p','v','m','S') AND a.grantee = 0;

-- expect: vector and pg_trgm, both in schema extensions
SELECT e.extname, e.extversion, n.nspname FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname IN ('vector','pg_trgm') ORDER BY e.extname;

-- expect: has_database_privilege rows: qp_app and qp_migrator CONNECT = t, TEMPORARY = f
SELECT r.rolname, has_database_privilege(r.rolname, current_database(), 'CONNECT') AS connect, has_database_privilege(r.rolname, current_database(), 'TEMPORARY') AS temp FROM (VALUES ('qp_app'),('qp_migrator')) AS r(rolname);

-- Assertions: pgvector >= 0.8 (ADR-003 section 4); app roles are neither superuser nor BYPASSRLS.
DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector' AND string_to_array(extversion, '.')::int[] >= ARRAY[0,8]) THEN
    RAISE EXCEPTION 'pgvector >= 0.8 required, found: %', (SELECT extversion FROM pg_extension WHERE extname = 'vector');
  END IF;
  IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname IN ('qp_app','qp_migrator') AND (rolsuper OR rolbypassrls)) THEN
    RAISE EXCEPTION 'qp_app and qp_migrator must have rolsuper = false and rolbypassrls = false';
  END IF;
END $$;

-- Assertions: each property fails with a non-zero exit (ON_ERROR_STOP) and lists offenders.
-- Schemas app and river exist and are owned by qp_migrator.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(s.name || ' (owner: ' || coalesce(pg_get_userbyid(n.nspowner), 'missing') || ')', ', ') INTO bad
  FROM (VALUES ('app'),('river')) AS s(name)
  LEFT JOIN pg_namespace n ON n.nspname = s.name
  WHERE n.oid IS NULL OR pg_get_userbyid(n.nspowner) <> 'qp_migrator';
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION 'schemas app and river must exist and be owned by qp_migrator; offending: %', bad;
  END IF;
END $$;

-- qp_app owns no relation, schema, function or type in app/river/extensions.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(o.kind || ' ' || o.schema || '.' || o.name, ', ') INTO bad FROM (
    SELECT 'relation' AS kind, n.nspname AS schema, c.relname AS name FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname IN ('app','river','extensions') AND c.relowner = (SELECT oid FROM pg_roles WHERE rolname = 'qp_app')
    UNION ALL
    SELECT 'schema', n.nspname, n.nspname FROM pg_namespace n WHERE n.nspname IN ('app','river','extensions') AND n.nspowner = (SELECT oid FROM pg_roles WHERE rolname = 'qp_app')
    UNION ALL
    SELECT 'function', n.nspname, p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname IN ('app','river','extensions') AND p.proowner = (SELECT oid FROM pg_roles WHERE rolname = 'qp_app')
    UNION ALL
    SELECT 'type', n.nspname, t.typname FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace WHERE n.nspname IN ('app','river','extensions') AND t.typowner = (SELECT oid FROM pg_roles WHERE rolname = 'qp_app')
  ) AS o;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION 'qp_app must own nothing in app, river, extensions; offending: %', bad;
  END IF;
END $$;

-- anon, authenticated, service_role hold no table privilege in app/river.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(r.rolname || ' ' || p.priv || ' on ' || n.nspname || '.' || c.relname, ', ') INTO bad
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace CROSS JOIN (VALUES ('anon'),('authenticated'),('service_role')) AS r(rolname) CROSS JOIN (VALUES ('SELECT'),('INSERT'),('UPDATE'),('DELETE'),('TRUNCATE'),('REFERENCES'),('TRIGGER')) AS p(priv)
  WHERE n.nspname IN ('app','river') AND c.relkind IN ('r','p','v','m') AND has_table_privilege(r.rolname, c.oid, p.priv);
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION 'anon, authenticated, service_role must have no table privileges in app/river; offending: %', bad;
  END IF;
END $$;

-- anon, authenticated, service_role cannot use schemas app or river.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(r.rolname || ' USAGE on ' || s.schema, ', ') INTO bad
  FROM (VALUES ('anon'),('authenticated'),('service_role')) AS r(rolname) CROSS JOIN (VALUES ('app'),('river')) AS s(schema)
  WHERE has_schema_privilege(r.rolname, s.schema, 'USAGE');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION 'anon, authenticated, service_role must not have USAGE on app/river; offending: %', bad;
  END IF;
END $$;

-- PUBLIC (grantee 0) holds no privilege on tables or sequences in app/river.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(a.privilege_type || ' on ' || n.nspname || '.' || c.relname, ', ') INTO bad
  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace CROSS JOIN LATERAL aclexplode(c.relacl) AS a
  WHERE n.nspname IN ('app','river') AND c.relkind IN ('r','p','v','m','S') AND a.grantee = 0;
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION 'PUBLIC must hold no privileges on relations in app/river; offending: %', bad;
  END IF;
END $$;

-- vector and pg_trgm are installed in schema extensions.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(x.name || ' (schema: ' || coalesce(n.nspname, 'not installed') || ')', ', ') INTO bad
  FROM (VALUES ('vector'),('pg_trgm')) AS x(name)
  LEFT JOIN pg_extension e ON e.extname = x.name
  LEFT JOIN pg_namespace n ON n.oid = e.extnamespace
  WHERE n.nspname IS DISTINCT FROM 'extensions';
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION 'vector and pg_trgm must be installed in schema extensions; offending: %', bad;
  END IF;
END $$;

-- qp_app and qp_migrator: CONNECT = true, TEMPORARY = false on the current database.
DO $$
DECLARE bad text;
BEGIN
  SELECT string_agg(r.rolname || ' (connect=' || has_database_privilege(r.rolname, current_database(), 'CONNECT') || ', temp=' || has_database_privilege(r.rolname, current_database(), 'TEMPORARY') || ')', ', ') INTO bad
  FROM (VALUES ('qp_app'),('qp_migrator')) AS r(rolname)
  WHERE NOT has_database_privilege(r.rolname, current_database(), 'CONNECT') OR has_database_privilege(r.rolname, current_database(), 'TEMPORARY');
  IF bad IS NOT NULL THEN
    RAISE EXCEPTION 'qp_app and qp_migrator need CONNECT = true and TEMPORARY = false; offending: %', bad;
  END IF;
END $$;

SELECT 'roles.sql: all assertions passed' AS result;
