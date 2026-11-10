package tenantdb_test

// Auth/session enforcement for migration 0003 (spec
// 2026-11-10-tenant-resolution-api sections 3, 5 and 9): the Layer A
// login/session function family exercised as qp_app — hash-only tokens,
// single-use consume, family-rooted rotation and reuse-kill. Layer A rows
// (users, orgs, memberships) are seeded as qp_migrator because qp_app holds no
// table privilege on them at all; that denial is part of what these tests
// prove. Split from schema_enforcement_test.go to keep files under the
// 500-line style limit (AGENTS.md).

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// seedAuthUser inserts an app.users row as qp_migrator — the only role that
// may write Layer A — and returns its id (testenv.SeedOrganization's
// equivalent for the identity table).
func seedAuthUser(t *testing.T, email, fullName string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, testenv.MigratorPool(t).QueryRow(context.Background(),
		`INSERT INTO app.users (email, full_name) VALUES ($1, $2) RETURNING id`,
		email, fullName).Scan(&id))
	return id
}

// TestSchemaEnforcementAuthLookup covers the three read-side functions:
// find_user_by_email (normalization included), get_user_profile and
// get_membership — plus the grant contract around them (spec section 9).
func TestSchemaEnforcementAuthLookup(t *testing.T) {
	db := testenv.Open(t)
	admin := testenv.AdminDB(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()

	org := uuid.New()
	testenv.SeedOrganization(t, org, "auth-lookup")
	userEmail := "auth-lookup-" + uuid.NewString() + "@test.dev"
	userID := seedAuthUser(t, userEmail, "Auth Lookup")
	_, err := migrator.Exec(ctx,
		`INSERT INTO app.organization_memberships (organization_id, user_id, role) VALUES ($1, $2, 'PRICING_ANALYST')`,
		org.String(), userID.String())
	require.NoError(t, err)

	// find_user_by_email: exact, then case + whitespace normalization, then
	// unknown — the users.email CHECK stores lowercase only, so the function
	// must lower() and btrim() the argument.
	var found *uuid.UUID
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.find_user_by_email($1)`, userEmail).Scan(&found))
	require.NotNil(t, found)
	assert.Equal(t, userID, *found)
	found = nil
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.find_user_by_email($1)`, "  "+strings.ToUpper(userEmail)+"  ").Scan(&found))
	require.NotNil(t, found)
	assert.Equal(t, userID, *found, "lookup must lower() and btrim() the argument")
	found = nil
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.find_user_by_email($1)`, "nobody-"+uuid.NewString()+"@test.dev").Scan(&found))
	assert.Nil(t, found, "unknown email must return NULL")

	// get_user_profile returns exactly the documented columns.
	var profID uuid.UUID
	var profEmail, profName string
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT id, email, full_name FROM app.get_user_profile($1)`,
		userID.String()).Scan(&profID, &profEmail, &profName))
	assert.Equal(t, userID, profID)
	assert.Equal(t, userEmail, profEmail)
	assert.Equal(t, "Auth Lookup", profName)
	var profileRows int
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM app.get_user_profile($1)`, uuid.NewString()).Scan(&profileRows))
	assert.Zero(t, profileRows, "unknown user must return no row")

	// get_membership: the role for a member; NULL indistinguishably for a
	// non-member and for an org that does not exist — that
	// indistinguishability is the security property (spec section 7).
	var role *string
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.get_membership($1, $2)::text`, userID.String(), org.String()).Scan(&role))
	require.NotNil(t, role)
	assert.Equal(t, "PRICING_ANALYST", *role)
	role = nil
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.get_membership($1, $2)::text`, uuid.NewString(), org.String()).Scan(&role))
	assert.Nil(t, role, "non-member must return NULL")
	role = nil
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.get_membership($1, $2)::text`, userID.String(), uuid.NewString()).Scan(&role))
	assert.Nil(t, role, "nonexistent org must return NULL — same shape as non-member")

	// The auth tables are Layer A: direct reads are denied even though the
	// functions work.
	var direct int
	directErr := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM app.auth_login_tokens`).Scan(&direct)
	require.Error(t, directErr, "qp_app must not SELECT app.auth_login_tokens directly")
	assert.Contains(t, directErr.Error(), "permission denied")
	directErr = db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM app.auth_sessions`).Scan(&direct)
	require.Error(t, directErr, "qp_app must not SELECT app.auth_sessions directly")
	assert.Contains(t, directErr.Error(), "permission denied")

	// qp_admin reaches none of the family (spec section 5): every call is a
	// permission error, not an empty answer.
	for _, call := range []string{
		`SELECT app.find_user_by_email('x@test.dev')`,
		`SELECT app.create_login_token(gen_random_uuid(), 'h', now())`,
		`SELECT app.consume_login_token('h')`,
		`SELECT app.create_session(gen_random_uuid(), 'h', now())`,
		`SELECT * FROM app.rotate_session('h', 'h2', now())`,
		`SELECT app.revoke_session(gen_random_uuid())`,
		`SELECT app.revoke_session_family_by_hash('h')`,
		`SELECT app.get_membership(gen_random_uuid(), gen_random_uuid())`,
		`SELECT * FROM app.get_user_profile(gen_random_uuid())`,
		`SELECT * FROM app.list_memberships_for_email('x@test.dev')`,
	} {
		_, err := admin.Pool().Exec(ctx, call)
		require.Error(t, err, "qp_admin must not EXECUTE %s", call)
		assert.Contains(t, err.Error(), "permission denied")
	}
}

// TestSchemaEnforcementLoginToken proves the magic-link lifecycle: create
// returns the token id, consume returns the user exactly once, and consumed,
// expired and unknown hashes all collapse to NULL (spec section 9).
func TestSchemaEnforcementLoginToken(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()

	userID := seedAuthUser(t, "auth-token-"+uuid.NewString()+"@test.dev", "Token User")

	// Happy path: create returns the token id, consume returns the user once.
	tokenHash := "hash-" + uuid.NewString()
	var tokenID uuid.UUID
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.create_login_token($1, $2, now() + interval '15 minutes')`,
		userID.String(), tokenHash).Scan(&tokenID))
	require.NotEqual(t, uuid.Nil, tokenID)
	var consumed *uuid.UUID
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.consume_login_token($1)`, tokenHash).Scan(&consumed))
	require.NotNil(t, consumed)
	assert.Equal(t, userID, *consumed)

	// Single use: the second consume is NULL, not the user again.
	consumed = nil
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.consume_login_token($1)`, tokenHash).Scan(&consumed))
	assert.Nil(t, consumed, "a consumed token must not work twice")

	// Expired and unknown hashes are NULL too — same shape as consumed.
	expiredHash := "hash-" + uuid.NewString()
	_, err := db.Pool().Exec(ctx,
		`SELECT app.create_login_token($1, $2, now() - interval '1 minute')`,
		userID.String(), expiredHash)
	require.NoError(t, err)
	consumed = nil
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.consume_login_token($1)`, expiredHash).Scan(&consumed))
	assert.Nil(t, consumed, "an expired token must return NULL")
	consumed = nil
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.consume_login_token($1)`, "hash-"+uuid.NewString()).Scan(&consumed))
	assert.Nil(t, consumed, "an unknown token must return NULL")
}

// TestSchemaEnforcementSessionRotation proves the refresh-token contract:
// generation 0 roots its own family, rotation descends in the same family,
// reuse kills every generation in the same atomic call, and
// unknown/expired/revoked hashes all return the empty set (spec sections 4-5).
func TestSchemaEnforcementSessionRotation(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()

	userID := seedAuthUser(t, "auth-sess-"+uuid.NewString()+"@test.dev", "Session User")

	// create_session: generation 0 roots the family at its own id.
	gen0Hash := "rh-" + uuid.NewString()
	var gen0 uuid.UUID
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.create_session($1, $2, now() + interval '30 days')`,
		userID.String(), gen0Hash).Scan(&gen0))
	require.NotEqual(t, uuid.Nil, gen0)
	var family uuid.UUID
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT family_id FROM app.auth_sessions WHERE id = $1`, gen0.String()).Scan(&family))
	assert.Equal(t, gen0, family, "generation 0 must root its own family")

	// Two rotations descend in the family rooted at gen0; each returns
	// rotated=true with the successor's id and the user.
	gen1Hash := "rh-" + uuid.NewString()
	var gen1, gen1User uuid.UUID
	var rotated bool
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT session_id, user_id, rotated FROM app.rotate_session($1, $2, now() + interval '30 days')`,
		gen0Hash, gen1Hash).Scan(&gen1, &gen1User, &rotated))
	assert.True(t, rotated, "the first rotation must succeed")
	assert.Equal(t, userID, gen1User)
	require.NotEqual(t, gen0, gen1)

	gen2Hash := "rh-" + uuid.NewString()
	var gen2 uuid.UUID
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT session_id, user_id, rotated FROM app.rotate_session($1, $2, now() + interval '30 days')`,
		gen1Hash, gen2Hash).Scan(&gen2, &gen1User, &rotated))
	assert.True(t, rotated, "the chain must keep rotating on the live generation")

	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT family_id FROM app.auth_sessions WHERE id = $1`, gen2.String()).Scan(&family))
	assert.Equal(t, gen0, family, "the second successor must descend in the same family")
	var rotatedAtSet bool
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT rotated_at IS NOT NULL FROM app.auth_sessions WHERE id = $1`, gen0.String()).Scan(&rotatedAtSet))
	assert.True(t, rotatedAtSet, "the exchanged generation must be stamped rotated_at")

	// Reuse: presenting the rotated gen0 again returns rotated=false with
	// NULL ids — and revokes every generation of the family, so the live
	// gen2 dies with it.
	var reuseID, reuseUser *uuid.UUID
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT session_id, user_id, rotated FROM app.rotate_session($1, $2, now() + interval '30 days')`,
		gen0Hash, "rh-"+uuid.NewString()).Scan(&reuseID, &reuseUser, &rotated))
	assert.False(t, rotated, "presenting an already-rotated token is reuse")
	assert.Nil(t, reuseID)
	assert.Nil(t, reuseUser)
	var liveRows int
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT count(*) FROM app.auth_sessions WHERE family_id = $1 AND revoked_at IS NULL`,
		gen0.String()).Scan(&liveRows))
	assert.Zero(t, liveRows, "reuse must revoke the whole family")

	// The family-killed gen2 hash now resolves to the empty set, same as
	// unknown, expired and revoked hashes.
	for name, hash := range map[string]string{
		"family-killed successor": gen2Hash,
		"unknown hash":            "rh-" + uuid.NewString(),
	} {
		var rows int
		require.NoError(t, db.Pool().QueryRow(ctx,
			`SELECT count(*) FROM app.rotate_session($1, $2, now() + interval '30 days')`,
			hash, "rh-"+uuid.NewString()).Scan(&rows))
		assert.Zero(t, rows, "%s must return the empty set", name)
	}

	// An expired session rotates to nothing.
	expiredHash := "rh-" + uuid.NewString()
	_, err := db.Pool().Exec(ctx,
		`SELECT app.create_session($1, $2, now() - interval '1 minute')`,
		userID.String(), expiredHash)
	require.NoError(t, err)
	var rows int
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM app.rotate_session($1, $2, now() + interval '30 days')`,
		expiredHash, "rh-"+uuid.NewString()).Scan(&rows))
	assert.Zero(t, rows, "an expired session must return the empty set")

	// revoke_session: single-row revoke — true once, false on replay and on
	// an unknown id; the revoked session's hash then resolves to empty. (The
	// logout interface is revoke_session_family_by_hash, proven below.)
	revokedHash := "rh-" + uuid.NewString()
	var liveSession uuid.UUID
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.create_session($1, $2, now() + interval '30 days')`,
		userID.String(), revokedHash).Scan(&liveSession))
	var ok bool
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.revoke_session($1)`, liveSession.String()).Scan(&ok))
	assert.True(t, ok, "the first revoke must return true")
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.revoke_session($1)`, liveSession.String()).Scan(&ok))
	assert.False(t, ok, "revoking twice must return false")
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.revoke_session($1)`, uuid.NewString()).Scan(&ok))
	assert.False(t, ok, "revoking an unknown id must return false")
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM app.rotate_session($1, $2, now() + interval '30 days')`,
		revokedHash, "rh-"+uuid.NewString()).Scan(&rows))
	assert.Zero(t, rows, "a revoked session must return the empty set")
}

// TestSchemaEnforcementSessionFamilyRevoke proves logout's revocation
// interface (spec 2026-11-10-tenant-resolution-api section 5): the opaque
// refresh cookie resolves to its whole family — every generation dies, live
// and already-rotated alike — in one atomic call. The boolean is metrics
// only: true while any generation was still live, false on replay and on an
// unknown hash.
func TestSchemaEnforcementSessionFamilyRevoke(t *testing.T) {
	db := testenv.Open(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()

	userID := seedAuthUser(t, "auth-fam-"+uuid.NewString()+"@test.dev", "Family User")

	// A family with two generations: gen0 already rotated, gen1 live.
	gen0Hash := "rh-" + uuid.NewString()
	gen1Hash := "rh-" + uuid.NewString()
	var gen0 uuid.UUID
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.create_session($1, $2, now() + interval '30 days')`,
		userID.String(), gen0Hash).Scan(&gen0))
	var gen1, gen1User uuid.UUID
	var rotated bool
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT session_id, user_id, rotated FROM app.rotate_session($1, $2, now() + interval '30 days')`,
		gen0Hash, gen1Hash).Scan(&gen1, &gen1User, &rotated))
	require.True(t, rotated)

	// Logout presents the *rotated* generation's hash: it still resolves the
	// family, and every generation — rotated gen0 and live gen1 — dies.
	var killed bool
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.revoke_session_family_by_hash($1)`, gen0Hash).Scan(&killed))
	assert.True(t, killed, "a family with a live generation must report true")
	var liveRows int
	require.NoError(t, migrator.QueryRow(ctx,
		`SELECT count(*) FROM app.auth_sessions WHERE family_id = $1 AND revoked_at IS NULL`,
		gen0.String()).Scan(&liveRows))
	assert.Zero(t, liveRows, "every generation of the family must be revoked")

	// Replay and unknown hashes collapse to false — the endpoint still
	// answers 204 either way, so the boolean is not an oracle.
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.revoke_session_family_by_hash($1)`, gen0Hash).Scan(&killed))
	assert.False(t, killed, "an already-dead family must return false")
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.revoke_session_family_by_hash($1)`, "rh-"+uuid.NewString()).Scan(&killed))
	assert.False(t, killed, "an unknown hash must return false")

	// The kill lands on the refresh path too: rotating either generation's
	// hash now returns the empty set.
	for _, hash := range []string{gen0Hash, gen1Hash} {
		var rows int
		require.NoError(t, db.Pool().QueryRow(ctx,
			`SELECT count(*) FROM app.rotate_session($1, $2, now() + interval '30 days')`,
			hash, "rh-"+uuid.NewString()).Scan(&rows))
		assert.Zero(t, rows, "a family-killed generation must return the empty set")
	}
}
