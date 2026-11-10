package pricing

import (
	"errors"
	"time"

	"github.com/shopspring/decimal"
)

// Price is the single entry point (spec §2). It turns one Input into a
// deterministic Output. The returned error is reserved for
// programmer/config problems (spec §4 step 1); data gaps surface as
// Output.Status = INCOMPLETE with machine-readable Reasons.
//
// The clock is injected so the package never reads time itself
// (architecture rule 5); every comparison today runs off the input's
// QuoteDate/PricedAt, so the clock is only required to be non-nil.
//
// Example:
//
//	out, err := Price(input, func() time.Time { return pricedAt })
//	// err != nil -> config bug; out.Status routes the RFQ to
//	// COMPLETE or NEEDS_REVIEW with out.Reasons for the gaps.
func Price(in Input, clock func() time.Time) (Output, error) {
	if clock == nil {
		return Output{}, errors.New("pricing: nil clock")
	}
	if err := in.validate(); err != nil {
		return Output{}, err
	}
	out := Output{Status: StatusComplete, InputsHash: hashInput(in)}
	g, early := requiredGroups(in)
	if early != nil {
		return earlyOutput(out, early), nil
	}
	return priceImport(in, g, out), nil
}

// earlyOutput renders EXPORT, non-MVP modals and sea-on-air as
// INCOMPLETE with their reason, and DDP as COMPLETE with zero lines.
func earlyOutput(out Output, e *earlyExit) Output {
	emptyCollections(&out)
	if e.ddp {
		out.Reasons = []Reason{}
		out.Assumptions = sortedSet(map[Assumption]struct{}{
			AssumptionDDPNothingPayable: {},
			AssumptionImportMatrixOnly:  {},
			AssumptionTaxesExcluded:     {},
			AssumptionNoInsuranceLine:   {},
		})
		return out
	}
	out.Status = StatusIncomplete
	out.Reasons = []Reason{e.reason}
	out.Assumptions = []Assumption{}
	return out
}

// emptyCollections normalizes an early-exit Output's slices to empty
// (never nil) so serialization is deterministic.
func emptyCollections(out *Output) {
	out.Lines = []Line{}
	out.Alternatives = []Line{}
	out.Warnings = []string{}
	out.FXApplied = []FXApplied{}
}

// assembly accumulates lines, audit collections and reasons while the
// calculation walks spec §4's order.
type assembly struct {
	in      Input
	fxr     *fxResolver
	lines   []Line
	alts    []Line
	reasons map[Reason]struct{}
	warns   map[string]struct{}
	assumps map[Assumption]struct{}
}

func (a *assembly) reason(r Reason) { a.reasons[r] = struct{}{} }
func (a *assembly) warn(w string)   { a.warns[w] = struct{}{} }
func (a *assembly) assume(s ...Assumption) {
	for _, x := range s {
		a.assumps[x] = struct{}{}
	}
}

// priceImport implements spec §4 steps 2-9 for the import matrix.
func priceImport(in Input, g *groups, out Output) Output {
	a := newAssembly(in)
	resolved := a.resolvedTariffs()
	verdict := assessOffer(in, g)
	if verdict.offer != nil && !verdict.ignored {
		a.assume(AssumptionOfferStrictCoverage)
	}
	a.offerWarnings(verdict)
	a.preCarriageSkips(verdict, g)
	freightWinners := a.buildFreight(g, resolved, verdict)
	a.buildPassThroughGroups(g, resolved, verdict)
	a.buildTerminal(g, freightWinners)
	a.finishReasonsAndAssumptions()
	return a.render(out)
}

// newAssembly wires an assembly with the always-true import
// assumptions (spec §9) already recorded.
func newAssembly(in Input) *assembly {
	a := &assembly{
		in:      in,
		fxr:     newFXResolver(in),
		reasons: make(map[Reason]struct{}),
		warns:   make(map[string]struct{}),
		assumps: make(map[Assumption]struct{}),
	}
	a.assume(AssumptionImportMatrixOnly, AssumptionTaxesExcluded, AssumptionNoInsuranceLine)
	if in.Modal == ModalAIR {
		a.assume(AssumptionAirFactor)
	}
	return a
}

// resolvedTariffs runs version resolution then drops out-of-MVP
// pre-carriage charge codes with a warning (spec §3 FCA note).
func (a *assembly) resolvedTariffs() []TariffCandidate {
	resolved := resolveTariffs(a.in.Tariffs, a.in.QuoteDate, a.in.PricedAt)
	kept := resolved[:0]
	for _, r := range resolved {
		if a.skipPreCarriage(r.ChargeCode) {
			continue
		}
		kept = append(kept, r)
	}
	return kept
}

// preCarriageSkips warns for the out-of-MVP inland/pre-carriage codes
// the other candidate paths drop silently (spec §3 FCA note): every
// charge of a usable offer and every org fixed fee scoped to an open
// group. A rejected offer never reaches a pool — its offer-level
// warning already says it was skipped — so only usable offers are
// scanned; a fixed fee in a closed group was never a candidate, so its
// skip must not warn.
func (a *assembly) preCarriageSkips(v offerVerdict, g *groups) {
	if v.usable != nil {
		for _, ch := range v.usable.Charges {
			a.skipPreCarriage(ch.Code)
		}
	}
	for _, f := range a.in.FixedFees {
		if groupOpen(g, f.Category) {
			a.skipPreCarriage(f.ChargeCode)
		}
	}
}

// groupOpen reports whether a fixed-fee category's cost group is open
// under the resolved matrix — the surcharge pool follows the freight
// payer's incoterm like the SURCHARGE groupSpec does.
func groupOpen(g *groups, cat Category) bool {
	switch cat {
	case CategoryIntlFreight, CategorySurcharge:
		return g.freight
	case CategoryOriginCharges:
		return g.origin
	case CategoryTerminalStorage:
		return g.terminal
	case CategoryCustomsDispatch:
		return g.customs
	}
	return false
}

// skipPreCarriage reports and warns when a charge code is out-of-MVP
// inland/pre-carriage — the shared guard for tariff rows, offer
// charges and fixed fees.
func (a *assembly) skipPreCarriage(code string) bool {
	if !isPreCarriage(code) {
		return false
	}
	a.warn("skipped out-of-MVP pre-carriage charge " + code)
	return true
}

// offerWarnings records why a supplied offer was not used (or how it
// replaced, via the OFFER_EXPIRED line flag set in selectFreight).
func (a *assembly) offerWarnings(v offerVerdict) {
	switch {
	case v.expired:
		a.warn("carrier offer expired or has no valid expiry; tariff candidates used instead")
	case v.incomplete:
		a.warn("carrier offer ignored: does not cover every required carrier group")
	case v.ignored:
		a.warn("carrier offer ignored: customer owes no FCL freight here")
	}
}

// buildFreight prices the INTL_FREIGHT group and returns the winning
// candidates — the CIF base needs the payable freight net (spec §4
// step 6).
func (a *assembly) buildFreight(g *groups, resolved []TariffCandidate, v offerVerdict) []candLine {
	if !g.freight {
		return nil
	}
	res := selectFreight(a.in, resolved, v, a.fxr)
	if res.offerFailed {
		a.warn("carrier offer could not be priced; tariff candidates used instead")
	}
	if res.missing {
		a.missingFreight(v, res)
		return nil
	}
	a.priceFreightWinners(res.winners)
	a.priceAlternates(res)
	// Fixed fees scoped to the freight group are pass-through charges
	// (e.g. a booking fee) — never part of the markup aggregate.
	a.emitPassFees(CategoryIntlFreight)
	return res.winners
}

// emitPassFees appends the fixed fees scoped to one category as
// pass-through lines.
func (a *assembly) emitPassFees(cat Category) {
	for _, c := range fixedFeeCands(cat, a.in.FixedFees, a.fxr) {
		a.emitPass(c)
	}
}

// missingFreight records the freight-gap reason, upgraded with
// NO_USABLE_OFFER when a supplied offer was part of the failure.
func (a *assembly) missingFreight(v offerVerdict, res freightResult) {
	a.reason(reasonMissingTariffGroup(freightMissingParam(a.in.Modal)))
	if v.offer != nil && (v.expired || v.incomplete || res.offerFailed) {
		a.reason(ReasonNoUsableOffer)
	}
}

// emitPass appends a pass-through line (sell = rounded cost, spec §5).
func (a *assembly) emitPass(c candLine) {
	a.lines = append(a.lines, toLine(c, passThroughSell(c)))
}

func freightMissingParam(m Modal) string {
	if m == ModalAIR {
		return string(ChargeAirFreight)
	}
	return string(CategoryIntlFreight)
}

// freightFloorBRL converts min_markup_usd x containers through USD's
// carrier-path rate (spec §5; identical to the line-currency formula
// for USD freight). A missing USD rate is a MISSING_FX data gap. The
// lookup's flags (e.g. FX_FALLBACK) are returned so the freight lines
// can carry them.
func (a *assembly) freightFloorBRL() (decimal.Decimal, []string) {
	// A nil or zero floor can never bind, so it resolves no USD rate —
	// otherwise a BRL-freight quote without USD rates would report a
	// spurious MISSING_FX:USD (PR15).
	if a.in.Modal != ModalFCL || a.in.MinMarkupUSD == nil || a.in.MinMarkupUSD.IsZero() {
		return decimal.Zero, nil
	}
	rate, flags, ok := a.fxr.resolve(true, "USD")
	if !ok {
		return decimal.Zero, nil
	}
	units := decimal.NewFromInt(int64(a.in.ContainerCount))
	return a.in.MinMarkupUSD.Mul(units).Mul(rate), flags
}

// priceFreightWinners computes the aggregate sell (FCL floor formula /
// AIR base markup) and distributes it across the winning lines (§7).
func (a *assembly) priceFreightWinners(winners []candLine) {
	floor, floorFlags := a.freightFloorBRL()
	weights := make([]decimal.Decimal, len(winners))
	for i, c := range winners {
		weights[i] = c.netBRL()
	}
	sells, bound := a.freightSells(weights, floor)
	if bound {
		// Only a bound floor earns its audit flags: FX_FALLBACK/STALE_FX
		// from the USD lookup are factually wrong on a line whose sell
		// never used them.
		for i := range winners {
			winners[i].flags = append(winners[i].flags, FlagMinMarkupApplied)
			winners[i].flags = append(winners[i].flags, floorFlags...)
		}
	}
	for i, sell := range sells {
		a.lines = append(a.lines, toLine(winners[i], sell))
	}
}

// freightAggregateSell is the group-level sell for one freight option
// before any per-line cent allocation: AIR marks up the summed
// chargeable bases, FCL applies the §5 floor formula.
func (a *assembly) freightAggregateSell(netBRL, floor decimal.Decimal) (decimal.Decimal, bool) {
	if a.in.Modal == ModalAIR {
		return airFreightSell(netBRL, a.in.MarkupRate), false
	}
	return fclFreightSell(netBRL, a.in.MarkupRate, floor)
}

// freightSells prices the winners' aggregate and splits it across the
// winning lines by cent allocation (spec §7).
func (a *assembly) freightSells(weights []decimal.Decimal, floor decimal.Decimal) ([]decimal.Decimal, bool) {
	total, bound := a.freightAggregateSell(sum(weights), floor)
	return allocateCents(total, weights), bound
}

// priceAlternates prices each losing freight row as a whole-option
// alternative for the audit view (Q5): its SellBRL is the freight
// aggregate the option would sell at if that row replaced the winner
// of its own charge code inside its own source pool — a tariff row
// within the tariff pool's per-code winners, an offer charge within
// the offer's — never a hybrid of offer winners plus tariff rows,
// which Q12 forbids. CostBRL stays the losing row's own net so the
// audit still shows what the row itself costs.
func (a *assembly) priceAlternates(res freightResult) {
	floor, floorFlags := a.freightFloorBRL()
	for _, c := range res.alternates {
		sell, bound := a.freightAggregateSell(sum(alternateWeights(res.poolWinners(c.src), c)), floor)
		if bound {
			// Bound alternates carry the floor's FX flags like winners.
			c.flags = append(c.flags, FlagMinMarkupApplied)
			c.flags = append(c.flags, floorFlags...)
		}
		a.alts = append(a.alts, toLine(c, sell))
	}
}

// poolWinners returns the per-code winners of one source pool — the
// baseline a losing row's alternative is priced against, so a tariff
// alternative never mixes offer lines into its option (Q12). Offer
// charges only ever win or fail whole today, so res.winners is the
// offer pool whenever an offer-sourced alternate exists.
func (r freightResult) poolWinners(src SourceType) []candLine {
	if src == SourcePrivateTariff {
		return r.tariffWinners
	}
	return r.winners
}

// alternateWeights is the net-BRL weight set behind one losing row's
// whole-option sell: the pool winners' nets without the row's own
// charge code — the row replaces that code's coverage, a no-op when no
// winner covers it — plus the losing row's own net.
func alternateWeights(winners []candLine, alt candLine) []decimal.Decimal {
	weights := make([]decimal.Decimal, 0, len(winners))
	for _, w := range winners {
		if w.code != alt.code {
			weights = append(weights, w.netBRL())
		}
	}
	return append(weights, alt.netBRL())
}

// groupSpec is one pass-through pool: the line category it feeds, the
// charge category its tariff/offer rows carry, whether the matrix opens
// it, and whether an empty pool is a gap. The surcharge pool opens only
// while the customer owes the freight — surcharges are the freight
// payer's charges — but it is never required.
type groupSpec struct {
	cat      Category
	chcat    ChargeCategory
	on       bool
	required bool
}

// buildPassThroughGroups prices the origin/customs/surcharge groups:
// resolved tariffs plus usable offer charges plus scoped fixed fees,
// cheapest per charge_code, every line selling at cost (spec §5).
func (a *assembly) buildPassThroughGroups(g *groups, resolved []TariffCandidate, v offerVerdict) {
	for _, s := range []groupSpec{
		{CategoryOriginCharges, ChargeOriginCharge, g.origin, true},
		{CategoryCustomsDispatch, ChargeCustoms, g.customs, true},
		{CategorySurcharge, ChargeSurcharge, g.freight, false},
	} {
		if s.on {
			a.buildGroup(s, resolved, v)
		}
	}
}

func (a *assembly) buildGroup(s groupSpec, resolved []TariffCandidate, v offerVerdict) {
	cands := tariffCandsFor(s.chcat, s.cat, a.in, resolved, a.fxr)
	if v.usable != nil {
		cands = append(cands, offerCandsFor(s.chcat, s.cat, v.usable, a.in, a.fxr)...)
	}
	res := collectGroup(cands)
	winners := append(res.winners, fixedFeeCands(s.cat, a.in.FixedFees, a.fxr)...)
	if len(winners) == 0 && s.required {
		a.missingGroup(s.cat, v)
	}
	for _, c := range winners {
		a.emitPass(c)
	}
	for _, c := range res.alternates {
		a.alts = append(a.alts, toLine(c, passThroughSell(c)))
	}
}

// missingGroup records an empty required pool. When a carrier offer was
// rejected outright and the missing group is one it could have covered
// (origin charges under EXW/FCA), NO_USABLE_OFFER explains the context.
func (a *assembly) missingGroup(cat Category, v offerVerdict) {
	a.reason(reasonMissingTariffGroup(string(cat)))
	if cat == CategoryOriginCharges && (v.expired || v.incomplete) {
		a.reason(ReasonNoUsableOffer)
	}
}

// finishReasonsAndAssumptions folds FX gaps and rate-path usage into
// the reason and assumption sets (spec §6, §9).
func (a *assembly) finishReasonsAndAssumptions() {
	for _, ccy := range a.fxr.missingCurrencies() {
		a.reason(reasonMissingFX(ccy))
	}
	if a.fxr.customsMiss {
		a.reason(ReasonMissingCustomsFX)
	}
	if a.fxr.ptaxUsed {
		a.assume(AssumptionPTAXPrevBusinessDay)
	}
	if a.fxr.customsUsed {
		a.assume(AssumptionCustomsRateUnconfirmed)
	}
}

// render sorts every collection, sums the rounded line amounts and sets
// the final status (spec §4 steps 8-9).
func (a *assembly) render(out Output) Output {
	out.Lines = sortLines(a.lines)
	out.Alternatives = sortLines(a.alts)
	out.FXApplied = a.fxr.appliedRates()
	out.Reasons = sortedSet(a.reasons)
	out.Warnings = sortedSet(a.warns)
	out.Assumptions = sortedSet(a.assumps)
	for _, l := range out.Lines {
		out.TotalCostBRL = out.TotalCostBRL.Add(l.CostBRL)
		out.FinalPriceBRL = out.FinalPriceBRL.Add(l.SellBRL)
	}
	if len(out.Reasons) > 0 {
		out.Status = StatusIncomplete
	}
	return out
}
