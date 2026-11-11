package platform_test

// Store accessors over the real Layer A function family (spec
// 2026-11-10-tenant-resolution-api sections 5 and 9): found/not-found for
// every accessor plus the rotate three-outcome mapping, all as qp_app with
// Layer A rows seeded as qp_migrator — qp_app holds no table privilege on
// them, and the Store may only reach them through the functions.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

func newStore(t *testing.T) *platform.Store {
	t.Helper()
	return platform.NewStore(testenv.Open(t).Pool())
}

// seedUser inserts an app.users row as qp_migrator (the only role that may
// write Layer A) and returns its id. An empty fullName lands as NULL so the
// NULL -> "" mapping is exercised too.
func seedUser(t *testing.T, email, fullName string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, testenv.MigratorPool(t).QueryRow(context.Background(),
		`INSERT INTO app.users (email, full_name) VALUES ($1, NULLIF($2, '')) RETURNING id`,
		email, fullName).Scan(&id))
	return id
}

// seedMembership links user and org with the given role, as qp_migrator.
func seedMembership(t *testing.T, orgID, userID uuid.UUID, role string) {
	t.Helper()
	_, err := testenv.MigratorPool(t).Exec(context.Background(),
		`INSERT INTO app.organization_memberships (organization_id, user_id, role) VALUES ($1, $2, $3::app.membership_role)`,
		orgID, userID, role)
	require.NoError(t, err)
}

// freshHash mints a real opaque token and returns its at-rest hash — the
// same pair production holds, never a literal.
func freshHash(t *testing.T) string {
	t.Helper()
	token, err := platform.NewOpaqueToken()
	require.NoError(t, err)
	return platform.HashToken(token)
}

func uniqueEmail(prefix string) string {
	return prefix + "-" + uuid.NewString() + "@test.dev"
}

// TestStoreFindUserByEmail covers found (with input normalization done by
// the function) and not-found.
func TestStoreFindUserByEmail(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	email := uniqueEmail("store-find")
	userID := seedUser(t, email, "Store Find")

	id, ok, err := store.FindUserByEmail(ctx, "  "+email+"  ")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, userID, id)

	_, ok, err = store.FindUserByEmail(ctx, uniqueEmail("store-find-unknown"))
	require.NoError(t, err)
	assert.False(t, ok, "unknown email must be (Nil, false, nil) — no error")
}

// TestStoreLoginTokenLifecycle covers create, single-use consume, and the
// consumed/unknown collapse to found=false.
func TestStoreLoginTokenLifecycle(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	userID := seedUser(t, uniqueEmail("store-token"), "Token User")

	hash := freshHash(t)
	tokenID, err := store.CreateLoginToken(ctx, userID, hash, time.Now().Add(15*time.Minute))
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, tokenID)

	gotUser, ok, err := store.ConsumeLoginToken(ctx, hash)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, userID, gotUser)

	_, ok, err = store.ConsumeLoginToken(ctx, hash)
	require.NoError(t, err)
	assert.False(t, ok, "a consumed token must not work twice")

	_, ok, err = store.ConsumeLoginToken(ctx, freshHash(t))
	require.NoError(t, err)
	assert.False(t, ok, "an unknown hash must be (Nil, false, nil)")
}

// TestStoreRotateSession proves the three-outcome mapping the refresh
// endpoint branches on: rotated=true with ids, rotated=false on reuse (the
// family is already dead), and found=false on unknown/dead hashes.
func TestStoreRotateSession(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	userID := seedUser(t, uniqueEmail("store-rotate"), "Rotate User")
	expiry := time.Now().Add(30 * 24 * time.Hour)

	gen0Hash := freshHash(t)
	gen0, err := store.CreateSession(ctx, userID, gen0Hash, expiry)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, gen0)

	// Happy path: rotated=true, successor id differs, user returned.
	gen1Hash := freshHash(t)
	gen1, gotUser, rotated, found, err := store.RotateSession(ctx, gen0Hash, gen1Hash, expiry)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, rotated)
	assert.Equal(t, userID, gotUser)
	assert.NotEqual(t, gen0, gen1)

	// Reuse: presenting the rotated gen0 maps the function's
	// rotated=false row — found=true distinguishes it from the empty set.
	sid, uid, rotated, found, err := store.RotateSession(ctx, gen0Hash, freshHash(t), expiry)
	require.NoError(t, err)
	require.True(t, found, "reuse returns a row; only unknown/dead hashes are the empty set")
	assert.False(t, rotated)
	assert.Equal(t, uuid.Nil, sid)
	assert.Equal(t, uuid.Nil, uid)

	// The reuse killed the family: the live successor's hash now resolves
	// to the same empty set as an unknown hash.
	_, _, _, found, err = store.RotateSession(ctx, gen1Hash, freshHash(t), expiry)
	require.NoError(t, err)
	assert.False(t, found, "family-killed successor must be the empty set")

	_, _, _, found, err = store.RotateSession(ctx, freshHash(t), freshHash(t), expiry)
	require.NoError(t, err)
	assert.False(t, found, "unknown hash must be the empty set")
}

// TestStoreRevokeSession covers the single-row revoke: true once, false on
// replay and on an unknown id.
func TestStoreRevokeSession(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	userID := seedUser(t, uniqueEmail("store-revoke"), "Revoke User")

	sessionID, err := store.CreateSession(ctx, userID, freshHash(t), time.Now().Add(time.Hour))
	require.NoError(t, err)

	ok, err := store.RevokeSession(ctx, sessionID)
	require.NoError(t, err)
	assert.True(t, ok)

	ok, err = store.RevokeSession(ctx, sessionID)
	require.NoError(t, err)
	assert.False(t, ok, "revoking twice must be false")

	ok, err = store.RevokeSession(ctx, uuid.New())
	require.NoError(t, err)
	assert.False(t, ok, "unknown id must be false")
}

// TestStoreRevokeSessionFamilyByHash covers logout's interface: presenting
// any generation's hash kills the family, then reports false on replay and
// on unknown hashes.
func TestStoreRevokeSessionFamilyByHash(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	userID := seedUser(t, uniqueEmail("store-family"), "Family User")
	expiry := time.Now().Add(30 * 24 * time.Hour)

	gen0Hash := freshHash(t)
	_, err := store.CreateSession(ctx, userID, gen0Hash, expiry)
	require.NoError(t, err)
	gen1Hash := freshHash(t)
	_, _, rotated, _, err := store.RotateSession(ctx, gen0Hash, gen1Hash, expiry)
	require.NoError(t, err)
	require.True(t, rotated)

	killed, err := store.RevokeSessionFamilyByHash(ctx, gen0Hash)
	require.NoError(t, err)
	assert.True(t, killed, "a family with a live generation reports true")

	killed, err = store.RevokeSessionFamilyByHash(ctx, gen0Hash)
	require.NoError(t, err)
	assert.False(t, killed, "an already-dead family reports false")

	killed, err = store.RevokeSessionFamilyByHash(ctx, freshHash(t))
	require.NoError(t, err)
	assert.False(t, killed, "an unknown hash reports false")
}

// TestStoreGetMembership proves the tenant check returns the role for a
// member and found=false — identically — for a non-member and a
// nonexistent org (the spec section 7 security property).
func TestStoreGetMembership(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	orgID := uuid.New()
	testenv.SeedOrganization(t, orgID, "store-member")
	userID := seedUser(t, uniqueEmail("store-member"), "Member User")
	seedMembership(t, orgID, userID, "PRICING_ANALYST")

	role, ok, err := store.GetMembership(ctx, userID, orgID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "PRICING_ANALYST", role)

	_, ok, err = store.GetMembership(ctx, uuid.New(), orgID)
	require.NoError(t, err)
	assert.False(t, ok, "non-member must be false")

	_, ok, err = store.GetMembership(ctx, userID, uuid.New())
	require.NoError(t, err)
	assert.False(t, ok, "nonexistent org must be false — same shape as non-member")
}

// TestStoreGetUserProfile covers the identity lookup: the row for a known
// user (full_name nullable → "" mapping), ok=false for an unknown id.
func TestStoreGetUserProfile(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	email := uniqueEmail("store-profile")
	userID := seedUser(t, email, "Profile User")

	id, gotEmail, gotName, ok, err := store.GetUserProfile(ctx, userID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, userID, id)
	assert.Equal(t, email, gotEmail)
	assert.Equal(t, "Profile User", gotName)

	nullNameID := seedUser(t, uniqueEmail("store-noname"), "")
	_, _, gotName, ok, err = store.GetUserProfile(ctx, nullNameID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "", gotName, "NULL full_name maps to empty string")

	_, _, _, ok, err = store.GetUserProfile(ctx, uuid.New())
	require.NoError(t, err)
	assert.False(t, ok, "unknown user must be ok=false")
}

// TestStoreListMembershipsForEmail proves the org-switcher shape: name and
// slug ride the membership row; unknown email is an empty slice.
func TestStoreListMembershipsForEmail(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()

	email := uniqueEmail("store-list")
	userID := seedUser(t, email, "List User")
	org1, org2 := uuid.New(), uuid.New()
	slug1 := testenv.SeedOrganization(t, org1, "store-list-one")
	slug2 := testenv.SeedOrganization(t, org2, "store-list-two")
	seedMembership(t, org1, userID, "OWNER")
	seedMembership(t, org2, userID, "ADMIN")

	got, err := store.ListMembershipsForEmail(ctx, email)
	require.NoError(t, err)
	require.Len(t, got, 2)
	byOrg := map[uuid.UUID]platform.Membership{}
	for _, m := range got {
		byOrg[m.OrgID] = m
	}
	require.Contains(t, byOrg, org1)
	assert.Equal(t, "OWNER", byOrg[org1].Role)
	assert.Equal(t, slug1, byOrg[org1].Slug)
	assert.Equal(t, "Test "+slug1, byOrg[org1].Name)
	require.Contains(t, byOrg, org2)
	assert.Equal(t, "ADMIN", byOrg[org2].Role)
	assert.Equal(t, slug2, byOrg[org2].Slug)

	got, err = store.ListMembershipsForEmail(ctx, uniqueEmail("store-list-unknown"))
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.NotNil(t, got, "an unknown email is an empty slice, never nil")
}

// TestStoreGetCurrentOrganization proves the tenant-scoped accessor end to
// end: inside InTenantReadTx the GUC resolves the org row — branding
// included — while the same call on the bare pool yields (zero, false, nil).
// The row exists only through the function; qp_app holds no grant on
// app.organizations itself.
func TestStoreGetCurrentOrganization(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()

	orgID := uuid.New()
	slug := testenv.SeedOrganization(t, orgID, "store-curorg")
	_, err := testenv.MigratorPool(t).Exec(ctx,
		`UPDATE app.organizations SET branding_json = $2 WHERE id = $1`,
		orgID, `{"primary":"#0a84ff"}`)
	require.NoError(t, err)

	var got platform.Organization
	var found bool
	require.NoError(t, db.InTenantReadTx(ctx, orgID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, found, err = platform.NewStore(tx).GetCurrentOrganization(ctx)
		return err
	}))
	require.True(t, found, "inside the tenant tx the GUC must resolve the org row")
	assert.Equal(t, orgID, got.ID)
	assert.Equal(t, "Test "+slug, got.Name)
	assert.Equal(t, slug, got.Slug)
	assert.Equal(t, slug+"@in.test", got.InboundEmailAlias)
	assert.JSONEq(t, `{"primary":"#0a84ff"}`, string(got.Branding))

	// Same call outside a tenant context: the NULL composite is the miss.
	_, found, err = platform.NewStore(db.Pool()).GetCurrentOrganization(ctx)
	require.NoError(t, err)
	assert.False(t, found, "no tenant context must be (zero, false, nil), not an error")
}

// TestStoreResolveInboundOrg covers every branch of app.resolve_inbound_org
// (ADR-005 section 7) as qp_app: exact-alias match, the pinned
// <slug>@in.quotepilot.app fallback, plus-tag and angle-bracket
// normalization, the malformed double-@ refusal and the plain miss. Misses
// are (nil, false, nil) — never an error, so the webhook can answer a drop
// without retrying.
func TestStoreResolveInboundOrg(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	orgID := uuid.New()
	slug := testenv.SeedOrganization(t, orgID, "store-resolve")

	for name, c := range resolveCases(slug, orgID) {
		got, ok, err := store.ResolveInboundOrg(ctx, c.recipient)
		require.NoError(t, err, name)
		require.Equal(t, c.wantOK, ok, name)
		require.Equal(t, c.wantOrg, got, name)
	}
}

// resolveCase is one resolve_inbound_org outcome: recipient in, (org, ok)
// expected out.
type resolveCase struct {
	recipient string
	wantOrg   uuid.UUID
	wantOK    bool
}

// resolveCases enumerates the function's six branches against a seeded org.
func resolveCases(slug string, orgID uuid.UUID) map[string]resolveCase {
	return map[string]resolveCase{
		"exact alias":       {slug + "@in.test", orgID, true},
		"slug fallback":     {slug + "@in.quotepilot.app", orgID, true},
		"plus tag stripped": {slug + "+invoice@in.test", orgID, true},
		"angle brackets":    {"Quotes <" + slug + "@in.test>", orgID, true},
		"double at refused": {slug + "@in.test@evil.test", uuid.Nil, false},
		"unknown alias":     {"nobody@in.test", uuid.Nil, false},
	}
}

// TestStoreMailOutboxLifecycle proves the durable-send contract (migration
// 0004): create returns the outbox id, read is idempotent so a retried
// worker sees the same link, delete is the post-send receipt, and an
// absent id collapses to ("", false, nil). Empty and whitespace-only
// link_urls are refused — either would queue a send that can never
// deliver.
func TestStoreMailOutboxLifecycle(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	userID := seedUser(t, uniqueEmail("store-outbox"), "Outbox User")
	tokenID, err := store.CreateLoginToken(ctx, userID, freshHash(t), time.Now().Add(15*time.Minute))
	require.NoError(t, err)

	link := "https://app.quotepilot.test/auth/exchange?token=" + uuid.NewString()
	outboxID, err := store.CreateMailOutbox(ctx, tokenID, link)
	require.NoError(t, err)
	require.NotEqual(t, uuid.Nil, outboxID)

	gotLink, ok := readOutbox(t, store, outboxID)
	require.True(t, ok)
	assert.Equal(t, link, gotLink)

	_, ok = readOutbox(t, store, outboxID)
	assert.True(t, ok, "read is idempotent — a retried worker sees the same link")
	deleted, err := store.DeleteMailOutbox(ctx, outboxID)
	require.NoError(t, err)
	assert.True(t, deleted)

	_, ok = readOutbox(t, store, outboxID)
	assert.False(t, ok, "delete-on-send: the row is gone after the receipt")
	_, ok = readOutbox(t, store, uuid.New())
	assert.False(t, ok, "an absent id is (\"\", false, nil), never an error")
	deleted, err = store.DeleteMailOutbox(ctx, uuid.New())
	require.NoError(t, err)
	assert.False(t, deleted, "deleting an absent row is false, not an error")
	for _, bad := range []string{"", "   "} {
		_, err = store.CreateMailOutbox(ctx, tokenID, bad)
		require.Error(t, err, "a blank link_url must be refused, got %q", bad)
	}
}

// readOutbox runs one ReadMailOutbox and asserts the error leg away, so the
// lifecycle test reads as (link, ok) pairs only.
func readOutbox(t *testing.T, store *platform.Store, id uuid.UUID) (string, bool) {
	t.Helper()
	link, ok, err := store.ReadMailOutbox(context.Background(), id)
	require.NoError(t, err)
	return link, ok
}
