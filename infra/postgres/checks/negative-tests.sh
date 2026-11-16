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
PLATFORM_SQL="$CHECKS_DIR/platform-functions.sql"
cd "$CHECKS_DIR/../../.."  # repo root, where docker-compose.yml lives
passed=0
failed=0
skipped=0
failures=()

run_psql() {
  if [ -n "${QP_CHECK_DSN:-}" ]; then
    psql -X -v ON_ERROR_STOP=1 -q "$QP_CHECK_DSN"
  else
    docker compose exec -T db sh -c 'psql -X -v ON_ERROR_STOP=1 -q -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
  fi
}

# run_psql_t: same connection, tuples-only unaligned output for probe queries.
run_psql_t() {
  if [ -n "${QP_CHECK_DSN:-}" ]; then
    psql -X -v ON_ERROR_STOP=1 -q -t -A "$QP_CHECK_DSN"
  else
    docker compose exec -T db sh -c 'psql -X -v ON_ERROR_STOP=1 -q -t -A -U "$POSTGRES_USER" -d "$POSTGRES_DB"'
  fi
}

# case_raises CHECK_FILE FRAGMENT MUTATION_SQL: sets $out; returns 0 only if
# CHECK_FILE raised FRAGMENT. Function-privilege assertions live in
# platform-functions.sql; everything else in roles.sql.
case_raises() {
  local check_file="$1" fragment="$2" mutation="$3" rc=0 after
  out="$({ printf 'BEGIN;\n%s\n\\warn QP_MUTATION_APPLIED\n' "$mutation"; cat "$check_file"; printf '\nROLLBACK;\n'; } | run_psql 2>&1)" || rc=$?
  CASE_RC=$rc
  [ "$rc" -ne 0 ] && [[ "$out" == *QP_MUTATION_APPLIED* ]] || return 1
  after="${out#*QP_MUTATION_APPLIED}"
  [[ "$after" == *"ERROR:  $fragment"* ]]
}

# run_case_in CHECK_FILE NAME EXPECTED_FRAGMENT MUTATION_SQL
run_case_in() {
  local check_file="$1" name="$2" fragment="$3" mutation="$4" out CASE_RC=0
  if case_raises "$check_file" "$fragment" "$mutation"; then
    echo "PASS  $name"; passed=$((passed + 1))
  else
    echo "FAIL  $name (exit=$CASE_RC, wanted non-zero and '$fragment' after the mutation)"
    echo "$out" | sed 's/^/      /' | tail -n 8
    failed=$((failed + 1)); failures+=("$name")
  fi
}

# run_case NAME EXPECTED_FRAGMENT MUTATION_SQL — against roles.sql.
run_case() {
  run_case_in "$ROLES_SQL" "$@"
}

# run_case_if CHECK_FILE FUNC_SIG NAME EXPECTED_FRAGMENT MUTATION_SQL: like
# run_case_in, but prints SKIP and counts neither pass nor fail when FUNC_SIG
# does not resolve. The CI integration-test job runs this script BEFORE
# migrations, where the platform_* and tenant-facing functions do not exist;
# the mutation would fail with "function does not exist" before the marker,
# which the harness rightly rejects as a FAIL. Those cases get their real
# proof post-migrate in the migrations job.
run_case_if() {
  local check_file="$1" sig="$2" name="$3" found
  shift 3
  found="$(printf "SELECT to_regprocedure('%s') IS NOT NULL;\n" "$sig" | run_psql_t)"
  if [ "$found" != "t" ]; then
    echo "SKIP  $name ($sig absent pre-migration)"; skipped=$((skipped + 1)); return 0
  fi
  run_case_in "$check_file" "$name" "$@"
}

# run_selftest: the harness must REJECT an error raised by the mutation itself.
run_selftest() {
  local out CASE_RC=0
  if case_raises "$ROLES_SQL" "[A] fake" "DO \$\$ BEGIN RAISE EXCEPTION '[A] fake'; END \$\$;"; then
    echo "FAIL  selftest (harness accepted an error raised by the mutation)"
    failed=$((failed + 1)); failures+=("selftest")
  else
    echo "PASS  selftest (error raised by the mutation is rejected)"; passed=$((passed + 1))
  fi
}

run_baseline() {
  local f out rc name
  for f in "$ROLES_SQL" "$PLATFORM_SQL"; do
    rc=0
    out="$({ printf 'BEGIN;\n'; cat "$f"; printf '\nROLLBACK;\n'; } | run_psql 2>&1)" || rc=$?
    name="baseline ($(basename "$f"))"
    if [ "$rc" -eq 0 ] && [[ "$out" == *"all assertions passed"* ]]; then
      echo "PASS  $name"; passed=$((passed + 1))
    else
      echo "FAIL  $name (exit=$rc)"; echo "$out" | sed 's/^/      /' | tail -n 8
      failed=$((failed + 1)); failures+=("$name")
    fi
  done
}

TBL='CREATE TABLE app.neg_t (id int, note text); ALTER TABLE app.neg_t OWNER TO qp_migrator;'
DBSET=$'DO $$ BEGIN EXECUTE format(\'ALTER ROLE qp_app IN DATABASE %I SET search_path = public, app\', current_database()); END $$;'
DBCREATE=$'DO $$ BEGIN EXECUTE format(\'GRANT CREATE ON DATABASE %I TO qp_app\', current_database()); END $$;'
DBTEMP=$'DO $$ BEGIN EXECUTE format(\'GRANT TEMPORARY ON DATABASE %I TO PUBLIC\', current_database()); END $$;'
DBCREATE_ANON=$'DO $$ BEGIN EXECUTE format(\'GRANT CREATE ON DATABASE %I TO anon\', current_database()); END $$;'
DBCONN_ADMIN=$'DO $$ BEGIN EXECUTE format(\'REVOKE CONNECT ON DATABASE %I FROM qp_admin\', current_database()); END $$;'
DBTEMP_ADMIN=$'DO $$ BEGIN EXECUTE format(\'GRANT TEMPORARY ON DATABASE %I TO qp_admin\', current_database()); END $$;'
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

# qp_admin (platform path, ADR-005): same role-model properties as qp_app plus
# the [N] split — it alone EXECUTEs platform_* and holds nothing else.
run_case "67 qp_admin member of pg_monitor"          "[B4]"  'GRANT pg_monitor TO qp_admin;'
run_case "68 qp_admin granted to anon"               "[B3]"  'GRANT qp_admin TO anon;'
run_case "69 qp_admin CREATEROLE"                    "[A]"   'ALTER ROLE qp_admin CREATEROLE;'
run_case "70 qp_admin BYPASSRLS"                     "[A]"   'ALTER ROLE qp_admin BYPASSRLS;'
run_case "71 qp_admin search_path changed"           "[C1]"  'ALTER ROLE qp_admin SET search_path = public, app;'
run_case "72 qp_admin CREATE on schema app"          "[E1]"  'GRANT CREATE ON SCHEMA app TO qp_admin;'
run_case "73 qp_admin USAGE on app revoked"          "[F3]"  'REVOKE USAGE ON SCHEMA app FROM qp_admin;'
run_case "74 qp_admin USAGE on extensions revoked"   "[F3]"  'REVOKE USAGE ON SCHEMA extensions FROM qp_admin;'
run_case "75 qp_admin USAGE on river"                "[F3]"  'GRANT USAGE ON SCHEMA river TO qp_admin;'
run_case "76 qp_admin CONNECT revoked"               "[G]"   "$DBCONN_ADMIN"
run_case "77 qp_admin TEMPORARY on database"         "[G]"   "$DBTEMP_ADMIN"
run_case "78 table owned by qp_admin"                "[D]"   'CREATE TABLE app.neg_t (id int); ALTER TABLE app.neg_t OWNER TO qp_admin;'
# [N] assertions live in platform-functions.sql (the post-migration
# function-privilege surface). Cases 79-82 also probe the function they mutate.
run_case_if "$PLATFORM_SQL" "app.platform_delete_organization(uuid)" "79 qp_app EXECUTE platform_delete_organization" "[N]" 'GRANT EXECUTE ON FUNCTION app.platform_delete_organization(uuid) TO qp_app;'
run_case_if "$PLATFORM_SQL" "app.platform_update_healing_session(uuid, app.healing_status, text, boolean, text)" "80 qp_app EXECUTE platform_update_healing_session" "[N]" 'GRANT EXECUTE ON FUNCTION app.platform_update_healing_session(uuid, app.healing_status, text, boolean, text) TO qp_app;'
run_case_if "$PLATFORM_SQL" "app.platform_insert_public_snapshot(text, text, text, text, text, jsonb, numeric, jsonb, timestamptz)" "81 qp_admin EXECUTE platform fn revoked"   "[N]"   'REVOKE EXECUTE ON FUNCTION app.platform_insert_public_snapshot(text, text, text, text, text, jsonb, numeric, jsonb, timestamptz) FROM qp_admin;'
run_case_if "$PLATFORM_SQL" "app.resolve_inbound_org(text)" "82 qp_admin EXECUTE tenant-facing fn"      "[N]"   'GRANT EXECUTE ON FUNCTION app.resolve_inbound_org(text) TO qp_admin;'
run_case_in "$PLATFORM_SQL" "83 table SELECT to qp_admin"              "[N]"   "$TBL GRANT SELECT ON app.neg_t TO qp_admin;"
run_case_in "$PLATFORM_SQL" "84 default EXECUTE in app to qp_admin"    "[N]"   "$ADP IN SCHEMA app GRANT EXECUTE ON FUNCTIONS TO qp_admin;"
run_case_in "$PLATFORM_SQL" "85 column SELECT to qp_admin"             "[N]"   "$TBL GRANT SELECT (note) ON app.neg_t TO qp_admin;"
# 86 stays on roles.sql: [K1b] (global default ACLs) is a roles.sql assertion.
run_case "86 global EXECUTE default to qp_admin"     "[K1b]" "$ADP GRANT EXECUTE ON FUNCTIONS TO qp_admin;"
run_case_in "$PLATFORM_SQL" "87 river EXECUTE to qp_admin"             "[N]"   'CREATE FUNCTION river.neg_f() RETURNS int LANGUAGE sql AS $$ SELECT 1 $$; REVOKE EXECUTE ON FUNCTION river.neg_f() FROM PUBLIC; GRANT EXECUTE ON FUNCTION river.neg_f() TO qp_admin;'

# Migration 0003 auth/session surface: every case is gated on an 0003
# signature (run_case_if) so it SKIPs until the migration exists — the
# mutations need the real function or table.
run_case_if "$PLATFORM_SQL" "app.find_user_by_email(text)"            "88 qp_app EXECUTE revoked on tenant fn" "[N]"  'REVOKE EXECUTE ON FUNCTION app.find_user_by_email(text) FROM qp_app;'
run_case_if "$PLATFORM_SQL" "app.rotate_session(text, text, timestamptz)" "89 qp_admin EXECUTE rotate_session"  "[N]"  'GRANT EXECUTE ON FUNCTION app.rotate_session(text, text, timestamptz) TO qp_admin;'
run_case_if "$PLATFORM_SQL" "app.create_session(uuid, text, timestamptz)" "90 qp_app SELECT on auth_login_tokens" "[N]" 'GRANT SELECT ON app.auth_login_tokens TO qp_app;'
run_case_if "$PLATFORM_SQL" "app.create_session(uuid, text, timestamptz)" "91 qp_app INSERT/DELETE on auth_sessions" "[N]" 'GRANT INSERT, DELETE ON app.auth_sessions TO qp_app;'
run_case_if "$ROLES_SQL" "app.get_membership(uuid, uuid)"             "92 PUBLIC EXECUTE get_membership"     "[I]"  'GRANT EXECUTE ON FUNCTION app.get_membership(uuid, uuid) TO PUBLIC;'
run_case_if "$ROLES_SQL" "app.get_user_profile(uuid)"                 "93 anon EXECUTE get_user_profile"     "[I]"  'GRANT EXECUTE ON FUNCTION app.get_user_profile(uuid) TO anon;'
run_case_if "$ROLES_SQL" "app.consume_login_token(text)"              "94 anon SELECT auth_sessions"         "[H1]" 'GRANT SELECT ON app.auth_sessions TO anon;'
run_case_if "$ROLES_SQL" "app.consume_login_token(text)"              "95 PUBLIC SELECT auth_login_tokens"   "[H4]" 'GRANT SELECT ON app.auth_login_tokens TO PUBLIC;'
run_case_if "$PLATFORM_SQL" "app.revoke_session_family_by_hash(text)" "96 qp_admin EXECUTE revoke_session_family_by_hash" "[N]" 'GRANT EXECUTE ON FUNCTION app.revoke_session_family_by_hash(text) TO qp_admin;'
run_case_if "$PLATFORM_SQL" "app.read_mail_outbox(uuid)"              "97 qp_app SELECT on auth_mail_outbox"  "[N]" 'GRANT SELECT ON app.auth_mail_outbox TO qp_app;'

echo
echo "negative tests: $passed passed, $failed failed, $skipped skipped"
if [ "$failed" -ne 0 ]; then
  printf 'failed: %s\n' "${failures[@]}"
  exit 1
fi
