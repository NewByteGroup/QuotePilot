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
		// The worker's filters are untrusted: a row for another modal
		// must never price into this quote (architecture rule 6).
		if r.ChargeCategory != chcat || r.Modal != in.Modal {
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
func offerCandsFor(chcat ChargeCategory, cat Category, o *CarrierOffer, in Input, fxr *fxResolver) []candLine {
	out := make([]candLine, 0, len(o.Charges))
	for _, ch := range o.Charges {
		if ch.Category != chcat || !equipmentMatches(ch.EquipmentOrUnit, in.EquipmentOrUnit) {
			continue
		}
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
// equipment on the charge covers any requested unit.
func equipmentMatches(chargeEquipment, requested string) bool {
	return chargeEquipment == "" || strings.EqualFold(chargeEquipment, requested)
}

// fixedFeeCands collects org fixed fees scoped to a group (spec §4 step
// 5/7). Fixed fees are org rules, not competing candidates: each emits
// its own line.
func fixedFeeCands(cat Category, fees []FixedFee, fxr *fxResolver) []candLine {
	out := make([]candLine, 0, len(fees))
	for _, f := range fees {
		if f.Category != cat {
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
// skipped with a warning (spec §3 FCA note).
func isPreCarriage(code string) bool {
	up := strings.ToUpper(code)
	return strings.Contains(up, "INLAND") || strings.Contains(up, "PRE_CARRIAGE") || strings.Contains(up, "PRE-CARRIAGE")
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
