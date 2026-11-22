package carrier_test

// Unit tests for the S3a carrier seam: the request_hash digest, the
// sanity bounds run on untrusted payloads, the deterministic mirror and
// the LIVE fetcher's wire passthrough.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/aiengine"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/carrier"
)

var readyDate = time.Date(2026, 3, 15, 0, 0, 0, 0, time.UTC)

func testRequest() carrier.Request {
	return carrier.Request{
		CorrelationID:   uuid.New(),
		OriginLOCODE:    "CNSHA",
		DestLOCODE:      "BRSSZ",
		EquipmentOrUnit: "40HC",
		Quantity:        2,
		ReadyDate:       readyDate,
	}
}

// TestRequestHashStable: the digest depends on the normalized request
// content only — same fields hash identically regardless of correlation
// id; route, equipment, count or date changes all yield a new hash (the
// unique key's replay half, migration 0010).
func TestRequestHashStable(t *testing.T) {
	a, b := testRequest(), testRequest()
	assert.Equal(t, carrier.RequestHash(a), carrier.RequestHash(b))
	b.CorrelationID = uuid.New()
	assert.Equal(t, carrier.RequestHash(a), carrier.RequestHash(b),
		"correlation_id is entropy, not request content — it must not rekey the digest")

	for _, mutate := range []func(*carrier.Request){
		func(r *carrier.Request) { r.OriginLOCODE = "DEHAM" },
		func(r *carrier.Request) { r.DestLOCODE = "BRRIO" },
		func(r *carrier.Request) { r.EquipmentOrUnit = "20GP" },
		func(r *carrier.Request) { r.Quantity = 3 },
		func(r *carrier.Request) { r.ReadyDate = readyDate.AddDate(0, 0, 1) },
	} {
		c := testRequest()
		mutate(&c)
		assert.NotEqual(t, carrier.RequestHash(a), carrier.RequestHash(c),
			"a changed normalized field must yield a new request_hash")
	}
	assert.Len(t, carrier.RequestHash(a), 64, "sha256 hex")
}

// validOfferJSON is a minimal contract-valid payload.
const validOfferJSON = `{
	"carrier": "CMA_CGM",
	"charges": [{"category":"OCEAN_FREIGHT","code":"OCEAN","amount":900.00,"currency":"USD","equipment_or_unit":"40HC"}],
	"expires_at": "2026-04-01T00:00:00Z"}`

// TestValidateOffer pins the sanity bounds table (rule 6): every case
// names the bound it breaks.
func TestValidateOffer(t *testing.T) {
	cases := map[string]string{
		"not an object":      `[{"x":1}]`,
		"empty object":       `{}`,
		"missing carrier":    `{"charges":[{"category":"OCEAN_FREIGHT","code":"O","amount":1,"currency":"USD","equipment_or_unit":"40HC"}],"expires_at":"2026-04-01T00:00:00Z"}`,
		"missing expires_at": `{"carrier":"CMA_CGM","charges":[{"category":"OCEAN_FREIGHT","code":"O","amount":1,"currency":"USD","equipment_or_unit":"40HC"}]}`,
		"null expires_at":    `{"carrier":"C","charges":[{"category":"OCEAN_FREIGHT","code":"O","amount":1,"currency":"USD","equipment_or_unit":"40HC"}],"expires_at":null}`,
		"empty charges":      `{"carrier":"C","charges":[],"expires_at":"2026-04-01T00:00:00Z"}`,
		"null charges":       `{"carrier":"C","expires_at":"2026-04-01T00:00:00Z"}`,
		"bad category":       `{"carrier":"C","charges":[{"category":"NINE","code":"O","amount":1,"currency":"USD","equipment_or_unit":"40HC"}],"expires_at":"2026-04-01T00:00:00Z"}`,
		"empty code":         `{"carrier":"C","charges":[{"category":"OCEAN_FREIGHT","code":"","amount":1,"currency":"USD","equipment_or_unit":"40HC"}],"expires_at":"2026-04-01T00:00:00Z"}`,
		"zero amount":        `{"carrier":"C","charges":[{"category":"OCEAN_FREIGHT","code":"O","amount":0,"currency":"USD","equipment_or_unit":"40HC"}],"expires_at":"2026-04-01T00:00:00Z"}`,
		"negative amount":    `{"carrier":"C","charges":[{"category":"OCEAN_FREIGHT","code":"O","amount":-5,"currency":"USD","equipment_or_unit":"40HC"}],"expires_at":"2026-04-01T00:00:00Z"}`,
		"bad currency":       `{"carrier":"C","charges":[{"category":"OCEAN_FREIGHT","code":"O","amount":1,"currency":"usd","equipment_or_unit":"40HC"}],"expires_at":"2026-04-01T00:00:00Z"}`,
		"empty equipment":    `{"carrier":"C","charges":[{"category":"OCEAN_FREIGHT","code":"O","amount":1,"currency":"USD","equipment_or_unit":""}],"expires_at":"2026-04-01T00:00:00Z"}`,
		"bad amount type":    `{"carrier":"C","charges":[{"category":"OCEAN_FREIGHT","code":"O","amount":"abc","currency":"USD","equipment_or_unit":"40HC"}],"expires_at":"2026-04-01T00:00:00Z"}`,
		"bad expires format": `{"carrier":"C","charges":[{"category":"OCEAN_FREIGHT","code":"O","amount":1,"currency":"USD","equipment_or_unit":"40HC"}],"expires_at":"next week"}`,
	}
	for name, raw := range cases {
		_, err := carrier.ValidateOffer([]byte(raw))
		require.Error(t, err, name)
		var v *carrier.OfferViolation
		require.True(t, errors.As(err, &v), "%s must be an OfferViolation, got %T", name, err)
		assert.False(t, v.Transient(), "%s must never be transient", name)
	}
}

// TestValidateOfferEdgeBounds: the two numeric caps — >50 charges and
// a >64 KiB payload — and a >64-char code.
func TestValidateOfferEdgeBounds(t *testing.T) {
	charges := `{"category":"OCEAN_FREIGHT","code":"O","amount":1,"currency":"USD","equipment_or_unit":"40HC"}`
	many := `{"carrier":"C","expires_at":"2026-04-01T00:00:00Z","charges":[` + strings.Repeat(charges+",", 50) + charges + `]}`
	_, err := carrier.ValidateOffer([]byte(many))
	require.Error(t, err, "51 charges must violate the cap")

	longCode := `{"carrier":"C","expires_at":"2026-04-01T00:00:00Z","charges":[{"category":"OCEAN_FREIGHT","code":"` + strings.Repeat("X", 65) + `","amount":1,"currency":"USD","equipment_or_unit":"40HC"}]}`
	_, err = carrier.ValidateOffer([]byte(longCode))
	require.Error(t, err, "a 65-char code must violate the bound")

	huge := `{"carrier":"C","expires_at":"2026-04-01T00:00:00Z","pad":"` + strings.Repeat("x", 64*1024) + `","charges":[]}`
	_, err = carrier.ValidateOffer([]byte(huge))
	require.Error(t, err, "a >64KiB payload must violate the bound")
}

// TestValidateOfferAcceptsValid: the seeded-fixture shape passes and
// decodes into the persisted shape (pricing.CarrierOffer minus id).
func TestValidateOfferAcceptsValid(t *testing.T) {
	p, err := carrier.ValidateOffer([]byte(validOfferJSON))
	require.NoError(t, err)
	assert.Equal(t, "CMA_CGM", p.Carrier)
	require.Len(t, p.Charges, 1)
	assert.Equal(t, "OCEAN_FREIGHT", string(p.Charges[0].Category))
	assert.Equal(t, "900", p.Charges[0].Amount.String())
	assert.Equal(t, time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC), p.ExpiresAt.UTC())
}

// TestIsNoOfferPayload: absent, empty and JSON null are all NO_OFFER.
func TestIsNoOfferPayload(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, []byte(""), []byte("null"), []byte(" null ")} {
		assert.True(t, carrier.IsNoOfferPayload(raw), "%q", raw)
	}
	assert.False(t, carrier.IsNoOfferPayload([]byte(validOfferJSON)))
}

// TestMirrorFetcherDeterministic: same request ⇒ same offer bytes; the
// payload passes the same sanity bounds any live offer must; a changed
// request yields a different offer (reference and digest differ).
func TestMirrorFetcherDeterministic(t *testing.T) {
	m := carrier.MirrorFetcher{}
	req := testRequest()
	first, err := m.FetchOffer(context.Background(), req)
	require.NoError(t, err)
	second, err := m.FetchOffer(context.Background(), testRequest())
	require.NoError(t, err)
	assert.Equal(t, first.Offer, second.Offer, "same request must yield identical offer bytes")
	require.NotNil(t, first.OfferReference)
	assert.Equal(t, first.OfferReference, second.OfferReference)
	assert.Equal(t, "MIRROR", m.Carrier())
	assert.Equal(t, carrier.SourceMirror, m.Source())

	p, err := carrier.ValidateOffer(first.Offer)
	require.NoError(t, err, "the mirror must produce sanity-bound-valid payloads")
	assert.Equal(t, "MIRROR", p.Carrier)
	assert.Equal(t, readyDate.AddDate(0, 0, 30), p.ExpiresAt.UTC())
	cats := map[string]bool{}
	for _, c := range p.Charges {
		cats[string(c.Category)] = true
		assert.Equal(t, req.EquipmentOrUnit, c.EquipmentOrUnit)
	}
	assert.True(t, cats["OCEAN_FREIGHT"] && cats["ORIGIN_CHARGE"],
		"the mirror covers freight + origin charges for every owed Incoterm")

	other := req
	other.DestLOCODE = "BRRIO"
	diff, err := m.FetchOffer(context.Background(), other)
	require.NoError(t, err)
	assert.NotEqual(t, first.Offer, diff.Offer, "a different request must yield a different offer")
}

// fakeSearcher is the named fake for carrier.Searcher — records the wire
// request and answers with a canned result or error.
type fakeSearcher struct {
	res *aiengine.CarrierOfferSearchResult
	err error

	calls int
	last  aiengine.CarrierOfferSearchRequest
}

func (f *fakeSearcher) SearchCarrierOffer(_ context.Context, req aiengine.CarrierOfferSearchRequest) (*aiengine.CarrierOfferSearchResult, error) {
	f.calls++
	f.last = req
	if f.err != nil {
		return nil, f.err
	}
	return f.res, nil
}

// TestLiveFetcherPassthrough: the wire request maps verbatim (ISO date,
// no organization_id — the type carries none), the raw offer bytes pass
// through untouched for the caller's validation, and a null offer is
// handed up as the NO_OFFER shape.
func TestLiveFetcherPassthrough(t *testing.T) {
	ref := "CMA-123"
	searcher := &fakeSearcher{res: &aiengine.CarrierOfferSearchResult{
		Offer:          []byte(validOfferJSON),
		OfferReference: &ref,
	}}
	f := carrier.NewLiveFetcher(searcher, "CMA_CGM")
	assert.Equal(t, "CMA_CGM", f.Carrier())
	assert.Equal(t, carrier.SourceLive, f.Source())

	req := testRequest()
	res, err := f.FetchOffer(context.Background(), req)
	require.NoError(t, err)
	require.Equal(t, 1, searcher.calls)
	got := searcher.last
	assert.Equal(t, req.CorrelationID, got.CorrelationID)
	assert.Equal(t, "CNSHA", got.OriginLOCODE)
	assert.Equal(t, "BRSSZ", got.DestLOCODE)
	assert.Equal(t, "40HC", got.EquipmentOrUnit)
	assert.Equal(t, 2, got.Quantity)
	assert.Equal(t, "2026-03-15", got.ReadyDate, "ready_date travels as IsoDate")
	assert.JSONEq(t, validOfferJSON, string(res.Offer))
	require.NotNil(t, res.OfferReference)
	assert.Equal(t, ref, *res.OfferReference)
}

// TestLiveFetcherNoOfferAndErrors: a null wire offer is NO_OFFER-shaped
// and a searcher error propagates unwrapped (the EngineError taxonomy
// reaches the worker intact).
func TestLiveFetcherNoOfferAndErrors(t *testing.T) {
	searcher := &fakeSearcher{res: &aiengine.CarrierOfferSearchResult{Offer: []byte("null")}}
	f := carrier.NewLiveFetcher(searcher, "CMA_CGM")
	res, err := f.FetchOffer(context.Background(), testRequest())
	require.NoError(t, err)
	assert.True(t, carrier.IsNoOfferPayload(res.Offer))

	fErr := errors.New("boom")
	searcher = &fakeSearcher{err: fErr}
	f = carrier.NewLiveFetcher(searcher, "CMA_CGM")
	_, err = f.FetchOffer(context.Background(), testRequest())
	require.ErrorIs(t, err, fErr)
}
