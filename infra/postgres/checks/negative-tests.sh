#!/usr/bin/env bash
# Proves each roles.sql assertion can fail. Every case runs one mutation followed by
# roles.sql inside BEGIN ... ROLLBACK; nothing is ever committed, so the database is
# unchanged afterwards. After the mutation the script prints the marker QP_MUTATION_APPLIED
# (psql \warn, same stream as errors, so ordering is guaranteed). A case passes only when psql
# exits non-zero and "ERROR:  <fragment>" appears AFTER that marker, so an error raised by the
# mutation itself (before the marker, or no marker at all) can never count as PASS.
# Works from any directory.
#
# Connection: defaults to `docker compose exec db` (local). Set QP_CHECK_DSN to a
# full libpq DSN to hit any Postgres over TCP instead — used by CI, where the
# database is a service container and there is no compose service to exec into.
set -euo pipefail

CHECKS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROLES_SQL="$CHECKS_DIR/roles.sql"
cd "$CHECKS_DIR/../../.."  # repo root, where docker-compose.yml lives
passed=0
failed=0
failures=()

run_psql() {
  if [ -n "${QP_CHECK_DSN:-}" ]; then
    psql -X -v ON_ERROR_STOP=1 -q "$QP_CHECK_DSN"
  else
    docker compose exec -T db sh -c 'psql -X -v ON_ERROR_STOP=1 -q -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
  fi
}

# case_raises FRAGMENT MUTATION_SQL: sets $out; returns 0 only if roles.sql raised FRAGMENT.
case_raises() {
  local fragment="$1" mutation="$2" rc=0 after
  out="$({ printf 'BEGIN;\n%s\n\\warn QP_MUTATION_APPLIED\n' "$mutation"; cat "$ROLES_SQL"; printf '\nROLLBACK;\n'; } | run_psql 2>&1)" || rc=$?
  CASE_RC=$rc
  [ "$rc" -ne 0 ] && [[ "$out" == *QP_MUTATION_APPLIED* ]] || return 1
  after="${out#*QP_MUTATION_APPLIED}"
  [[ "$after" == *"ERROR:  $fragment"* ]]
}

# run_case NAME EXPECTED_FRAGMENT MUTATION_SQL
run_case() {
  local name="$1" fragment="$2" mutation="$3" out CASE_RC=0
  if case_raises "$fragment" "$mutation"; then
    echo "PASS  $name"; passed=$((passed + 1))
  else
    echo "FAIL  $name (exit=$CASE_RC, wanted non-zero and '$fragment' after the mutation)"
    echo "$out" | sed 's/^/      /' | tail -n 8
    failed=$((failed + 1)); failures+=("$name")
  fi
}

# run_selftest: the harness must REJECT an error raised by the mutation itself.
run_selftest() {
  local out CASE_RC=0
  if case_raises "[A] fake" "DO \$\$ BEGIN RAISE EXCEPTION '[A] fake'; END \$\$;"; then
    echo "FAIL  selftest (harness accepted an error raised by the mutation)"
    failed=$((failed + 1)); failures+=("selftest")
  else
    echo "PASS  selftest (error raised by the mutation is rejected)"; passed=$((passed + 1))
  fi
}

run_baseline() {
  local out rc=0
  out="$({ printf 'BEGIN;\n'; cat "$ROLES_SQL"; printf '\nROLLBACK;\n'; } | run_psql 2>&1)" || rc=$?
  if [ "$rc" -eq 0 ] && [[ "$out" == *"all assertions passed"* ]]; then
    echo "PASS  baseline (no mutation)"; passed=$((passed + 1))
  else
    echo "FAIL  baseline (exit=$rc)"; echo "$out" | sed 's/^/      /' | tail -n 8
    failed=$((failed + 1)); failures+=("baseline")
  fi
}

TBL='CREATE TABLE app.neg_t (id int, note text); ALTER TABLE app.neg_t OWNER TO qp_migrator;'
DBSET=$'DO $$ BEGIN EXECUTE format(\'ALTER ROLE qp_app IN DATABASE %I SET search_path = public, app\', current_database()); END $$;'
DBCREATE=$'DO $$ BEGIN EXECUTE format(\'GRANT CREATE ON DATABASE %I TO qp_app\', current_database()); END $$;'
DBTEMP=$'DO $$ BEGIN EXECUTE format(\'GRANT TEMPORARY ON DATABASE %I TO PUBLIC\', current_database()); END $$;'
DBCREATE_ANON=$'DO $$ BEGIN EXECUTE format(\'GRANT CREATE ON DATABASE %I TO anon\', current_database()); END $$;'
ADP='ALTER DEFAULT PRIVILEGES FOR ROLE qp_migrator'

# name | expected tag | mutation. One case per line.
run_baseline
run_selftest
run_case "01 qp_migrator granted to qp_app"          "[B1]"  'GRANT qp_migrator TO qp_app;'
run_case "02 pg_write_all_data granted to qp_app"    "[B1]"  'GRANT pg_write_all_data TO qp_app;'
run_case "03 qp_app CREATEROLE"                      "[A]"   'ALTER ROLE qp_app CREATEROLE;'
run_case "04 qp_app CREATEDB"                        "[A]"   'ALTER ROLE qp_app CREATEDB;'
run_case "05 qp_app search_path changed"             "[C1]"   'ALTER ROLE qp_app SET search_path = public, app;'
run_case "06 qp_app CREATE on schema app"            "[E1]"  'GRANT CREATE ON SCHEMA app TO qp_app;'
run_case "07 qp_app CREATE on schema extensions"     "[E1]"  'GRANT CREATE ON SCHEMA extensions TO qp_app;'
run_case "08 anon CREATE on schema app"              "[E1]"  'GRANT CREATE ON SCHEMA app TO anon;'
run_case "09 qp_app CREATE on database"              "[E2]"  "$DBCREATE"
run_case "10 default ACL tables to anon in app"      "[K2]"  "$ADP IN SCHEMA app GRANT ALL ON TABLES TO anon;"
run_case "11 global function default to PUBLIC"      "[K1a]" "$ADP GRANT EXECUTE ON FUNCTIONS TO PUBLIC;"
run_case "12 default EXECUTE in app to qp_app"       "[K3]"  "$ADP IN SCHEMA app GRANT EXECUTE ON FUNCTIONS TO qp_app;"
run_case "13 default SELECT on app tables revoked"   "[K3]"  "$ADP IN SCHEMA app REVOKE SELECT ON TABLES FROM qp_app;"
run_case "14 column SELECT to anon"                  "[H2]"  "$TBL GRANT SELECT (note) ON app.neg_t TO anon;"
run_case "15 sequence USAGE to anon"                 "[H3]"  'CREATE TABLE app.neg_s (id bigserial); ALTER TABLE app.neg_s OWNER TO qp_migrator; GRANT USAGE ON SEQUENCE app.neg_s_id_seq TO anon;'
run_case "16 function EXECUTE to anon"               "[I]"   'CREATE FUNCTION app.neg_f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$; REVOKE EXECUTE ON FUNCTION app.neg_f() FROM PUBLIC; GRANT EXECUTE ON FUNCTION app.neg_f() TO anon;'
run_case "17 TRUNCATE to qp_app"                     "[J]"   "$TBL GRANT TRUNCATE ON app.neg_t TO qp_app;"
run_case "18 table SELECT to anon"                   "[H1]"  "$TBL GRANT SELECT ON app.neg_t TO anon;"
run_case "19 USAGE on schema app to anon"            "[F1]"  'GRANT USAGE ON SCHEMA app TO anon;'
run_case "20 TEMPORARY on database to PUBLIC"        "[G]"   "$DBTEMP"
run_case "21 table owned by qp_app"                  "[D]"   'CREATE TABLE app.neg_t (id int); ALTER TABLE app.neg_t OWNER TO qp_app;'
run_case "22 table SELECT to PUBLIC"                 "[H4]"  "$TBL GRANT SELECT ON app.neg_t TO PUBLIC;"
run_case "23 organization_id table without RLS"      "[M]"   'CREATE TABLE app.neg_t (id int, organization_id uuid); ALTER TABLE app.neg_t OWNER TO qp_migrator;'
run_case "24 USAGE on extensions revoked"            "[F2]"  'REVOKE USAGE ON SCHEMA extensions FROM qp_app;'
run_case "25 column SELECT to PUBLIC"                "[H5]"  "$TBL GRANT SELECT (note) ON app.neg_t TO PUBLIC;"
run_case "26 role anon dropped"                      "[1]"   'DROP ROLE anon;'
run_case "27 schema river owned by superuser"        "[2]"   'ALTER SCHEMA river OWNER TO CURRENT_USER;'
run_case "28 qp_migrator REPLICATION"                "[A]"   'ALTER ROLE qp_migrator REPLICATION;'
run_case "29 qp_migrator member of BYPASSRLS role"   "[B2]"  'CREATE ROLE zb NOLOGIN BYPASSRLS; GRANT zb TO qp_migrator;'
run_case "30 pg_read_all_data INHERIT FALSE"         "[B2]"  'GRANT pg_read_all_data TO qp_migrator WITH INHERIT FALSE;'
run_case "31 pg_read_all_data SET FALSE"             "[B2]"  'GRANT pg_read_all_data TO qp_migrator WITH SET FALSE;'
run_case "32 pg_create_subscription to qp_migrator"  "[B2]"  'GRANT pg_create_subscription TO qp_migrator;'
run_case "33 pg_monitor to qp_migrator"              "[B2]"  'GRANT pg_monitor TO qp_migrator;'
run_case "34 qp_migrator granted to anon"            "[B3]"  'GRANT qp_migrator TO anon WITH INHERIT FALSE;'
run_case "35 qp_app granted to authenticated"        "[B3]"  'GRANT qp_app TO authenticated WITH INHERIT FALSE;'
run_case "36 qp_migrator granted to login role"      "[B3]"  'CREATE ROLE zlogin LOGIN; GRANT qp_migrator TO zlogin;'
run_case "37 per-database search_path override"      "[C2]"   "$DBSET"
run_case "38 pg_trgm moved to public"                "[L1]"  'ALTER EXTENSION pg_trgm SET SCHEMA public;'
run_case "39 pgvector version too old"               "[L2]"  "UPDATE pg_extension SET extversion='0.7.4' WHERE extname='vector';"
run_case "40 global function default PUBLIC + anon"  "[K1b]" "$ADP GRANT EXECUTE ON FUNCTIONS TO PUBLIC, anon;"
run_case "41 global table default to anon"           "[K1b]" "$ADP GRANT SELECT ON TABLES TO anon;"
run_case "42 global sequence default to anon"        "[K1b]" "$ADP GRANT USAGE ON SEQUENCES TO anon;"
run_case "43 global function default to anon"        "[K1b]" "$ADP GRANT EXECUTE ON FUNCTIONS TO anon;"
run_case "44 global TRUNCATE default to qp_app"      "[K1b]" "$ADP GRANT TRUNCATE ON TABLES TO qp_app;"
run_case "45 global EXECUTE default to qp_app"       "[K1b]" "$ADP GRANT EXECUTE ON FUNCTIONS TO qp_app;"
run_case "46 schema TRUNCATE default to qp_app"      "[K3]"  "$ADP IN SCHEMA app GRANT TRUNCATE ON TABLES TO qp_app;"
run_case "47 river EXECUTE default revoked"          "[K3]"  "$ADP IN SCHEMA river REVOKE EXECUTE ON FUNCTIONS FROM qp_app;"
run_case "48 default SELECT to qp_app WITH GRANT"    "[K4]"  "$ADP IN SCHEMA app GRANT SELECT ON TABLES TO qp_app WITH GRANT OPTION;"
run_case "49 other-role default ACL to anon"         "[K5]"  'ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA app GRANT SELECT ON TABLES TO anon;'
run_case "50 other-role default ACL to PUBLIC"       "[K5]"  'ALTER DEFAULT PRIVILEGES FOR ROLE postgres IN SCHEMA river GRANT EXECUTE ON FUNCTIONS TO PUBLIC;'
run_case "51 table SELECT to qp_app WITH GRANT"      "[J2]"  "$TBL GRANT SELECT ON app.neg_t TO qp_app WITH GRANT OPTION;"

run_case "52 qp_migrator in CREATEROLE/CREATEDB role" "[B2]" 'CREATE ROLE zcr NOLOGIN CREATEROLE CREATEDB; GRANT zcr TO qp_migrator;'
run_case "53 qp_migrator granted anon"               "[B2]"  'GRANT anon TO qp_migrator;'
run_case "54 default SELECT to role in anon's group" "[X]"   "CREATE ROLE zmid; GRANT zmid TO anon; $ADP IN SCHEMA app GRANT SELECT ON TABLES TO zmid;"
run_case "55 default SELECT,INSERT to LOGIN role"    "[X]"   "CREATE ROLE zlg LOGIN; $ADP IN SCHEMA app GRANT SELECT, INSERT ON TABLES TO zlg;"
run_case "56 USAGE on app to BYPASSRLS login role"   "[X]"   'CREATE ROLE zlogin LOGIN BYPASSRLS; GRANT USAGE ON SCHEMA app TO zlogin;'
run_case "57 table SELECT to arbitrary role"         "[X]"   "$TBL CREATE ROLE zlogin2 LOGIN; GRANT SELECT ON app.neg_t TO zlogin2;"
run_case "58 column SELECT to arbitrary role"        "[X]"   "$TBL CREATE ROLE zlogin3 LOGIN; GRANT SELECT (note) ON app.neg_t TO zlogin3;"
run_case "59 function EXECUTE to arbitrary role"     "[X]"   'CREATE ROLE zlogin4 LOGIN; CREATE FUNCTION app.neg_f4() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$; REVOKE EXECUTE ON FUNCTION app.neg_f4() FROM PUBLIC; GRANT EXECUTE ON FUNCTION app.neg_f4() TO zlogin4;'
run_case "60 schema owned by qp_app"                 "[D]"   'CREATE SCHEMA zevil AUTHORIZATION qp_app;'
run_case "61 public table owned by qp_app"           "[D]"   'CREATE TABLE public.neg_p (id int); ALTER TABLE public.neg_p OWNER TO qp_app;'
run_case "62 function owned by qp_app"               "[D]"   'CREATE FUNCTION app.neg_f3() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$; ALTER FUNCTION app.neg_f3() OWNER TO qp_app;'
run_case "63 type owned by qp_app"                   "[D]"   "CREATE TYPE app.neg_ty AS ENUM ('a'); ALTER TYPE app.neg_ty OWNER TO qp_app;"
run_case "64 column SELECT to qp_app WITH GRANT"     "[J2]"  "$TBL GRANT SELECT (note) ON app.neg_t TO qp_app WITH GRANT OPTION;"
run_case "65 qp_migrator CREATE on extensions"       "[E1]"  'GRANT CREATE ON SCHEMA extensions TO qp_migrator;'
run_case "66 anon CREATE on database"                "[E2]"  "$DBCREATE_ANON"

echo
echo "negative tests: $passed passed, $failed failed"
if [ "$failed" -ne 0 ]; then
  printf 'failed: %s\n' "${failures[@]}"
  exit 1
fi
