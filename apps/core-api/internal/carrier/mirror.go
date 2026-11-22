package carrier

// mirror.go — the MIRROR demo binding (spec S3a + RFC-003): a
// deterministic synthetic offer derived from the request, so the demo
// exercises the whole pipeline without a carrier credential. Same
// request ⇒ same offer. Every row it produces carries source=MIRROR —
// the UI's "simulated" label reads the column, not the environment.
// Boot guards (APP_ENV + validateProductionGuards) keep it out of
// production entirely; it must never price a real tenant's quote.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/shopspring/decimal"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
)

// mirrorCarrier is the synthetic carrier name the mirror's rows and
// payloads carry — honest about what produced them, and distinct from
// every real binding so checkpoint keys never collide across sources.
const mirrorCarrier = "MIRROR"

// MirrorFetcher is the deterministic demo connector. The offer is seeded
// from request_hash — the request content alone decides amounts, so a
// replay of the same request sees the same offer byte for byte.
type MirrorFetcher struct{}

var _ Fetcher = MirrorFetcher{}

// Carrier returns the synthetic binding name.
func (MirrorFetcher) Carrier() string { return mirrorCarrier }

// Source reports MIRROR (RFC-003's simulated label).
func (MirrorFetcher) Source() string { return SourceMirror }

// FetchOffer synthesizes a contract-valid offer: OCEAN_FREIGHT plus an
// ORIGIN_CHARGE (so the payload satisfies the engine's strict coverage
// for every owed-freight Incoterm, EXW/FCA included) in USD, expiring
// thirty days after the request date. Amounts derive deterministically
// from the request digest — never random: a quote priced twice off the
// same checkpoint sees identical lines.
func (m MirrorFetcher) FetchOffer(_ context.Context, req Request) (*Result, error) {
	hash := RequestHash(req)
	payload, err := json.Marshal(m.offer(req, hash))
	if err != nil {
		return nil, fmt.Errorf("carrier: mirror marshal: %w", err)
	}
	ref := "MIR-" + hash[:8]
	return &Result{Offer: payload, OfferReference: &ref}, nil
}

// offer builds the synthetic payload: two seeded amounts in bounded,
// plausible ranges — the point is a stable, valid offer, not a real rate.
func (MirrorFetcher) offer(req Request, hash string) OfferPayload {
	return OfferPayload{
		Carrier:   mirrorCarrier,
		ExpiresAt: req.ReadyDate.AddDate(0, 0, 30),
		Charges: []pricing.OfferCharge{
			{
				Category:        pricing.ChargeOceanFreight,
				Code:            "OCEAN",
				Amount:          seededAmount(hash[0:8], 900, 3200),
				Currency:        "USD",
				EquipmentOrUnit: req.EquipmentOrUnit,
			},
			{
				Category:        pricing.ChargeOriginCharge,
				Code:            "THC_ORIGIN",
				Amount:          seededAmount(hash[8:16], 150, 550),
				Currency:        "USD",
				EquipmentOrUnit: req.EquipmentOrUnit,
			},
		},
	}
}

// seededAmount maps eight hex chars of the request digest into
// [base, base+span) — the same slice always yields the same amount.
func seededAmount(hexSeed string, base, span int64) decimal.Decimal {
	var n int64
	for _, c := range hexSeed {
		n = n*16 + int64(hexNibble(c))
	}
	return decimal.NewFromInt(base + n%span)
}

// hexNibble decodes one hex digit (RequestHash emits lowercase only).
func hexNibble(c rune) int64 {
	if c >= 'a' {
		return int64(c-'a') + 10
	}
	return int64(c - '0')
}
