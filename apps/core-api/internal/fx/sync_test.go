package fx_test

// Real-Postgres tests for the fx_rates sync and load path (ADR-008: DB
// behaviour is proven against the real role model, never mocked).
// fx_rates is platform-global, so the plain qp_app pool is the honest
// executor — the same one the production worker uses.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/fx"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// syncFixture is the canned PTAX page the httptest server serves; dates
// are fixed so repeated runs against the shared dev database pin the
// same (source, currency, rate_date) keys with the same values —
// ON CONFLICT DO NOTHING keeps runs deterministic.
const syncFixture = `{"value":[
  {"cotacaoVenda":5.31000,"dataHoraCotacao":"2025-11-10 10:06:26.657"},
  {"cotacaoVenda":5.32010,"dataHoraCotacao":"2025-11-10 11:09:24.834"},
  {"cotacaoVenda":5.34920,"dataHoraCotacao":"2025-11-10 13:10:31.898"},
  {"cotacaoVenda":5.35670,"dataHoraCotacao":"2025-11-10 13:10:31.908"},
  {"cotacaoVenda":5.29520,"dataHoraCotacao":"2025-11-14 13:11:28.202"}
]}`

func ptaxServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, syncFixture)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEnsurePTAXUpsertsAndIsIdempotent: the sync pins one row per
// bulletin date under each requested currency, keeps the closing
// bulletin, and a second run changes nothing (first-seen wins — the pin
// is immutable by rule 9).
func TestEnsurePTAXUpsertsAndIsIdempotent(t *testing.T) {
	db := testenv.Open(t)
	srv := ptaxServer(t)
	syncer := fx.NewSyncer(db.Pool(),
		fx.NewFetcher(fx.WithHTTPClient(srv.Client()), fx.WithBaseURL(srv.URL)))
	ctx := context.Background()
	since := time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC)
	until := time.Date(2025, 11, 30, 0, 0, 0, 0, time.UTC)

	require.NoError(t, syncer.EnsurePTAX(ctx, []string{"USD"}, since, until))
	require.NoError(t, syncer.EnsurePTAX(ctx, []string{"USD"}, since, until),
		"a re-sync is a no-op, not an error")

	var n int
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM app.fx_rates
		 WHERE source = 'PTAX' AND currency = 'USD' AND rate_date IN ('2025-11-10','2025-11-14')`,
	).Scan(&n))
	assert.Equal(t, 2, n, "one pinned row per bulletin date")

	var rate decimal.Decimal
	var ref string
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT rate, coalesce(source_ref,'') FROM app.fx_rates
		 WHERE source = 'PTAX' AND currency = 'USD' AND rate_date = '2025-11-10'`,
	).Scan(&rate, &ref))
	assert.Equal(t, "5.3567", rate.String(), "the closing bulletin wins the pin")
	assert.Contains(t, ref, "13:10:31.908", "source_ref names the winning bulletin")
}

// TestEnsurePTAXSkipsBRL: BRL resolves to 1 inside the engine — no
// fetch, no row.
func TestEnsurePTAXSkipsBRL(t *testing.T) {
	db := testenv.Open(t)
	srv := ptaxServer(t)
	syncer := fx.NewSyncer(db.Pool(),
		fx.NewFetcher(fx.WithHTTPClient(srv.Client()), fx.WithBaseURL(srv.URL)))
	require.NoError(t, syncer.EnsurePTAX(context.Background(), []string{"BRL"},
		time.Now(), time.Now()))
	var n int
	require.NoError(t, db.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM app.fx_rates WHERE currency = 'BRL'`).Scan(&n))
	assert.Equal(t, 0, n)
}

// TestEnsurePTAXRejectsBadCurrency: a malformed key is refused before
// the request is built — the CHECK would reject it anyway, but the
// error belongs here, not mid-transaction.
func TestEnsurePTAXRejectsBadCurrency(t *testing.T) {
	db := testenv.Open(t)
	srv := ptaxServer(t)
	syncer := fx.NewSyncer(db.Pool(),
		fx.NewFetcher(fx.WithHTTPClient(srv.Client()), fx.WithBaseURL(srv.URL)))
	err := syncer.EnsurePTAX(context.Background(), []string{"us"}, time.Now(), time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), `currency "us"`)
}

// TestLoadRatesWindow: all three sources come back for the requested
// currencies inside the window; rows older than since stay out and
// unrequested currencies are never read. Seeds use exotic codes so the
// shared table's PTAX rows cannot interfere.
func TestLoadRatesWindow(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()

	for _, row := range []struct{ source, ccy, rate, date string }{
		{"PTAX", "CHF", "6.100000", "2025-11-10"},
		{"PTAX", "CHF", "6.200000", "2025-11-14"},
		{"PTAX", "CHF", "9.900000", "2025-10-01"}, // before the window
		{"CUSTOMS", "CHF", "6.150000", "2025-11-12"},
		{"CARRIER", "CHF", "6.500000", "2025-11-13"},
		{"PTAX", "JPY", "0.035000", "2025-11-13"},
	} {
		_, err := db.Pool().Exec(ctx, `
			INSERT INTO app.fx_rates (source, currency, rate, rate_date)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (source, currency, rate_date) DO NOTHING`,
			row.source, row.ccy, row.rate, row.date)
		require.NoError(t, err)
	}

	rates, err := fx.LoadRates(ctx, db.Pool(), []string{"CHF"},
		time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)

	got := make(map[string]string, len(rates))
	for _, r := range rates {
		assert.Equal(t, "CHF", r.Currency)
		got[r.Source+"@"+r.Date.Format("2006-01-02")] = r.Rate.String()
	}
	assert.Equal(t, map[string]string{
		"PTAX@2025-11-10":    "6.1",
		"PTAX@2025-11-14":    "6.2",
		"CUSTOMS@2025-11-12": "6.15",
		"CARRIER@2025-11-13": "6.5",
	}, got, "every source inside the window, nothing older, nothing for JPY")

	empty, err := fx.LoadRates(ctx, db.Pool(), nil, time.Now())
	require.NoError(t, err)
	assert.Empty(t, empty, "no currencies means no query")
}

// TestLoadRatesFeedsTheEngine: the loaded shape is the pricing.FXRate
// the resolver consumes — the (source, currency, date, rate) tuple is
// all it needs.
func TestLoadRatesFeedsTheEngine(t *testing.T) {
	db := testenv.Open(t)
	srv := ptaxServer(t)
	syncer := fx.NewSyncer(db.Pool(),
		fx.NewFetcher(fx.WithHTTPClient(srv.Client()), fx.WithBaseURL(srv.URL)))
	ctx := context.Background()
	require.NoError(t, syncer.EnsurePTAX(ctx, []string{"EUR"},
		time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2025, 11, 30, 0, 0, 0, 0, time.UTC)))

	rates, err := fx.LoadRates(ctx, db.Pool(), []string{"EUR"},
		time.Date(2025, 11, 1, 0, 0, 0, 0, time.UTC))
	require.NoError(t, err)
	require.NotEmpty(t, rates)
	for _, r := range rates {
		assert.Equal(t, pricing.FXSourcePTAX, r.Source)
		assert.Equal(t, "EUR", r.Currency)
		assert.True(t, r.Rate.IsPositive())
		assert.False(t, r.Date.IsZero())
	}
}
