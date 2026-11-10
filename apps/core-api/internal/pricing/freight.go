package pricing

import (
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// airVolumetricFactor is the fixed MVP volumetric divisor for air
// freight (Q8: 166.67 kg per m3, AIR_FACTOR_166_67).
var airVolumetricFactor = decimal.RequireFromString("166.67")

// chargeableKg implements spec §4 step 2 for AIR:
// chargeable_kg = max(GrossKg, VolumeM3 x 166.67).
func chargeableKg(grossKg, volumeM3 decimal.Decimal) decimal.Decimal {
	volumetric := volumeM3.Mul(airVolumetricFactor)
	if volumetric.GreaterThan(grossKg) {
		return volumetric
	}
	return grossKg
}

// airBaseNet implements spec §5 for AIR:
// net = max(min_amount, chargeable_kg x net_amount); a NULL min_amount
// means no floor.
func airBaseNet(row TariffCandidate, kg decimal.Decimal) decimal.Decimal {
	net := kg.Mul(row.NetAmount)
	if row.MinAmount != nil && row.MinAmount.GreaterThan(net) {
		return *row.MinAmount
	}
	return net
}

// candLine is a priced candidate before it becomes an audit Line: the
// source-currency net amount plus the FX rate already resolved for it,
// so selection can compare net BRL before any line exists.
type candLine struct {
	cat      Category
	code     string
	desc     string
	src      SourceType
	tariffID *uuid.UUID
	snapID   *uuid.UUID
	offerID  *uuid.UUID
	ccy      string
	net      decimal.Decimal
	carrier  bool // carrier-charged → carrier FX path (spec §6)
	rate     decimal.Decimal
	flags    []string
}

// netBRL is the unrounded BRL cost used for cheapest-wins comparison
// and for aggregate markup math.
func (c candLine) netBRL() decimal.Decimal {
	return c.net.Mul(c.rate)
}

// offerVerdict is the spec §4 step 4 decision on the input carrier
// offer. usable is non-nil only when the offer may price the freight.
type offerVerdict struct {
	offer       *CarrierOffer // the offer as supplied, for context
	usable      *CarrierOffer // non-nil iff usable per Q12 completeness
	expired     bool          // missing expiry or ExpiresAt < PricedAt
	incomplete  bool          // usable coverage check failed
	ignored     bool          // present but irrelevant (not FCL / not owed)
	offerFailed bool          // usable but no priceable charge (set by selectFreight)
}

// assessOffer decides offer candidacy (spec §4 step 4): offers apply to
// FCL freight the customer owes only; expiry and the Q12 strict
// coverage check make a present offer unusable.
func assessOffer(in Input, g *groups) offerVerdict {
	if in.CarrierOffer == nil {
		return offerVerdict{}
	}
	v := offerVerdict{offer: in.CarrierOffer}
	if in.Modal != ModalFCL || !g.freight {
		v.ignored = true
		return v
	}
	return completeOffer(v, in, g)
}

// completeOffer applies the expiry and Q12 strict-coverage checks to an
// offer that is in scope (FCL, freight owed). The payload is untrusted
// ai-engine data (rule 6), so a missing ExpiresAt fails closed — it is
// treated exactly like a past expiry.
func completeOffer(v offerVerdict, in Input, g *groups) offerVerdict {
	o := v.offer
	if o.ExpiresAt.IsZero() || o.ExpiresAt.Before(in.PricedAt) {
		v.expired = true
		return v
	}
	if !offerCovers(o, in, g) {
		v.incomplete = true
		return v
	}
	v.usable = o
	return v
}

// offerCovers is the Q12 strict default: the offer must cover every
// required carrier group — ocean freight always, plus origin charges
// when the matrix requires them (EXW/FCA). Public terminal fees are
// carrier-independent and never required from the offer. An offer that
// fails coverage is rejected whole: it never combines with another
// carrier's rows silently.
func offerCovers(o *CarrierOffer, in Input, g *groups) bool {
	if len(offerCharges(o, ChargeOceanFreight, in.EquipmentOrUnit)) == 0 {
		return false
	}
	if g.origin && len(offerCharges(o, ChargeOriginCharge, in.EquipmentOrUnit)) == 0 {
		return false
	}
	return true
}

// offerCharges returns the offer's charges of one category matching the
// requested equipment (an empty equipment on the charge means generic).
// Pre-carriage codes are out of MVP (spec §3 FCA note) and drop here so
// they can neither satisfy coverage nor reach a price line.
func offerCharges(o *CarrierOffer, cat ChargeCategory, equipment string) []OfferCharge {
	out := make([]OfferCharge, 0, len(o.Charges))
	for _, c := range o.Charges {
		if c.Category != cat || !equipmentMatches(c.EquipmentOrUnit, equipment) || isPreCarriage(c.Code) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// freightResult is the freight group's selection: the winning option's
// line(s) — several lines can share one aggregate sell (spec §7) — and
// the losing options' lines for the audit view.
type freightResult struct {
	winners    []candLine
	alternates []candLine
	// tariffWinners is the tariff pool's own per-code winners — the
	// baseline tariff alternates price against, so an alternative is
	// never a hybrid of offer winners plus tariff rows (Q12).
	tariffWinners []candLine
	missing       bool
	offerFailed   bool // a usable offer produced no priceable line
}

// selectFreight picks the freight option (spec §4 step 5): a usable
// carrier offer is preferred over tariff rows; otherwise the cheapest
// net cost wins and losers go to Alternatives. An offer whose charges
// cannot be priced (e.g. missing FX) falls through to tariffs rather
// than blocking the route.
func selectFreight(in Input, resolved []TariffCandidate, v offerVerdict, fxr *fxResolver) freightResult {
	tariffs := freightTariffCands(in, resolved, fxr)
	if v.usable != nil {
		if res, ok := offerWinners(in, v.usable, tariffs, fxr); ok {
			return res
		}
		// The offer could not be priced at all; it counts as unusable.
		v.offerFailed = true
	}
	return tariffFreight(v, tariffs)
}

// offerWinners returns the offer's priced freight lines when at least
// one could be priced; tariff rows then become the alternatives, each
// still priced against the tariff pool's own winners (never a hybrid).
func offerWinners(in Input, o *CarrierOffer, tariffs []candLine, fxr *fxResolver) (freightResult, bool) {
	winners := offerFreightCands(in, o, fxr)
	if len(winners) == 0 {
		return freightResult{}, false
	}
	pool := collectGroup(tariffs)
	return freightResult{winners: winners, alternates: tariffs, tariffWinners: pool.winners}, true
}

// tariffFreight applies the same per-charge-code competition as
// collectGroup (spec §4 step 5): the cheapest resolved row per code
// wins — a freight charge modeled as two distinct codes must keep both
// legs — and every winner shares the aggregate sell. OFFER_EXPIRED
// marks the winning lines only when they actually replaced an expired
// offer — an unpriceable offer is a warning, not an expiry (spec §9).
func tariffFreight(v offerVerdict, tariffs []candLine) freightResult {
	if len(tariffs) == 0 {
		return freightResult{missing: true, offerFailed: v.offerFailed}
	}
	res := collectGroup(tariffs)
	if v.expired {
		for i := range res.winners {
			res.winners[i].flags = append(res.winners[i].flags, FlagOfferExpired)
		}
	}
	return freightResult{winners: res.winners, alternates: res.alternates,
		tariffWinners: res.winners, offerFailed: v.offerFailed}
}

// freightTariffCands prices every resolved freight tariff row: FCL nets
// per container count, AIR nets on the chargeable base (spec §5). The
// shared applicability gate re-verifies modal, route and equipment —
// a misplaced-but-valid row must never win the freight group (rule 6).
func freightTariffCands(in Input, resolved []TariffCandidate, fxr *fxResolver) []candLine {
	out := make([]candLine, 0, len(resolved))
	for _, r := range resolved {
		if r.ChargeCategory != freightCategory(in.Modal) || !tariffApplies(r, in) {
			continue
		}
		c := toFreightTariffCand(in, r)
		if !priceCand(&c, fxr) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// toFreightTariffCand maps a resolved freight tariff row to a candidate
// at its chargeable net (per-container FCL, per-kg AIR).
func toFreightTariffCand(in Input, r TariffCandidate) candLine {
	return candLine{
		cat:      CategoryIntlFreight,
		code:     r.ChargeCode,
		desc:     freightDesc(r),
		src:      SourcePrivateTariff,
		tariffID: idPtr(r.ID),
		ccy:      r.Currency,
		carrier:  true,
		net:      freightNet(in, r),
	}
}

func freightCategory(m Modal) ChargeCategory {
	if m == ModalAIR {
		return ChargeAirFreight
	}
	return ChargeOceanFreight
}

// freightNet is the candidate's source-currency net: FCL tariffs rate
// per equipment unit, AIR tariffs rate per chargeable kg, offer charges
// arrive already totalled.
func freightNet(in Input, r TariffCandidate) decimal.Decimal {
	if in.Modal == ModalAIR {
		return airBaseNet(r, chargeableKg(in.GrossKg, in.VolumeM3))
	}
	return r.NetAmount.Mul(decimal.NewFromInt(int64(in.ContainerCount)))
}

func freightDesc(r TariffCandidate) string {
	if r.SearchDescription != "" {
		return r.SearchDescription
	}
	return r.CarrierOrAgent + " " + r.ChargeCode
}

// offerFreightCands prices a usable offer's ocean charges; every charge
// becomes a line sharing the aggregate sell (spec §7).
func offerFreightCands(in Input, o *CarrierOffer, fxr *fxResolver) []candLine {
	out := make([]candLine, 0, len(o.Charges))
	for _, ch := range offerCharges(o, ChargeOceanFreight, in.EquipmentOrUnit) {
		c := candLine{
			cat:     CategoryIntlFreight,
			code:    ch.Code,
			desc:    o.Carrier + " " + ch.Code,
			src:     SourceCarrierOffer,
			offerID: idPtr(o.ID),
			ccy:     ch.Currency,
			carrier: true,
			net:     ch.Amount,
		}
		if !priceCand(&c, fxr) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// priceCand resolves the candidate's FX rate; failure leaves it out and
// the resolver records the missing currency.
func priceCand(c *candLine, fxr *fxResolver) bool {
	rate, flags, ok := fxr.resolve(c.carrier, c.ccy)
	if !ok {
		return false
	}
	c.rate = rate
	c.flags = append(c.flags, flags...)
	return true
}

// cheapestIdx returns the candidate with the lowest unrounded net BRL;
// ties break on the stable description for determinism.
func cheapestIdx(cands []candLine) int {
	best := 0
	for i := 1; i < len(cands); i++ {
		if less(cands[i], cands[best]) {
			best = i
		}
	}
	return best
}

func less(a, b candLine) bool {
	if c := a.netBRL().Cmp(b.netBRL()); c != 0 {
		return c < 0
	}
	return a.desc < b.desc
}

func dropIdx(cands []candLine, i int) []candLine {
	out := make([]candLine, 0, len(cands)-1)
	out = append(out, cands[:i]...)
	return append(out, cands[i+1:]...)
}

func idPtr(id uuid.UUID) *uuid.UUID {
	return &id
}
