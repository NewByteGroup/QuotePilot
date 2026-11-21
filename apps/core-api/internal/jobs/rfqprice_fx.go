package jobs

// rfqprice_fx.go — S3b's FX phases around the single tenant tx (spec §6,
// migration 0011):
//
//   A. a lock-free InTenantReadTx peeks at the RFQ and derives which
//      currencies the run can need;
//   B. outside any tx, the bound FXSyncer tops app.fx_rates up from the
//      BCB PTAX feed — slow or dead BCB degrades to the engine's own
//      MISSING_FX park instead of burning a River attempt;
//   C. inside the tenant tx, LoadRates pins the window into
//      Input.FXRates and the commit path derives quotes.cargo_cif_brl.

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
)

const (
	// fxWindowDays bounds the phase-B fetch and the phase-C load. PTAX
	// publishes on business days only, so 14 calendar days cover ~10
	// bulletins — enough for the engine's previous-business-day rule
	// with holiday slack, small enough for one OData page.
	fxWindowDays = 14
	// fxEnsureBudget caps phase B inside the 5s job budget: a BCB that
	// stalls past this degrades to an honest MISSING_FX park rather
	// than consuming the attempt's deadline (spec §3).
	fxEnsureBudget = 3 * time.Second
)

// FXSyncer is phase B's seam — *fx.Syncer in production. Tests inject
// fakes to pin the failure contract: any error degrades to a warn and
// the job continues, never a retry.
type FXSyncer interface {
	EnsurePTAX(ctx context.Context, currencies []string, since, until time.Time) error
}

// alwaysFetchCurrencies warms the currencies the extraction cannot see:
// tariff and offer payloads are DB-side inputs, and for this product
// they are overwhelmingly USD/EUR — pinning their PTAX before the
// tenant tx is cheap insurance against MISSING_FX on an otherwise
// complete quote. A currency outside this set plus the three extracted
// currency fields parks honestly until its own feed exists.
var alwaysFetchCurrencies = []string{"EUR", "USD"}

// peekFXCurrencies is phase A: a lock-free tenant read that turns the
// persisted extraction into the currency set phase B warms. Every
// failure mode returns nil — a missing or non-PRICING RFQ is the main
// path's no-op, an undecodable payload its deterministic cancel — so
// the warm-up simply never ran. A read error is warned and skipped the
// same way: the write tx re-reads under the lock anyway.
func (w *RfqPriceWorker) peekFXCurrencies(ctx context.Context, args PriceArgs) []string {
	var needed []string
	err := w.Runner.InTenantReadTx(ctx, args.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		var status string
		var payload []byte
		err := tx.QueryRow(ctx, `
			SELECT status::text, coalesce(extracted_payload_json::text,'')
			FROM app.rfq_requests WHERE id = $1`, args.RfqID).Scan(&status, &payload)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if status != rfqPricing {
			return nil
		}
		fields, err := decodePriceExtraction(payload)
		if err != nil {
			return nil
		}
		needed = neededCurrencies(fields)
		return nil
	})
	if err != nil {
		slog.WarnContext(ctx, "jobs: rfq_price: currency peek failed; FX warm-up skipped",
			"rfq_id", args.RfqID, "error", err)
		return nil
	}
	return needed
}

// neededCurrencies is the union phase B fetches: the three extracted
// currency fields (BRL needs no rate, malformed values are the main
// path's park not ours) plus the USD/EUR constants. Sorted so the order
// is deterministic — the same rule the engine applies to its outputs.
func neededCurrencies(fields map[string]extractedField) []string {
	set := make(map[string]struct{}, len(alwaysFetchCurrencies)+3)
	for _, c := range alwaysFetchCurrencies {
		set[c] = struct{}{}
	}
	for _, key := range []string{
		"cargo_value_currency", "declared_freight_currency", "declared_insurance_currency",
	} {
		s, ok := fieldString(fields, key)
		if !ok {
			continue
		}
		s = strings.ToUpper(s)
		if s == "BRL" || !currencyPattern.MatchString(s) {
			continue
		}
		set[s] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// warmFXRates is phase B: it runs outside every transaction because a
// remote call must never hold a tenant tx open. Any error — timeout,
// BCB down, a write refusal — is warned and swallowed: the engine parks
// MISSING_FX honestly and the attempt is not burned. No tenant content
// is logged (IDs and counts only).
func (w *RfqPriceWorker) warmFXRates(ctx context.Context, currencies []string) {
	if w.FX == nil || len(currencies) == 0 {
		return
	}
	now := w.clock()
	ensureCtx, cancel := context.WithTimeout(ctx, fxEnsureBudget)
	defer cancel()
	if err := w.FX.EnsurePTAX(ensureCtx, currencies, now.AddDate(0, 0, -fxWindowDays), now); err != nil {
		slog.WarnContext(ctx, "jobs: rfq_price: PTAX warm-up failed; pricing continues and may park MISSING_FX",
			"currencies", len(currencies), "error", err)
	}
}

// loadFXWindow is the LoadRates "since": min(QuoteDate, PricedAt) minus
// the window. A ready_date ahead of the run needs rates that exist today
// (the engine takes newest-at-or-before, so nothing younger applies);
// one in the past needs the rates that were pinned then — the earlier of
// the two dates covers both.
func loadFXWindow(in pricing.Input) time.Time {
	anchor := in.QuoteDate
	if in.PricedAt.Before(anchor) {
		anchor = in.PricedAt
	}
	return anchor.AddDate(0, 0, -fxWindowDays)
}

// cargoCIFBRL computes the quote's declared CIF in BRL — goods value,
// declared freight and declared insurance — each component converted at
// the newest CUSTOMS rate at or before QuoteDate. This mirrors the
// engine's own CIF base (spec §6: every CIF component converts with the
// customs rate, and a zero-valued component is a known zero resolving no
// rate), deliberately NOT the carrier path: the column would otherwise
// disagree with the band-selection base the engine computed.
//
// ok is false — the column stays NULL — when any component is absent or
// its rate unresolvable. Never fabricate: NULL means "not computable",
// which is the only value an audit can trust. The engine uses the
// payable freight net instead of the declared freight when freight is
// owed; that net lives inside Output internals this helper does not
// reconstruct, so freight-owed quotes keep NULL here unless all three
// declared inputs exist (flagged on the PR).
func cargoCIFBRL(in pricing.Input) (decimal.Decimal, bool) {
	if in.CargoValue.IsZero() || in.DeclaredFreight == nil || in.DeclaredInsurance == nil {
		return decimal.Zero, false
	}
	total := decimal.Zero
	for _, m := range []pricing.Money{
		{Amount: in.CargoValue, Currency: in.CargoCurrency},
		*in.DeclaredFreight,
		*in.DeclaredInsurance,
	} {
		conv, ok := customsBRL(m, in.FXRates, in.QuoteDate)
		if !ok {
			return decimal.Zero, false
		}
		total = total.Add(conv)
	}
	return total.Round(2), true
}

// customsBRL converts one component at the pinned CUSTOMS rate; BRL and
// known-zero components resolve no rate (engine cifAcc.add).
func customsBRL(m pricing.Money, rates []pricing.FXRate, quoteDate time.Time) (decimal.Decimal, bool) {
	if m.Amount.IsZero() || m.Currency == "BRL" {
		return m.Amount, true
	}
	r, ok := latestRate(rates, pricing.FXSourceCustoms, m.Currency, quoteDate)
	if !ok {
		return decimal.Zero, false
	}
	return m.Amount.Mul(r.Rate), true
}

// latestRate mirrors the engine resolver's newest-at-or-before pick: a
// stale bulletin still applies (the engine flags it STALE_FX on its
// side), a rate newer than the target is never touched.
func latestRate(rates []pricing.FXRate, source, currency string, maxDate time.Time) (pricing.FXRate, bool) {
	var best pricing.FXRate
	found := false
	limit := maxDate.UTC().Truncate(24 * time.Hour)
	for _, r := range rates {
		if r.Source != source || r.Currency != currency {
			continue
		}
		d := r.Date.UTC().Truncate(24 * time.Hour)
		if d.After(limit) {
			continue
		}
		if !found || d.After(best.Date.UTC().Truncate(24*time.Hour)) {
			best, found = r, true
		}
	}
	return best, found
}
