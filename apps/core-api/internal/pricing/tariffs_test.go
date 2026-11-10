package pricing

import (
	"testing"
	"time"
)

// Task 3 (spec §4 step 3): version resolution groups candidates by
// natural key, keeps in-window rows from batches activated at or before
// PricedAt, and lets the latest-activated row win; a tombstone winner
// drops the whole natural key.

func activatedAt(t time.Time) *time.Time { return &t }

func TestResolveTariffs_SingleEligibleRowPasses(t *testing.T) {
	row := tariffRow(testUUID1, testUUID2, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "950.0000")
	got := resolveTariffs([]TariffCandidate{row}, testQuoteDate, testPricedAt)
	if len(got) != 1 || got[0].ID != testUUID1 {
		t.Fatalf("eligible row dropped: %+v", got)
	}
}

func TestResolveTariffs_NewerBatchWinsNaturalKey(t *testing.T) {
	old := tariffRow(testUUID1, testUUID2, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "950.0000")
	newer := tariffRow(testUUID3, testUUID4, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "800.0000")
	act := ts(2026, 11, 15, 9, 0)
	newer.BatchActivatedAt = &act
	newer.CreatedAt = ts(2026, 11, 15, 9, 0)

	got := resolveTariffs([]TariffCandidate{old, newer}, testQuoteDate, testPricedAt)
	if len(got) != 1 || got[0].ID != testUUID3 {
		t.Fatalf("latest-activated row must win: %+v", got)
	}
}

func TestResolveTariffs_TombstoneWinnerDropsKey(t *testing.T) {
	live := tariffRow(testUUID1, testUUID2, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "950.0000")
	tomb := tariffRow(testUUID3, testUUID4, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "0")
	tomb.IsRemoved = true
	act := ts(2026, 11, 15, 9, 0)
	tomb.BatchActivatedAt = &act

	got := resolveTariffs([]TariffCandidate{live, tomb}, testQuoteDate, testPricedAt)
	if len(got) != 0 {
		t.Fatalf("tombstone winner must drop the natural key: %+v", got)
	}
}

func TestResolveTariffs_UnactivatedBatchIgnored(t *testing.T) {
	row := tariffRow(testUUID1, testUUID2, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "950.0000")
	row.BatchActivatedAt = nil // still PENDING_REVIEW
	got := resolveTariffs([]TariffCandidate{row}, testQuoteDate, testPricedAt)
	if len(got) != 0 {
		t.Fatalf("unactivated batch row must be ignored: %+v", got)
	}
}

func TestResolveTariffs_BatchActivatedAfterPricedAtIgnored(t *testing.T) {
	row := tariffRow(testUUID1, testUUID2, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "950.0000")
	late := ts(2026, 11, 30, 9, 0) // after PricedAt 2026-11-16
	row.BatchActivatedAt = &late
	got := resolveTariffs([]TariffCandidate{row}, testQuoteDate, testPricedAt)
	if len(got) != 0 {
		t.Fatalf("batch activated after priced_at must be ignored: %+v", got)
	}
}

func TestResolveTariffs_OutOfWindowIgnored(t *testing.T) {
	expired := tariffRow(testUUID1, testUUID2, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "950.0000")
	expired.ValidUntil = day(2026, 11, 15) // before QuoteDate
	future := tariffRow(testUUID3, testUUID4, "MAERSK", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "800.0000")
	future.ValidFrom = day(2026, 11, 17) // after QuoteDate

	got := resolveTariffs([]TariffCandidate{expired, future}, testQuoteDate, testPricedAt)
	if len(got) != 0 {
		t.Fatalf("out-of-window rows must be ignored: %+v", got)
	}
}

func TestResolveTariffs_QuoteDateBoundsInclusive(t *testing.T) {
	row := tariffRow(testUUID1, testUUID2, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "950.0000")
	row.ValidFrom = testQuoteDate
	row.ValidUntil = testQuoteDate
	got := resolveTariffs([]TariffCandidate{row}, testQuoteDate, testPricedAt)
	if len(got) != 1 {
		t.Fatalf("window bounds are inclusive: %+v", got)
	}
}

func TestResolveTariffs_TieBreaksByCreatedAtThenID(t *testing.T) {
	base := tariffRow(testUUID1, testUUID2, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "950.0000")
	base.CreatedAt = ts(2026, 11, 10, 9, 0)
	later := tariffRow(testUUID3, testUUID2, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "800.0000")
	later.CreatedAt = ts(2026, 11, 10, 10, 0) // same batch, later created_at

	got := resolveTariffs([]TariffCandidate{base, later}, testQuoteDate, testPricedAt)
	if len(got) != 1 || got[0].ID != testUUID3 {
		t.Fatalf("created_at tie-break failed: %+v", got)
	}

	// Same activation and created_at: greatest UUID wins, deterministically.
	a := tariffRow(testUUID1, testUUID2, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "950.0000")
	b := tariffRow(testUUID5, testUUID2, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "800.0000")
	got = resolveTariffs([]TariffCandidate{a, b}, testQuoteDate, testPricedAt)
	if len(got) != 1 || got[0].ID != testUUID5 {
		t.Fatalf("id tie-break failed: %+v", got)
	}
}

func TestResolveTariffs_KeysAreIndependent(t *testing.T) {
	freight := tariffRow(testUUID1, testUUID2, "HAPAG", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "950.0000")
	customs := tariffRow(testUUID3, testUUID2, "AGENT", ChargeCustoms, "CUSTOMS_CLEARANCE", "BRL", "450.0000")
	customs.EquipmentOrUnit = ""
	got := resolveTariffs([]TariffCandidate{freight, customs}, testQuoteDate, testPricedAt)
	if len(got) != 2 {
		t.Fatalf("distinct natural keys must both resolve: %+v", got)
	}
}

func TestResolveTariffs_OutputOrderDeterministic(t *testing.T) {
	rows := []TariffCandidate{
		tariffRow(testUUID1, testUUID2, "CARRIER_B", ChargeCustoms, "CUSTOMS_CLEARANCE", "BRL", "450.0000"),
		tariffRow(testUUID3, testUUID2, "CARRIER_A", ChargeOceanFreight, "OCEAN_FREIGHT", "USD", "950.0000"),
		tariffRow(testUUID4, testUUID2, "CARRIER_A", ChargeSurcharge, "BAF", "USD", "120.0000"),
	}
	first := resolveTariffs(rows, testQuoteDate, testPricedAt)
	for i := 0; i < 5; i++ {
		got := resolveTariffs(rows, testQuoteDate, testPricedAt)
		if len(got) != len(first) {
			t.Fatalf("resolution not deterministic: %d vs %d", len(got), len(first))
		}
		for j := range got {
			if got[j].ID != first[j].ID {
				t.Fatalf("order changed between runs: %+v vs %+v", got, first)
			}
		}
	}
}
