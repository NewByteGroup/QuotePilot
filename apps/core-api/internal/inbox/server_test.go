package inbox_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/inbox"
)

// resolverFunc adapts a function to the OrgResolver seam so each test pins
// exactly the matches it wants.
type resolverFunc func(ctx context.Context, recipient string) (uuid.UUID, bool, error)

func (f resolverFunc) ResolveInboundOrg(ctx context.Context, r string) (uuid.UUID, bool, error) {
	return f(ctx, r)
}

// acceptAll verifier: unit tests exercise resolution and dropping, not Svix
// (TestSpikeWebhook owns signature coverage end to end).
type acceptAll struct{}

func (acceptAll) Verify([]byte, http.Header) error { return nil }

// spyRunner records the tenant it would open and never runs fn — the drop
// tests prove the handler never reaches the tenant transaction, and the
// routing test pins the resolved org without a real insert.
type spyRunner struct {
	called bool
	org    uuid.UUID
}

func (r *spyRunner) InTenantTx(_ context.Context, org uuid.UUID, _ func(context.Context, pgx.Tx) error) error {
	r.called = true
	r.org = org
	return nil
}

// InTenantReadTx is unused on the inbox path; it only satisfies the
// TenantRunner seam.
func (r *spyRunner) InTenantReadTx(_ context.Context, _ uuid.UUID, _ func(context.Context, pgx.Tx) error) error {
	return nil
}

// post fires a body at a fresh server built on the given resolver and
// returns the recorder plus the runner spy for assertions.
func post(t *testing.T, resolver inbox.OrgResolver, body string) (*httptest.ResponseRecorder, *spyRunner) {
	t.Helper()
	runner := &spyRunner{}
	srv := inbox.NewServer(runner, resolver, acceptAll{}, testLogger)
	req := httptest.NewRequest("POST", "/webhooks/resend/inbound", strings.NewReader(body))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec, runner
}

// okResolver answers the same org for every recipient.
func okResolver(org uuid.UUID) resolverFunc {
	return func(context.Context, string) (uuid.UUID, bool, error) { return org, true, nil }
}

// eventBody wraps a `data` object in the documented email.received envelope
// (https://resend.com/docs/webhooks/emails/received) so each test writes
// only the fields it exercises.
func eventBody(data string) string {
	return `{"type":"email.received","created_at":"2026-11-09T12:00:00.000Z","data":` + data + `}`
}

var validBody = eventBody(`{"email_id":"m1","created_at":"2026-11-09T11:59:58.000Z","from":"customer@example.com","to":["a@in.test"],"subject":"RFQ: 20 boxes Shenzhen","attachments":[{"id":"att-1","filename":"rfq.pdf","content_type":"application/pdf","content_disposition":"attachment"}]}`)

// TestInboundSingleOrgRoutes proves the happy path reaches the tenant
// transaction with the resolved organization — the insert itself is covered
// by TestSpikeWebhook on real Postgres.
func TestInboundSingleOrgRoutes(t *testing.T) {
	org := uuid.New()
	rec, runner := post(t, okResolver(org), validBody)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, runner.called, "tenant tx must be opened for the resolved org")
	require.Equal(t, org, runner.org)
}

// TestInboundNoMatchDrops pins ADR-005 section 7: no org answers 2xx with
// the abort header and never opens a tenant transaction — a non-2xx would
// make Svix replay an undeliverable message for ~27h.
func TestInboundNoMatchDrops(t *testing.T) {
	miss := func(context.Context, string) (uuid.UUID, bool, error) { return uuid.Nil, false, nil }
	rec, runner := post(t, resolverFunc(miss), validBody)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"inserted":false,"dropped":true}`, rec.Body.String())
	assert.Equal(t, "abort-message", rec.Header().Get("webhook-delivery"))
	assert.False(t, runner.called, "a no-match message must not open a tenant tx")
}

// TestInboundMultiOrgDrops pins the anti-guess rule: two recipients owned by
// two organizations quarantine the message instead of picking one.
func TestInboundMultiOrgDrops(t *testing.T) {
	orgs := map[string]uuid.UUID{"a@in.test": uuid.New(), "b@in.test": uuid.New()}
	resolver := resolverFunc(func(_ context.Context, r string) (uuid.UUID, bool, error) {
		return orgs[r], true, nil
	})
	body := eventBody(`{"email_id":"m1","created_at":"2026-11-09T11:59:58.000Z","to":["a@in.test","b@in.test"]}`)
	rec, runner := post(t, resolver, body)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "abort-message", rec.Header().Get("webhook-delivery"))
	assert.False(t, runner.called, "a multi-tenant message must not be guessed")
}

// TestInboundSameOrgTwice dedupes at the org level: two distinct addresses
// that resolve to the same tenant are one match, so the message stores.
func TestInboundSameOrgTwice(t *testing.T) {
	org := uuid.New()
	resolver := resolverFunc(func(_ context.Context, r string) (uuid.UUID, bool, error) {
		return org, strings.HasSuffix(r, "@in.test"), nil
	})
	body := eventBody(`{"email_id":"m1","created_at":"2026-11-09T11:59:58.000Z","to":["a@in.test","b@in.test"]}`)
	rec, runner := post(t, resolver, body)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, runner.called)
	require.Equal(t, org, runner.org)
}

// TestInboundPartialMatchStores pins ADR-005 section 7's "exactly one
// organization matches": one resolvable plus one unresolvable recipient is
// a single tenant — the message stores, it does not drop.
func TestInboundPartialMatchStores(t *testing.T) {
	org := uuid.New()
	resolver := resolverFunc(func(_ context.Context, r string) (uuid.UUID, bool, error) {
		return org, r == "a@in.test", nil
	})
	body := eventBody(`{"email_id":"m1","created_at":"2026-11-09T11:59:58.000Z","to":["a@in.test","ghost@in.test"]}`)
	rec, runner := post(t, resolver, body)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, runner.called)
	require.Equal(t, org, runner.org)
}

// TestInboundResolverError is the one differently-shaped answer: a platform
// lookup failure is a server error (500), not information about the tenant.
func TestInboundResolverError(t *testing.T) {
	broken := func(context.Context, string) (uuid.UUID, bool, error) {
		return uuid.Nil, false, errors.New("pool exhausted")
	}
	rec, _ := post(t, resolverFunc(broken), validBody)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
}

// TestInboundMinimalPayload proves the handler tolerates absent fields it
// never reads: the handler's minimum — `type` plus `data.created_at`,
// `data.to` and `data.email_id` — still resolves and routes into the
// tenant transaction even though the schema also requires `from` and
// `subject` (required there documents Resend's guarantee for consumers;
// it is not what this handler enforces).
func TestInboundMinimalPayload(t *testing.T) {
	org := uuid.New()
	body := `{"type":"email.received","data":{"email_id":"m-min","created_at":"2026-11-09T11:59:58.000Z","to":["a@in.test"]}}`
	rec, runner := post(t, okResolver(org), body)
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, runner.called)
	require.Equal(t, org, runner.org)
}

// TestInboundValidation covers the parse gates that must die before any DB
// touch: a wrong or missing event type (a misconfigured endpoint pointing
// other Resend events at this route fails fast), missing created_at,
// empty/missing recipient list, blank entries, missing email id, malformed
// JSON. Every one is permanent for that body, so the abort header must be
// set — Svix retries would replay the same invalid payload for ~27h.
func TestInboundValidation(t *testing.T) {
	bodies := map[string]string{
		"wrong type":  `{"type":"email.delivered","created_at":"2026-11-09T12:00:00.000Z","data":{"email_id":"m1","to":["a@in.test"]}}`,
		"no type":     `{"created_at":"2026-11-09T12:00:00.000Z","data":{"email_id":"m1","to":["a@in.test"]}}`,
		"no created":  eventBody(`{"email_id":"m1","to":["a@in.test"]}`),
		"no to":       eventBody(`{"email_id":"m1","created_at":"2026-11-09T11:59:58.000Z"}`),
		"empty to":    eventBody(`{"email_id":"m1","created_at":"2026-11-09T11:59:58.000Z","to":[]}`),
		"blank entry": eventBody(`{"email_id":"m1","created_at":"2026-11-09T11:59:58.000Z","to":["  "]}`),
		"no email id": eventBody(`{"created_at":"2026-11-09T11:59:58.000Z","to":["a@in.test"]}`),
		"too many to": eventBody(`{"email_id":"m1","created_at":"2026-11-09T11:59:58.000Z","to":[` + strings.Repeat(`"a@in.test",`, 51) + `"a@in.test"]}`),
		"bad json":    `{nope`,
	}
	for name, body := range bodies {
		rec, runner := post(t, okResolver(uuid.New()), body)
		require.Equal(t, http.StatusBadRequest, rec.Code, name)
		assert.Equal(t, "abort-message", rec.Header().Get("webhook-delivery"), name)
		assert.False(t, runner.called, name)
	}
}
