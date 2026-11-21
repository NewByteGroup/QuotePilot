package jobs_test

// Migration-0010 contract tests against real Postgres: the checkpoint
// vocabulary, the payload-presence CHECK and the NULL-carrier dedupe key
// are proven on the live schema — as qp_app inside the tenant tx, the
// same path the S3a worker will write through.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// offerExec runs one insert inside the tenant tx and returns its error —
// CHECK and UNIQUE violations arrive as query errors.
func offerExec(t *testing.T, db *tenantdb.DB, org uuid.UUID, stmt string, args ...any) error {
	t.Helper()
	return db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, stmt, args...)
		return err
	})
}

// seedOfferRFQ is a minimal rfq_requests parent row for the FK.
func seedOfferRFQ(t *testing.T, db *tenantdb.DB, org uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	require.NoError(t, db.InTenantTx(context.Background(), org, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO app.rfq_requests (organization_id, rfq_number, resend_message_id, customer_email)
			VALUES ($1, (SELECT coalesce(max(rfq_number),0)+1 FROM app.rfq_requests WHERE organization_id=$1),
			        $2, 'cust@example.test')
			RETURNING id`, org, "offer-"+uuid.NewString()).Scan(&id)
	}))
	return id
}

// TestCarrierOffersOutcomesRoundTrip: every vocabulary outcome inserts
// with its legal payload shape — OFFER_FOUND+carrier+payload,
// NO_OFFER/FETCH_FAILED+carrier only, NOT_APPLICABLE with a NULL carrier.
func TestCarrierOffersOutcomesRoundTrip(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "offers-rt")
	rfq := seedOfferRFQ(t, db, org)

	insert := `INSERT INTO app.carrier_offers
		(organization_id, rfq_id, carrier, outcome, source, request_hash, offer_payload_json, fetched_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7, now())`
	for _, tc := range []struct {
		name    string
		carrier *string
		outcome string
		hash    string
		payload *string
	}{
		{"found", ptr("HAPAG"), "OFFER_FOUND", "h1", ptr(`{"charges":[]}`)},
		{"none", ptr("HAPAG"), "NO_OFFER", "h2", nil},
		{"failed", ptr("MAERSK"), "FETCH_FAILED", "h3", nil},
		{"na", nil, "NOT_APPLICABLE", "h4", nil},
	} {
		require.NoError(t, offerExec(t, db, org, insert,
			org, rfq, tc.carrier, tc.outcome, "LIVE", tc.hash, tc.payload), tc.name)
	}
	n := countRows(t, db, org,
		`SELECT count(*) FROM app.carrier_offers WHERE rfq_id = $1`, rfq.String())
	assert.Equal(t, 4, n)
}

// TestCarrierOffersMirrorSource: a MIRROR row is a legal vocabulary
// member (the UI labels it simulated off this column, RFC-003) while an
// unknown source is rejected.
func TestCarrierOffersMirrorSource(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "offers-src")
	rfq := seedOfferRFQ(t, db, org)

	require.NoError(t, offerExec(t, db, org, `
		INSERT INTO app.carrier_offers
			(organization_id, rfq_id, carrier, outcome, source, request_hash, fetched_at)
		VALUES ($1,$2,'HAPAG','NO_OFFER','MIRROR','m1', now())`, org, rfq))
	err := offerExec(t, db, org, `
		INSERT INTO app.carrier_offers
			(organization_id, rfq_id, carrier, outcome, source, request_hash, fetched_at)
		VALUES ($1,$2,'HAPAG','NO_OFFER','BOGUS','m2', now())`, org, rfq)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "carrier_offers_source_check")
}

// TestCarrierOffersPayloadCheck enforces outcome iff payload: a found
// offer without content and a non-found outcome carrying content are
// both rejected.
func TestCarrierOffersPayloadCheck(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "offers-chk")
	rfq := seedOfferRFQ(t, db, org)

	err := offerExec(t, db, org, `
		INSERT INTO app.carrier_offers
			(organization_id, rfq_id, carrier, outcome, request_hash, offer_payload_json, fetched_at)
		VALUES ($1,$2,'HAPAG','OFFER_FOUND','c1', NULL, now())`, org, rfq)
	require.Error(t, err, "OFFER_FOUND without payload must violate the CHECK")
	assert.Contains(t, err.Error(), "carrier_offers_payload_outcome_check")

	err = offerExec(t, db, org, `
		INSERT INTO app.carrier_offers
			(organization_id, rfq_id, carrier, outcome, request_hash, offer_payload_json, fetched_at)
		VALUES ($1,$2,'HAPAG','NO_OFFER','c2', '{"charges":[]}', now())`, org, rfq)
	require.Error(t, err, "NO_OFFER with payload must violate the CHECK")
	assert.Contains(t, err.Error(), "carrier_offers_payload_outcome_check")
}

// TestCarrierOffersUniqueDedupesNullCarrier: NULLS NOT DISTINCT makes
// (rfq_id, NULL carrier, request_hash) unique too — a re-run's
// NOT_APPLICABLE checkpoint collapses instead of duplicating.
func TestCarrierOffersUniqueDedupesNullCarrier(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "offers-uniq")
	rfq := seedOfferRFQ(t, db, org)

	stmt := `
		INSERT INTO app.carrier_offers
			(organization_id, rfq_id, carrier, outcome, request_hash, fetched_at)
		VALUES ($1,$2,NULL,'NOT_APPLICABLE','same-request', now())`
	require.NoError(t, offerExec(t, db, org, stmt, org, rfq))
	err := offerExec(t, db, org, stmt, org, rfq)
	require.Error(t, err, "NULL-carrier rows must dedupe on the request hash")
	assert.Contains(t, err.Error(), "carrier_offers_rfq_carrier_request_key")
	n := countRows(t, db, org,
		`SELECT count(*) FROM app.carrier_offers WHERE rfq_id = $1`, rfq.String())
	assert.Equal(t, 1, n)

	// A different request hash is a different attempt and does not collide.
	require.NoError(t, offerExec(t, db, org, `
		INSERT INTO app.carrier_offers
			(organization_id, rfq_id, carrier, outcome, request_hash, fetched_at)
		VALUES ($1,$2,NULL,'NOT_APPLICABLE','other-request', now())`, org, rfq))
}

func ptr[T any](v T) *T { return &v }
