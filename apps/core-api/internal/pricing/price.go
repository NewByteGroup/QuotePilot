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
		if isPreCarriage(r.ChargeCode) {
			a.warn("skipped out-of-MVP pre-carriage charge " + r.ChargeCode)
			continue
		}
		kept = append(kept, r)
	}
	return kept
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
	a.priceAlternates(res.alternates)
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
	if a.in.Modal != ModalFCL || a.in.MinMarkupUSD == nil {
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

// freightSells prices the winners' aggregate: AIR marks up the summed
// chargeable bases; FCL applies the §5 floor formula. Either way the
// aggregate sell splits across lines by cent allocation (spec §7).
func (a *assembly) freightSells(weights []decimal.Decimal, floor decimal.Decimal) ([]decimal.Decimal, bool) {
	if a.in.Modal == ModalAIR {
		return allocateCents(airFreightSell(sum(weights), a.in.MarkupRate), weights), false
	}
	total, bound := fclFreightSell(sum(weights), a.in.MarkupRate, floor)
	return allocateCents(total, weights), bound
}

// priceAlternates prices each losing freight option as a complete line
// for the audit view (Q5), with the same sell formula it would have had.
func (a *assembly) priceAlternates(alts []candLine) {
	floor, _ := a.freightFloorBRL()
	for _, c := range alts {
		var sell decimal.Decimal
		if a.in.Modal == ModalAIR {
			sell = airFreightSell(c.netBRL(), a.in.MarkupRate)
		} else {
			var bound bool
			sell, bound = fclFreightSell(c.netBRL(), a.in.MarkupRate, floor)
			if bound {
				c.flags = append(c.flags, FlagMinMarkupApplied)
			}
		}
		a.alts = append(a.alts, toLine(c, sell))
	}
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

// buildTerminal prices TERMINAL_STORAGE_THC for imports: the CIF base
// (converted at the customs rate) selects the destination fee band, and
// scoped fixed fees add on (spec §4 steps 5-6).
func (a *assembly) buildTerminal(g *groups, freight []candLine) {
	if !g.terminal {
		return
	}
	a.emitBand(freight)
	a.emitPassFees(CategoryTerminalStorage)
}

// emitBand prices the matched fee band on the CIF base; a base that
// cannot be computed has already raised its own reason in cifBase. A
// band matched but unpriceable is a MISSING_FX gap (already recorded by
// the resolver) — MISSING_TERMINAL_BAND fires only on a true no-match.
func (a *assembly) emitBand(freight []candLine) {
	base, declared, ok := a.cifBase(freight)
	if !ok {
		return
	}
	c, outcome := bandCand(a.in, base, a.fxr)
	if outcome == bandNoMatch {
		a.reason(ReasonMissingTerminalBand)
		return
	}
	a.assume(AssumptionStorageFirstPeriodOnly)
	if outcome == bandUnpriced {
		return
	}
	if declared {
		c.flags = append(c.flags, FlagDeclaredBasis)
	}
	a.emitPass(c)
}

// cifBase computes (cargo + freight + insurance) converted per
// component at the customs rate (spec §4 step 6, §6). declared reports
// whether declared freight fed the base (DECLARED_BASIS flag); a stale
// customs rate still applies but warns — no line carries the flag.
func (a *assembly) cifBase(freight []candLine) (decimal.Decimal, bool, bool) {
	if a.in.CargoValue.IsZero() {
		a.reason(ReasonMissingCargoValue)
		return decimal.Zero, false, false
	}
	c := &cifAcc{fxr: a.fxr}
	if !c.add(a.in.CargoValue, a.in.CargoCurrency) ||
		!c.addFreight(a.in, freight) ||
		!c.addDeclared(a.in.DeclaredInsurance) {
		a.reason(ReasonMissingCustomsFX)
		return decimal.Zero, false, false
	}
	if c.stale {
		a.warn("CUSTOMS_FX_STALE")
	}
	return roundMoney(c.total), c.declared, true
}

// cifAcc accumulates the CIF base in BRL, converting each component at
// the customs rate; declared records DECLARED_BASIS provenance and
// stale remembers whether any applied customs rate was STALE_FX.
type cifAcc struct {
	fxr      *fxResolver
	total    decimal.Decimal
	declared bool
	stale    bool
}

func (c *cifAcc) add(amount decimal.Decimal, ccy string) bool {
	rate, flags, ok := c.fxr.customs(ccy)
	if !ok {
		return false
	}
	for _, f := range flags {
		if f == FlagStaleFX {
			c.stale = true
		}
	}
	c.total = c.total.Add(amount.Mul(rate))
	return true
}

func (c *cifAcc) addDeclared(m *Money) bool {
	if m == nil {
		return true
	}
	return c.add(m.Amount, m.Currency)
}

// addFreight adds the freight component of the CIF base: payable
// freight net when owed and priced, else the declared freight (RFC-001
// §5). A declared component sets the DECLARED_BASIS flag.
func (c *cifAcc) addFreight(in Input, freight []candLine) bool {
	for _, l := range freight {
		if !c.add(l.net, l.ccy) {
			return false
		}
	}
	if len(freight) > 0 {
		return true
	}
	if !c.addDeclared(in.DeclaredFreight) {
		return false
	}
	c.declared = in.DeclaredFreight != nil
	return true
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
