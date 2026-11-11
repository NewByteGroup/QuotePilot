package inbox_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	svix "github.com/svix/svix-webhooks/go"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/inbox"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/inbox/inboxdb"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

var testLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// TestSpikeInTenantTxSqlcDecimal is gate criterion 3: InTenantTx plus one sqlc
// query end to end, with NUMERIC round-tripping to decimal.Decimal exactly and
// the transaction-local tenant context actually applied.
func TestSpikeInTenantTxSqlcDecimal(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()
	org := uuid.New()
	// inbound_messages.organization_id is a real FK since 0002: the org must
	// exist (seeded owner-side; qp_app cannot write Layer A).
	testenv.SeedOrganization(t, org, "inbox-decimal")
	amount := decimal.RequireFromString("1234.56")

	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := inboxdb.New(tx).InsertInboundMessage(ctx, inboxdb.InsertInboundMessageParams{
			OrganizationID:   pgtype.UUID{Bytes: org, Valid: true},
			ResendMessageID:  "msg-decimal-1",
			Subject:          "decimal roundtrip",
			DeclaredValueUsd: amount,
		})
		require.NoError(t, err)
		require.Equal(t, int64(1), rows, "first insert must report one row")

		var setting string
		require.NoError(t, tx.QueryRow(ctx,
			`SELECT current_setting('app.current_organization_id', true)`).Scan(&setting))
		require.Equal(t, org.String(), setting, "set_config must apply the tenant inside fn")

		got, err := inboxdb.New(tx).GetInboundMessage(ctx, inboxdb.GetInboundMessageParams{
			OrganizationID:  pgtype.UUID{Bytes: org, Valid: true},
			ResendMessageID: "msg-decimal-1",
		})
		require.NoError(t, err)
		assert.Equal(t, org.String(), uuid.UUID(got.OrganizationID.Bytes).String())
		assert.True(t, amount.Equal(got.DeclaredValueUsd),
			"NUMERIC roundtrip: want %s, got %s", amount, got.DeclaredValueUsd)
		return nil
	}))
}

// TestSpikeWebhook is gate criterion 5 plus the ADR-005 section 7 path: a
// valid Svix signature resolves the tenant from `to` through
// resolve_inbound_org, then inserts idempotently on
// (organization_id, resend_message_id); an invalid one is a 401 before any
// DB write.
func TestSpikeWebhook(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()

	secretBytes := []byte("spike-webhook-signing-secret")
	secret := "whsec_" + base64.StdEncoding.EncodeToString(secretBytes)
	verifier, err := inbox.NewSvixVerifier(secret)
	require.NoError(t, err)
	signer, err := svix.NewWebhook(secret)
	require.NoError(t, err)

	org := uuid.New()
	alias := testenv.SeedOrganization(t, org, "inbox-webhook") + "@in.test"

	srv := httptest.NewServer(inbox.NewServer(db, platform.NewStore(db.Pool()), verifier, testLogger))
	defer srv.Close()

	// The documented email.received envelope
	// (https://resend.com/docs/webhooks/emails/received): metadata only —
	// body and attachment bytes come from the Receiving/Attachments APIs.
	payload, err := json.Marshal(map[string]any{
		"type":       "email.received",
		"created_at": "2026-11-09T12:00:00.000Z",
		"data": map[string]any{
			"email_id":   "resend-spike-1",
			"created_at": "2026-11-09T11:59:58.000Z",
			"from":       "customer@example.com",
			"to":         []string{alias},
			"cc":         []string{},
			"bcc":        []string{},
			"message_id": "<spike-1@mail.example.com>",
			"subject":    "RFQ: 20 boxes Shenzhen",
			"attachments": []map[string]any{{
				"id":                  "att-1",
				"filename":            "rfq.pdf",
				"content_type":        "application/pdf",
				"content_disposition": "attachment",
			}},
		},
	})
	require.NoError(t, err)

	// 1. Valid signature: 200 and an insert.
	resp, body := postSigned(t, srv.URL+"/webhooks/resend/inbound", signer, "msg_first", payload)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	assert.Contains(t, body, `"inserted":true`)

	// 2. Replay the same business payload (fresh svix id): still 2xx, still 1 row.
	resp, body = postSigned(t, srv.URL+"/webhooks/resend/inbound", signer, "msg_replay", payload)
	require.Equal(t, http.StatusOK, resp.StatusCode, "body: %s", body)
	assert.Contains(t, body, `"inserted":false`)

	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		n, err := inboxdb.New(tx).CountInboundMessagesForOrg(ctx, pgtype.UUID{Bytes: org, Valid: true})
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "idempotent insert must leave exactly one row")
		return nil
	}))

	// 3. Tampered signature: 401 and no DB write at all.
	tampered, _ := postRaw(t, srv.URL+"/webhooks/resend/inbound", payload, map[string]string{
		"svix-id":        "msg_tampered",
		"svix-timestamp": strconv.FormatInt(time.Now().Unix(), 10),
		"svix-signature": "v1,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
	})
	assert.Equal(t, http.StatusUnauthorized, tampered.StatusCode)

	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		n, err := inboxdb.New(tx).CountInboundMessagesForOrg(ctx, pgtype.UUID{Bytes: org, Valid: true})
		require.NoError(t, err)
		require.Equal(t, int64(1), n, "rejected webhook must not write")
		return nil
	}))
}

func postSigned(t *testing.T, url string, signer *svix.Webhook, msgID string, payload []byte) (*http.Response, string) {
	t.Helper()
	ts := time.Now()
	sig, err := signer.Sign(msgID, ts, payload)
	require.NoError(t, err)
	return postRaw(t, url, payload, map[string]string{
		"svix-id":        msgID,
		"svix-timestamp": strconv.FormatInt(ts.Unix(), 10),
		"svix-signature": sig,
	})
}

func postRaw(t *testing.T, url string, payload []byte, headers map[string]string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(payload))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return resp, string(body)
}
