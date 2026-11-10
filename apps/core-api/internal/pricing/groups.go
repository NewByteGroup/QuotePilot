package pricing

import (
	"fmt"
	"sort"
	"strings"

	"github.com/shopspring/decimal"
)

// groupResult splits a group's priced candidates into winning lines and
// losing options for the audit view (Q5: one price plus alternatives).
type groupResult struct {
	winners    []candLine
	alternates []candLine
}

// collectGroup applies "cheapest net cost wins" per charge_code (spec §4
// step 5): rows sharing a code are the same fee from different parties —
// either/or — while distinct codes are additive lines.
func collectGroup(cands []candLine) groupResult {
	byCode := make(map[string][]candLine)
	for _, c := range cands {
		byCode[c.code] = append(byCode[c.code], c)
	}
	codes := make([]string, 0, len(byCode))
	for code := range byCode {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	res := groupResult{}
	for _, code := range codes {
		rows := byCode[code]
		best := cheapestIdx(rows)
		res.winners = append(res.winners, rows[best])
		res.alternates = append(res.alternates, dropIdx(rows, best)...)
	}
	return res
}

// tariffCandsFor collects resolved tariff rows of one charge category
// into a line group. carrier marks the carrier FX path: freight and
// carrier surcharges (spec §6), everything else prices at PTAX.
func tariffCandsFor(chcat ChargeCategory, cat Category, in Input, resolved []TariffCandidate, fxr *fxResolver) []candLine {
	out := make([]candLine, 0, len(resolved))
	for _, r := range resolved {
		// The worker's filters are untrusted: a row for another modal,
		// route or equipment must never price into this quote (rule 6).
		if r.ChargeCategory != chcat || !tariffApplies(r, in) {
			continue
		}
		c := toTariffCand(r, cat)
		if !priceCand(&c, fxr) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// toTariffCand maps a resolved tariff row to a group candidate at its
// flat net amount — freight rows get per-unit math in freight.go, so
// only the surcharge pool carries carrier-charged rows here.
func toTariffCand(r TariffCandidate, cat Category) candLine {
	return candLine{
		cat:      cat,
		code:     r.ChargeCode,
		desc:     freightDesc(r),
		src:      SourcePrivateTariff,
		tariffID: idPtr(r.ID),
		ccy:      r.Currency,
		carrier:  cat == CategorySurcharge,
		net:      r.NetAmount,
	}
}

// offerCandsFor collects a usable offer's charges of one category into
// the matching group pool; offer charges are always carrier-charged.
// offerCharges already drops pre-carriage codes before they reach a
// pool (spec §3 FCA note).
func offerCandsFor(chcat ChargeCategory, cat Category, o *CarrierOffer, in Input, fxr *fxResolver) []candLine {
	out := make([]candLine, 0, len(o.Charges))
	for _, ch := range offerCharges(o, chcat, in.EquipmentOrUnit) {
		c := toOfferCand(ch, cat, o)
		if !priceCand(&c, fxr) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func toOfferCand(ch OfferCharge, cat Category, o *CarrierOffer) candLine {
	return candLine{
		cat:     cat,
		code:    ch.Code,
		desc:    o.Carrier + " " + ch.Code,
		src:     SourceCarrierOffer,
		offerID: idPtr(o.ID),
		ccy:     ch.Currency,
		carrier: true,
		net:     ch.Amount,
	}
}

// equipmentMatches is the offer-charge equipment rule: an empty
// equipment on the charge covers any requested unit; otherwise the
// codes compare normalized like LOCODEs — padding and case are noise.
func equipmentMatches(chargeEquipment, requested string) bool {
	return chargeEquipment == "" ||
		strings.EqualFold(strings.TrimSpace(chargeEquipment), strings.TrimSpace(requested))
}

// fixedFeeCands collects org fixed fees scoped to a group (spec §4 step
// 5/7). Fixed fees are org rules, not competing candidates: each emits
// its own line. Pre-carriage codes are out of MVP and drop here; the
// audit warning is emitted once up front in preCarriageSkips.
func fixedFeeCands(cat Category, fees []FixedFee, fxr *fxResolver) []candLine {
	out := make([]candLine, 0, len(fees))
	for _, f := range fees {
		if f.Category != cat || isPreCarriage(f.ChargeCode) {
			continue
		}
		c := toFixedFeeCand(f)
		if !priceCand(&c, fxr) {
			continue
		}
		out = append(out, c)
	}
	return out
}

func toFixedFeeCand(f FixedFee) candLine {
	desc := f.Description
	if desc == "" {
		desc = f.ChargeCode
	}
	return candLine{
		cat:  f.Category,
		code: f.ChargeCode,
		desc: desc,
		src:  SourceOrgFixedRule,
		ccy:  f.Currency,
		net:  f.Amount,
	}
}

// isPreCarriage detects out-of-MVP inland/pre-carriage charge codes,
// skipped with a warning (spec §3 FCA note). INLAND must be a whole
// code token — MAINLAND_FEE is not a pre-carriage charge.
func isPreCarriage(code string) bool {
	up := strings.ToUpper(code)
	if strings.Contains(up, "PRE_CARRIAGE") || strings.Contains(up, "PRE-CARRIAGE") {
		return true
	}
	for _, tok := range strings.FieldsFunc(up, isCodeSep) {
		if tok == "INLAND" {
			return true
		}
	}
	return false
}

// isCodeSep separates charge-code tokens: codes in the feed use '_',
// '-' or ' ' as word boundaries.
func isCodeSep(r rune) bool { return r == '_' || r == '-' || r == ' ' }

// buildTerminal prices TERMINAL_STORAGE_THC for imports: the CIF base
// (converted at the customs rate) selects the destination fee band, and
// scoped fixed fees add on (spec §4 steps 5-6). needsDeclared marks the
// matrix rows where no freight is payable — the CIF family and
// requested DAP/DPU — so the band can only be sized from the
// seller-paid declared freight.
func (a *assembly) buildTerminal(g *groups, freight []candLine) {
	if !g.terminal {
		return
	}
	a.emitBand(freight, !g.freight)
	a.emitPassFees(CategoryTerminalStorage)
}

// emitBand prices the matched fee band on the CIF base; a base that
// cannot be computed has already raised its own reason in cifBase. A
// band matched but unpriceable is a MISSING_FX gap (already recorded by
// the resolver) — MISSING_TERMINAL_BAND fires only on a true no-match.
func (a *assembly) emitBand(freight []candLine, needsDeclared bool) {
	base, declared, ok := a.cifBase(freight, needsDeclared)
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
func (a *assembly) cifBase(freight []candLine, needsDeclared bool) (decimal.Decimal, bool, bool) {
	if !a.cifInputsPresent(needsDeclared) {
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

// cifInputsPresent checks the inputs the CIF base needs before any
// conversion: a cargo value always, and — when no freight is payable
// (the CIF family and requested DAP/DPU) — the seller-paid declared
// freight. A nil declared freight is absent input, never a silent
// zero; a zero-valued Money stays a known zero (PR15). Each absent
// input records its reason.
func (a *assembly) cifInputsPresent(needsDeclared bool) bool {
	ok := true
	if a.in.CargoValue.IsZero() {
		a.reason(ReasonMissingCargoValue)
		ok = false
	}
	if needsDeclared && a.in.DeclaredFreight == nil {
		a.reason(ReasonMissingDeclaredFreight)
		ok = false
	}
	return ok
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
	// A known zero contributes nothing and resolves no rate — otherwise
	// a zero declared in a rate-less currency raises a spurious
	// MISSING_CUSTOMS_FX (PR15 review).
	if amount.IsZero() {
		return true
	}
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

// bandOutcome is the fee-band selection result. A band that matched the
// CIF base but failed FX resolution is a MISSING_FX gap (recorded by the
// resolver), never MISSING_TERMINAL_BAND — that reason is reserved for
// a true no-match.
type bandOutcome int

const (
	bandNoMatch  bandOutcome = iota // no band bracketed the CIF base
	bandUnpriced                    // a band matched but its FX failed
	bandPriced                      // a priced band is ready to emit
)

// bandCand picks the destination fee band for the CIF base (spec §4
// step 5): normalized location match, CIFMin <= base < CIFMax, optional
// weight/volume minimums, first period only (Q9).
func bandCand(in Input, cifBaseBRL decimal.Decimal, fxr *fxResolver) (candLine, bandOutcome) {
	b, ok := pickBand(in, cifBaseBRL)
	if !ok {
		return candLine{}, bandNoMatch
	}
	c := candLine{
		cat:    CategoryTerminalStorage,
		code:   "TERMINAL_STORAGE",
		desc:   fmt.Sprintf("Terminal/storage %s period %dd", normalizeLocode(b.Location), b.PeriodDays),
		src:    SourceFirecrawlPublic,
		snapID: idPtr(b.SnapshotID),
		ccy:    b.Currency,
		net:    b.Amount,
	}
	if !priceCand(&c, fxr) {
		return candLine{}, bandUnpriced
	}
	return c, bandPriced
}

// pickBand returns the matching band for the first period only (Q9):
// smallest PeriodDays, snapshot id as the deterministic tiebreaker.
func pickBand(in Input, cifBaseBRL decimal.Decimal) (FeeBand, bool) {
	matches := make([]FeeBand, 0, len(in.Snapshots))
	for _, b := range in.Snapshots {
		if bandMatches(b, in, cifBaseBRL) {
			matches = append(matches, b)
		}
	}
	if len(matches) == 0 {
		return FeeBand{}, false
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].PeriodDays != matches[j].PeriodDays {
			return matches[i].PeriodDays < matches[j].PeriodDays
		}
		return matches[i].SnapshotID.String() < matches[j].SnapshotID.String()
	})
	return matches[0], true
}

// bandMatches implements the band predicate: normalized destination
// LOCODE, half-open CIF range, and min bounds only when set.
func bandMatches(b FeeBand, in Input, cifBaseBRL decimal.Decimal) bool {
	if normalizeLocode(b.Location) != normalizeLocode(in.DestLOCODE) {
		return false
	}
	if cifBaseBRL.LessThan(b.CIFMin) || !cifBaseBRL.LessThan(b.CIFMax) {
		return false
	}
	if b.WeightMinKg != nil && in.GrossKg.LessThan(*b.WeightMinKg) {
		return false
	}
	if b.VolumeMinM3 != nil && in.VolumeM3.LessThan(*b.VolumeMinM3) {
		return false
	}
	return true
}

func normalizeLocode(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}
