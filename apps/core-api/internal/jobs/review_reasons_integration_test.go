package jobs

// Stored-value guard for rfq_requests.review_reason (spec
// 2026-11-11-staged-quote-pipeline S3b): the column is plain text with no
// CHECK, so this real-Postgres scan is the only enforcement that persisted
// codes stay inside the ReviewReasons list. Any writer that lands a string
// literal outside the vocabulary fails here, not in production.

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

func TestStoredReviewReasonsInClosedList(t *testing.T) {
	db := testenv.Open(t)
	org := uuid.New()
	testenv.SeedOrganization(t, org, "review-reasons")

	ctx := context.Background()
	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		// Store every code through the constant — the same write shape the
		// parking workers use — so the list itself is proven storable.
		for _, reason := range ReviewReasons {
			_, err := tx.Exec(ctx,
				`INSERT INTO app.rfq_requests
					(organization_id, resend_message_id, customer_email, status, review_reason)
				 VALUES ($1, $2, $3, 'NEEDS_REVIEW', $4)`,
				org, "review-reason-"+string(reason), "customer@example.com", string(reason))
			require.NoError(t, err)
		}

		// The sentinel: whatever the suite stored must be inside the closed
		// list — a rogue literal at any call site trips this assertion.
		rows, err := tx.Query(ctx,
			`SELECT DISTINCT review_reason FROM app.rfq_requests WHERE review_reason IS NOT NULL`)
		require.NoError(t, err)
		stored, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)

		listed := make(map[ReviewReason]bool, len(ReviewReasons))
		for _, r := range ReviewReasons {
			listed[r] = true
		}
		for _, value := range stored {
			require.True(t, listed[ReviewReason(value)],
				"stored review_reason %q is not in the ReviewReasons list", value)
		}
		return nil
	}))
}
