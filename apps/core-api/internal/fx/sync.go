package fx

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
)

// Executor is the Exec subset pgx.Tx and *pgxpool.Pool share — phase B
// writes through the plain qp_app pool outside any tenant transaction
// (fx_rates is platform-global, no RLS), while tests may hand a tx.
type Executor interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
}

// Querier is the Query subset pgx.Tx and *pgxpool.Pool share — phase C
// loads rates inside the tenant transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Syncer pairs the bulletin feed with the rate table: fetch from BCB,
// upsert into app.fx_rates. Production wires the qp_app pool and the
// real endpoint; tests swap either half.
type Syncer struct {
	fetcher *Fetcher
	exec    Executor
}

// NewSyncer binds a Fetcher to the executor EnsurePTAX writes through.
//
//	syncer := fx.NewSyncer(db.Pool(), fx.NewFetcher())
func NewSyncer(exec Executor, f *Fetcher) *Syncer {
	return &Syncer{fetcher: f, exec: exec}
}

// EnsurePTAX tops up app.fx_rates with the PTAX sell rate of every
// non-BRL currency in the window [since, until] — one HTTP call per
// currency. The (source, currency, rate_date) pin is immutable (rule
// 9), so the upsert is ON CONFLICT DO NOTHING: first-seen wins and a
// re-run is a no-op — idempotent by construction. BRL needs no rate
// (the engine resolves 1). The first error aborts; the caller decides
// whether a BCB outage degrades or retries.
func (s *Syncer) EnsurePTAX(ctx context.Context, currencies []string, since, until time.Time) error {
	for _, ccy := range currencies {
		if ccy == "BRL" {
			continue
		}
		if !currencyShape.MatchString(ccy) {
			return fmt.Errorf("fx: ptax: currency %q: expected [A-Z]{3}", ccy)
		}
		rows, err := s.fetcher.bulletins(ctx, ccy, since, until)
		if err != nil {
			return err
		}
		if err := s.upsert(ctx, ccy, rows); err != nil {
			return fmt.Errorf("fx: ptax %s upsert: %w", ccy, err)
		}
	}
	return nil
}

// upsert inserts each bulletin under its pin key, leaving the existing
// row — the rate earlier quotes pinned — untouched.
func (s *Syncer) upsert(ctx context.Context, currency string, rows []bulletin) error {
	for _, b := range rows {
		ref := "bcb-ptax " + b.at.Format(ptaxStampLayout)
		if _, err := s.exec.Exec(ctx, `
			INSERT INTO app.fx_rates (source, currency, rate, rate_date, source_ref)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (source, currency, rate_date) DO NOTHING`,
			pricing.FXSourcePTAX, currency, b.rate, b.day, ref); err != nil {
			return err
		}
	}
	return nil
}

// LoadRates returns every pinned rate for currencies dated at or after
// since — all three sources, newest included. The engine's resolver
// picks newest-at-or-before its own target, so supplying a slightly
// wider window than needed is safe; a currency absent here is the
// engine's MISSING_FX signal, not an error.
func LoadRates(ctx context.Context, q Querier, currencies []string, since time.Time) ([]pricing.FXRate, error) {
	if len(currencies) == 0 {
		return nil, nil
	}
	rows, err := q.Query(ctx, `
		SELECT source, currency, rate, rate_date
		FROM app.fx_rates
		WHERE currency = ANY($1) AND rate_date >= $2::date`,
		currencies, since.Format(isoDate))
	if err != nil {
		return nil, fmt.Errorf("fx: load rates: %w", err)
	}
	defer rows.Close()
	var out []pricing.FXRate
	for rows.Next() {
		var r pricing.FXRate
		if err := rows.Scan(&r.Source, &r.Currency, &r.Rate, &r.Date); err != nil {
			return nil, fmt.Errorf("fx: load rates scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("fx: load rates: %w", err)
	}
	return out, nil
}
