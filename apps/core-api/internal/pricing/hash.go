package pricing

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Decimal render scales by column kind (spec §8): money 2, FX rates 6,
// percentages 4, tariff amounts 4, cargo measures 3.
const (
	scaleMoney   = 2
	scaleWeight  = 3
	scalePercent = 4
	scaleTariff  = 4
	scaleRate    = 6
)

// hashInput is the spec §8 fingerprint: sha256 hex over the canonical
// serialization of the whole Input — sorted keys, fixed decimal scales,
// RFC-3339 UTC timestamps, lowercase UUIDs, slices in input order. It
// pins quotes.inputs_hash so "same quote twice" is detectable.
func hashInput(in Input) string {
	sum := sha256.Sum256([]byte(canonicalInput(in)))
	return hex.EncodeToString(sum[:])
}

// canonicalInput renders Input as canonical JSON. Keys are emitted in
// alphabetical order by construction; every number is a quoted
// fixed-scale decimal string so byte equality is semantic equality.
func canonicalInput(in Input) string {
	var b strings.Builder
	b.WriteString("{\"carrier_offer\":")
	writeOffer(&b, in.CarrierOffer)
	writeScalars(&b, in)
	writeSlices(&b, in)
	b.WriteString("}")
	return b.String()
}

// writeScalars emits the scalar Input fields alphabetically after
// carrier_offer (which canonicalInput already wrote).
func writeScalars(b *strings.Builder, in Input) {
	b.WriteString(",\"cargo_currency\":" + js(in.CargoCurrency))
	b.WriteString(",\"cargo_value\":" + ds(in.CargoValue, scaleMoney))
	b.WriteString(",\"container_count\":" + strconv.Itoa(in.ContainerCount))
	b.WriteString(",\"declared_freight\":")
	writeMoney(b, in.DeclaredFreight)
	b.WriteString(",\"declared_insurance\":")
	writeMoney(b, in.DeclaredInsurance)
	b.WriteString(",\"dest_locode\":" + js(in.DestLOCODE))
	b.WriteString(",\"direction\":" + js(string(in.Direction)))
	b.WriteString(",\"equipment_or_unit\":" + js(in.EquipmentOrUnit))
}

// writeSlices emits the slice Input fields alphabetically after
// equipment_or_unit.
func writeSlices(b *strings.Builder, in Input) {
	b.WriteString(",\"fixed_fees\":")
	writeFixedFees(b, in.FixedFees)
	b.WriteString(",\"fx_rates\":")
	writeFXRates(b, in.FXRates)
	b.WriteString(",\"fx_safety_spread\":" + ds(in.FXSafetySpread, scalePercent))
	b.WriteString(",\"gross_kg\":" + ds(in.GrossKg, scaleWeight))
	b.WriteString(",\"incoterm\":" + js(string(in.Incoterm)))
	b.WriteString(",\"markup_rate\":" + ds(in.MarkupRate, scalePercent))
	b.WriteString(",\"min_markup_usd\":")
	writeDecPtr(b, in.MinMarkupUSD, scaleMoney)
	writeTail(b, in)
}

// writeTail emits the remaining scalar + slice fields alphabetically.
func writeTail(b *strings.Builder, in Input) {
	b.WriteString(",\"modal\":" + js(string(in.Modal)))
	b.WriteString(",\"origin_locode\":" + js(in.OriginLOCODE))
	b.WriteString(",\"priced_at\":" + jt(in.PricedAt))
	b.WriteString(",\"quote_date\":" + jt(in.QuoteDate))
	b.WriteString(",\"snapshots\":")
	writeBands(b, in.Snapshots)
	b.WriteString(",\"tariffs\":")
	writeTariffs(b, in.Tariffs)
	b.WriteString(",\"terminal_requested\":" + strconv.FormatBool(in.TerminalRequested))
	b.WriteString(",\"volume_m3\":" + ds(in.VolumeM3, scaleWeight))
}

func writeTariffs(b *strings.Builder, rows []TariffCandidate) {
	b.WriteString("[")
	for i, r := range rows {
		if i > 0 {
			b.WriteString(",")
		}
		writeTariff(b, r)
	}
	b.WriteString("]")
}

func writeTariff(b *strings.Builder, r TariffCandidate) {
	b.WriteString("{\"batch_activated_at\":")
	writeTimePtr(b, r.BatchActivatedAt)
	b.WriteString(",\"batch_id\":" + ju(r.BatchID))
	b.WriteString(",\"carrier_or_agent\":" + js(r.CarrierOrAgent))
	b.WriteString(",\"charge_category\":" + js(string(r.ChargeCategory)))
	b.WriteString(",\"charge_code\":" + js(r.ChargeCode))
	b.WriteString(",\"created_at\":" + jt(r.CreatedAt))
	b.WriteString(",\"currency\":" + js(r.Currency))
	b.WriteString(",\"dest_locode\":" + js(r.DestLOCODE))
	b.WriteString(",\"equipment_or_unit\":" + js(r.EquipmentOrUnit))
	b.WriteString(",\"id\":" + ju(r.ID))
	b.WriteString(",\"is_removed\":" + strconv.FormatBool(r.IsRemoved))
	b.WriteString(",\"min_amount\":")
	writeDecPtr(b, r.MinAmount, scaleTariff)
	writeTariffTail(b, r)
}

func writeTariffTail(b *strings.Builder, r TariffCandidate) {
	b.WriteString(",\"modal\":" + js(string(r.Modal)))
	b.WriteString(",\"net_amount\":" + ds(r.NetAmount, scaleTariff))
	b.WriteString(",\"origin_locode\":" + js(r.OriginLOCODE))
	b.WriteString(",\"search_description\":" + js(r.SearchDescription))
	b.WriteString(",\"valid_from\":" + jt(r.ValidFrom))
	b.WriteString(",\"valid_until\":" + jt(r.ValidUntil))
	b.WriteString("}")
}

func writeBands(b *strings.Builder, rows []FeeBand) {
	b.WriteString("[")
	for i, f := range rows {
		if i > 0 {
			b.WriteString(",")
		}
		writeBand(b, f)
	}
	b.WriteString("]")
}

func writeBand(b *strings.Builder, f FeeBand) {
	b.WriteString("{\"amount\":" + ds(f.Amount, scaleTariff))
	b.WriteString(",\"cif_max\":" + ds(f.CIFMax, scaleMoney))
	b.WriteString(",\"cif_min\":" + ds(f.CIFMin, scaleMoney))
	b.WriteString(",\"currency\":" + js(f.Currency))
	b.WriteString(",\"location\":" + js(f.Location))
	b.WriteString(",\"period_days\":" + strconv.Itoa(f.PeriodDays))
	b.WriteString(",\"snapshot_id\":" + ju(f.SnapshotID))
	b.WriteString(",\"volume_min_m3\":")
	writeDecPtr(b, f.VolumeMinM3, scaleWeight)
	b.WriteString(",\"weight_min_kg\":")
	writeDecPtr(b, f.WeightMinKg, scaleWeight)
	b.WriteString("}")
}

func writeFixedFees(b *strings.Builder, rows []FixedFee) {
	b.WriteString("[")
	for i, f := range rows {
		if i > 0 {
			b.WriteString(",")
		}
		writeFixedFee(b, f)
	}
	b.WriteString("]")
}

func writeFixedFee(b *strings.Builder, f FixedFee) {
	b.WriteString("{\"amount\":" + ds(f.Amount, scaleMoney))
	b.WriteString(",\"category\":" + js(string(f.Category)))
	b.WriteString(",\"charge_code\":" + js(f.ChargeCode))
	b.WriteString(",\"currency\":" + js(f.Currency))
	b.WriteString(",\"description\":" + js(f.Description))
	b.WriteString("}")
}

func writeFXRates(b *strings.Builder, rows []FXRate) {
	b.WriteString("[")
	for i, r := range rows {
		if i > 0 {
			b.WriteString(",")
		}
		writeFXRate(b, r)
	}
	b.WriteString("]")
}

func writeFXRate(b *strings.Builder, r FXRate) {
	b.WriteString("{\"currency\":" + js(r.Currency))
	b.WriteString(",\"date\":" + jt(r.Date))
	b.WriteString(",\"rate\":" + ds(r.Rate, scaleRate))
	b.WriteString(",\"source\":" + js(r.Source))
	b.WriteString("}")
}

func writeOffer(b *strings.Builder, o *CarrierOffer) {
	if o == nil {
		b.WriteString("null")
		return
	}
	b.WriteString("{\"carrier\":" + js(o.Carrier))
	b.WriteString(",\"charges\":[")
	for i, c := range o.Charges {
		if i > 0 {
			b.WriteString(",")
		}
		writeOfferCharge(b, c)
	}
	b.WriteString("],\"expires_at\":" + jt(o.ExpiresAt))
	b.WriteString(",\"id\":" + ju(o.ID))
	b.WriteString("}")
}

func writeOfferCharge(b *strings.Builder, c OfferCharge) {
	b.WriteString("{\"amount\":" + ds(c.Amount, scaleTariff))
	b.WriteString(",\"category\":" + js(string(c.Category)))
	b.WriteString(",\"code\":" + js(c.Code))
	b.WriteString(",\"currency\":" + js(c.Currency))
	b.WriteString(",\"equipment_or_unit\":" + js(c.EquipmentOrUnit))
	b.WriteString("}")
}

func writeMoney(b *strings.Builder, m *Money) {
	if m == nil {
		b.WriteString("null")
		return
	}
	b.WriteString("{\"amount\":" + ds(m.Amount, scaleMoney))
	b.WriteString(",\"currency\":" + js(m.Currency))
	b.WriteString("}")
}

func writeDecPtr(b *strings.Builder, d *decimal.Decimal, scale int32) {
	if d == nil {
		b.WriteString("null")
		return
	}
	b.WriteString(ds(*d, scale))
}

func writeTimePtr(b *strings.Builder, t *time.Time) {
	if t == nil {
		b.WriteString("null")
		return
	}
	b.WriteString(jt(*t))
}

// js renders a string as canonical JSON (json.Marshal guarantees valid
// escaping).
func js(s string) string {
	out, _ := json.Marshal(s)
	return string(out)
}

// ds renders a decimal at its column's fixed scale, quoted so trailing
// zeros survive (1.5 and 1.50 hash identically — semantic equality).
func ds(d decimal.Decimal, scale int32) string {
	return js(d.StringFixed(scale))
}

// jt renders a timestamp RFC-3339 UTC. RFC3339Nano keeps sub-second
// precision when present (DB timestamps arrive at micro/nano scale);
// whole-second values render identically to RFC3339, so nothing is lost.
func jt(t time.Time) string {
	return js(t.UTC().Format(time.RFC3339Nano))
}

// ju renders a UUID lowercase.
func ju(id uuid.UUID) string {
	return js(strings.ToLower(id.String()))
}
