package pricing

import (
	"fmt"
	"sort"
	"time"
)

// tariffKey is the private_tariffs natural key (RFC-002 step 3): all
// versions of "the same charge" share it across batches.
type tariffKey struct {
	carrierOrAgent  string
	originLocode    string
	destLocode      string
	modal           Modal
	equipmentOrUnit string
	chargeCategory  ChargeCategory
	chargeCode      string
}

func (r TariffCandidate) key() tariffKey {
	return tariffKey{
		carrierOrAgent:  r.CarrierOrAgent,
		originLocode:    r.OriginLOCODE,
		destLocode:      r.DestLOCODE,
		modal:           r.Modal,
		equipmentOrUnit: r.EquipmentOrUnit,
		chargeCategory:  r.ChargeCategory,
		chargeCode:      r.ChargeCode,
	}
}

func (k tariffKey) String() string {
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s", k.carrierOrAgent, k.originLocode,
		k.destLocode, k.modal, k.equipmentOrUnit, k.chargeCategory, k.chargeCode)
}

// resolveTariffs applies spec §4 step 3: keep rows in their validity
// window vs QuoteDate whose batch activated at or before PricedAt, take
// the latest-activated row per natural key (ties: later created_at,
// then greatest id), and drop the key when that winner is a tombstone.
// The result is sorted by natural key so no map order leaks downstream.
func resolveTariffs(candidates []TariffCandidate, quoteDate, pricedAt time.Time) []TariffCandidate {
	winners := make(map[tariffKey]TariffCandidate, len(candidates))
	for _, r := range candidates {
		if !eligibleRow(r, quoteDate, pricedAt) {
			continue
		}
		if cur, ok := winners[r.key()]; !ok || r.beats(cur) {
			winners[r.key()] = r
		}
	}
	return drainWinners(winners)
}

// drainWinners drops tombstoned keys and returns the survivors sorted
// by natural key so no map order leaks downstream.
func drainWinners(winners map[tariffKey]TariffCandidate) []TariffCandidate {
	keys := make([]tariffKey, 0, len(winners))
	for k, r := range winners {
		if !r.IsRemoved {
			keys = append(keys, k)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	out := make([]TariffCandidate, 0, len(keys))
	for _, k := range keys {
		out = append(out, winners[k])
	}
	return out
}

// eligibleRow implements the two window gates: valid_from <= QuoteDate
// <= valid_until (inclusive, day granularity) and activated_at <=
// PricedAt (a NULL activation means the batch is still PENDING_REVIEW).
func eligibleRow(r TariffCandidate, quoteDate, pricedAt time.Time) bool {
	if r.BatchActivatedAt == nil || r.BatchActivatedAt.After(pricedAt) {
		return false
	}
	day := quoteDate.UTC().Truncate(24 * time.Hour)
	if day.Before(r.ValidFrom.UTC().Truncate(24 * time.Hour)) {
		return false
	}
	return !day.After(r.ValidUntil.UTC().Truncate(24 * time.Hour))
}

// beats reports the "latest wins" ordering between two rows of the same
// natural key: newer batch activation, then later created_at, then the
// greatest id for a fully deterministic result.
func (r TariffCandidate) beats(other TariffCandidate) bool {
	if !r.BatchActivatedAt.Equal(*other.BatchActivatedAt) {
		return r.BatchActivatedAt.After(*other.BatchActivatedAt)
	}
	if !r.CreatedAt.Equal(other.CreatedAt) {
		return r.CreatedAt.After(other.CreatedAt)
	}
	return r.ID.String() > other.ID.String()
}
