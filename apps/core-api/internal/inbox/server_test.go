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

const validBody = `{"to":["a@in.test"],"resend_message_id":"m1","declared_value_usd":"1"}`

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
	body := `{"to":["a@in.test","b@in.test"],"resend_message_id":"m1","declared_value_usd":"1"}`
	rec, runner := post(t, resolver, body)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "abort-message", rec.Header().Get("webhook-delivery"))
	assert.False(t, runner.called, "a multi-tenant message must not be guessed")
}

// TestInboundSameOrgTwice dedupes recipients: the same org reached through
// two addresses is one tenant, so the message stores once.
func TestInboundSameOrgTwice(t *testing.T) {
	org := uuid.New()
	body := `{"to":["a@in.test","a@in.test"],"resend_message_id":"m1","declared_value_usd":"1"}`
	rec, runner := post(t, okResolver(org), body)
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

// TestInboundValidation covers the parse gates that must die before any DB
// touch: empty/missing recipient list, blank entries, missing message id,
// negative amount, malformed JSON.
func TestInboundValidation(t *testing.T) {
	bodies := map[string]string{
		"no to":        `{"resend_message_id":"m1","declared_value_usd":"1"}`,
		"empty to":     `{"to":[],"resend_message_id":"m1","declared_value_usd":"1"}`,
		"blank entry":  `{"to":["  "],"resend_message_id":"m1","declared_value_usd":"1"}`,
		"no msg id":    `{"to":["a@in.test"],"declared_value_usd":"1"}`,
		"negative amt": `{"to":["a@in.test"],"resend_message_id":"m1","declared_value_usd":"-1"}`,
		"bad json":     `{nope`,
	}
	for name, body := range bodies {
		rec, runner := post(t, okResolver(uuid.New()), body)
		require.Equal(t, http.StatusBadRequest, rec.Code, name)
		assert.False(t, runner.called, name)
	}
}
