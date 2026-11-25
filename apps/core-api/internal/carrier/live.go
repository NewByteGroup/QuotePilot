package carrier

// live.go — the LIVE binding: the carrier-offer search over the shared
// ai-engine transport (X-Internal-Service-Key, correlation_id, bounded
// body, the transient taxonomy — timeout/408/425/429/5xx transient, other
// 4xx and invalid payloads contract violations). The wire returns the raw
// offer bytes verbatim; the worker bounds them before persisting.

import (
	"context"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/aiengine"
)

// Searcher is the narrow ai-engine slice the live binding consumes —
// *aiengine.HTTPClient and aiengine.UnconfiguredClient satisfy it, and
// tests bind a named fake. It stays separate from aiengine.Client so the
// S2 fakes are untouched by the new endpoint.
type Searcher interface {
	SearchCarrierOffer(ctx context.Context, req aiengine.CarrierOfferSearchRequest) (*aiengine.CarrierOfferSearchResult, error)
}

// LiveFetcher is the LIVE binding: one carrier per process (pilot: CMA
// CGM — the first approved real connector, spec S3a connector order).
type LiveFetcher struct {
	searcher Searcher
	carrier  string
}

// NewLiveFetcher binds the searcher to a carrier name — the value the
// checkpoint's carrier column carries on every outcome row.
//
//	live := carrier.NewLiveFetcher(aiengine.NewHTTPClient(url, tok), "CMA_CGM")
func NewLiveFetcher(searcher Searcher, carrierName string) *LiveFetcher {
	return &LiveFetcher{searcher: searcher, carrier: carrierName}
}

// Carrier returns the binding's carrier name (the checkpoint key).
func (f *LiveFetcher) Carrier() string { return f.carrier }

// Source reports LIVE (migration 0010's source vocabulary).
func (f *LiveFetcher) Source() string { return SourceLive }

// FetchOffer runs the provisional search: the normalized request travels
// as correlation_id + route/equipment/count/date — never organization_id
// (rule 2). A null offer key is NO_OFFER; everything else hands the raw
// bytes up for the caller's ValidateOffer pass (rule 6).
func (f *LiveFetcher) FetchOffer(ctx context.Context, req Request) (*Result, error) {
	res, err := f.searcher.SearchCarrierOffer(ctx, aiengine.CarrierOfferSearchRequest{
		CorrelationID:   req.CorrelationID,
		OriginLOCODE:    req.OriginLOCODE,
		DestLOCODE:      req.DestLOCODE,
		EquipmentOrUnit: req.EquipmentOrUnit,
		Quantity:        req.Quantity,
		ReadyDate:       req.ReadyDate.Format(isoDate),
	})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return &Result{}, nil
	}
	return &Result{Offer: res.Offer, OfferReference: res.OfferReference}, nil
}
