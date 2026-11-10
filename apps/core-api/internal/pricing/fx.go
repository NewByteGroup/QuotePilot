package pricing

import (
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// fxResolver resolves pinned exchange rates per spec §6. Rates are
// looked up newest-first at or before their target date so a stale
// bulletin still prices and is flagged STALE_FX; a currency with no
// usable rate is recorded as missing and the caller raises
// MISSING_FX/MISSING_CUSTOMS_FX.
type fxResolver struct {
	rates       []FXRate // sorted by (source, currency, date)
	quoteDate   time.Time
	prevBizDay  time.Time
	spread      decimal.Decimal
	applied     []FXApplied
	appliedSeen map[string]struct{}
	missing     map[string]bool
	customsMiss bool
	ptaxUsed    bool
	customsUsed bool
}

func newFXResolver(in Input) *fxResolver {
	rates := make([]FXRate, len(in.FXRates))
	copy(rates, in.FXRates)
	sort.Slice(rates, func(i, j int) bool {
		return rateLess(rates[i].Source, rates[i].Currency, rates[i].Date,
			rates[j].Source, rates[j].Currency, rates[j].Date)
	})
	return &fxResolver{
		rates:       rates,
		quoteDate:   in.QuoteDate,
		prevBizDay:  prevBusinessDay(in.QuoteDate),
		spread:      in.FXSafetySpread,
		appliedSeen: make(map[string]struct{}),
		missing:     make(map[string]bool),
	}
}

// rateLess is the (source, currency, date) ordering shared by the raw
// rate table and the applied-rate audit view.
func rateLess(srcA, ccyA string, dA time.Time, srcB, ccyB string, dB time.Time) bool {
	if srcA != srcB {
		return srcA < srcB
	}
	if ccyA != ccyB {
		return ccyA < ccyB
	}
	return dA.Before(dB)
}

// resolve returns the effective rate (base × (1 + s_org) on the carrier
// path) and the flags to stamp on the line. ok is false when no usable
// rate exists; the currency is then recorded for MISSING_FX.
func (r *fxResolver) resolve(carrierPath bool, currency string) (decimal.Decimal, []string, bool) {
	if currency == "BRL" {
		return decimal.NewFromInt(1), nil, true
	}
	if carrierPath {
		return r.resolveCarrier(currency)
	}
	return r.resolvePTAX(currency)
}

// resolveCarrier implements "carrier-charged lines": carrier rate for
// QuoteDate times (1 + s_org), else PTAX previous business day sell
// times (1 + s_org) flagged FX_FALLBACK.
func (r *fxResolver) resolveCarrier(currency string) (decimal.Decimal, []string, bool) {
	if rate, ok := r.latest(FXSourceCarrier, currency, r.quoteDate); ok {
		r.record(rate, r.spread)
		return withSpread(rate.Rate, r.spread), staleFlag(rate, r.quoteDate), true
	}
	if rate, ok := r.latest(FXSourcePTAX, currency, r.prevBizDay); ok {
		r.ptaxUsed = true
		r.record(rate, r.spread)
		return withSpread(rate.Rate, r.spread), append(staleFlag(rate, r.quoteDate), FlagFXFallback), true
	}
	r.missing[currency] = true
	return decimal.Zero, nil, false
}

// resolvePTAX prices non-carrier lines: previous business day sell
// rate with no spread (spec §6: the spread protects carrier drift only).
func (r *fxResolver) resolvePTAX(currency string) (decimal.Decimal, []string, bool) {
	rate, ok := r.latest(FXSourcePTAX, currency, r.prevBizDay)
	if !ok {
		r.missing[currency] = true
		return decimal.Zero, nil, false
	}
	r.ptaxUsed = true
	r.record(rate, decimal.Zero)
	return rate.Rate, staleFlag(rate, r.quoteDate), true
}

// customs resolves the CUSTOMS rate for the goods value that feeds the
// import CIF base (Q6 default; CUSTOMS_RATE_UNCONFIRMED is recorded by
// the caller). Missing it is its own reason, not generic MISSING_FX.
func (r *fxResolver) customs(currency string) (decimal.Decimal, []string, bool) {
	if currency == "BRL" {
		return decimal.NewFromInt(1), nil, true
	}
	rate, ok := r.latest(FXSourceCustoms, currency, r.quoteDate)
	if !ok {
		r.customsMiss = true
		return decimal.Zero, nil, false
	}
	r.customsUsed = true
	r.record(rate, decimal.Zero)
	return rate.Rate, staleFlag(rate, r.quoteDate), true
}

// latest returns the newest rate for (source, currency) dated at or
// before maxDate. Newest-at-or-before makes a stale bulletin usable and
// STALE_FX meaningful; rates newer than the target are never touched.
func (r *fxResolver) latest(source, currency string, maxDate time.Time) (FXRate, bool) {
	var best FXRate
	found := false
	limit := maxDate.UTC().Truncate(24 * time.Hour)
	for _, rate := range r.rates {
		if rate.Source != source || rate.Currency != currency {
			continue
		}
		d := rate.Date.UTC().Truncate(24 * time.Hour)
		if d.After(limit) {
			continue
		}
		if !found || d.After(best.Date.UTC().Truncate(24*time.Hour)) {
			best, found = rate, true
		}
	}
	return best, found
}

// record appends to FXApplied deduped by the full applied tuple — the
// same (source, currency, date) can legitimately appear with and
// without the org spread (spec §6 carrier vs non-carrier paths).
func (r *fxResolver) record(rate FXRate, spread decimal.Decimal) {
	key := rate.Source + "|" + rate.Currency + "|" +
		rate.Date.UTC().Truncate(24*time.Hour).Format(time.RFC3339) + "|" +
		rate.Rate.String() + "|" + spread.String()
	if _, seen := r.appliedSeen[key]; seen {
		return
	}
	r.appliedSeen[key] = struct{}{}
	r.applied = append(r.applied, FXApplied{
		Source:   rate.Source,
		Currency: rate.Currency,
		Date:     rate.Date.UTC().Truncate(24 * time.Hour),
		Rate:     rate.Rate,
		Spread:   spread,
	})
}

// appliedRates returns the deduped applied rates sorted by (source,
// currency, date, spread) so no map order reaches the output. The
// spread is part of the key because the same PTAX rate legitimately
// appears with and without the org spread (carrier vs non-carrier).
func (r *fxResolver) appliedRates() []FXApplied {
	out := make([]FXApplied, len(r.applied))
	copy(out, r.applied)
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if rateLess(a.Source, a.Currency, a.Date, b.Source, b.Currency, b.Date) {
			return true
		}
		if rateLess(b.Source, b.Currency, b.Date, a.Source, a.Currency, a.Date) {
			return false
		}
		return a.Spread.Cmp(b.Spread) < 0
	})
	return out
}

// missingCurrencies returns sorted MISSING_FX reason currencies.
func (r *fxResolver) missingCurrencies() []string {
	out := make([]string, 0, len(r.missing))
	for c := range r.missing {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func withSpread(rate, spread decimal.Decimal) decimal.Decimal {
	return rate.Mul(decimal.NewFromInt(1).Add(spread))
}

// staleFlag appends STALE_FX when the applied rate is older than one
// business day before the quote date (spec §6).
func staleFlag(rate FXRate, quoteDate time.Time) []string {
	if businessDaysBetween(rate.Date, quoteDate) > 1 {
		return []string{FlagStaleFX}
	}
	return nil
}

// prevBusinessDay steps one business day back from d, skipping weekend
// days only — holidays need a calendar the MVP does not have (Q7,
// recorded as PTAX_PREV_BUSINESS_DAY_SELL).
func prevBusinessDay(d time.Time) time.Time {
	day := d.UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	for day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
		day = day.AddDate(0, 0, -1)
	}
	return day
}

// businessDaysBetween counts business days strictly after a up to b
// inclusive of b's own weekday; a is assumed <= b.
func businessDaysBetween(a, b time.Time) int {
	day := a.UTC().Truncate(24 * time.Hour)
	end := b.UTC().Truncate(24 * time.Hour)
	n := 0
	for day.Before(end) {
		day = day.AddDate(0, 0, 1)
		if day.Weekday() == time.Saturday || day.Weekday() == time.Sunday {
			continue
		}
		if !day.After(end) {
			n++
		}
	}
	return n
}
