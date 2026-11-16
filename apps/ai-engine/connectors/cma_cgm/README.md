# cma_cgm connector

Client for the CMA CGM **API SpotOn** (`pricing.commercial.instantquote.v2`,
OpenAPI v2.7.0, from api-portal.cma-cgm.com). Only `POST /spotOn/search` is
implemented: QuotePilot fetches carrier spot offers here and generates its own
quotations, so the API's `POST /offers/{offerId}/quotations` is out of scope
by design (same rationale as the Hapag-Lloyd `/quotations` endpoint).

## Usage

```python
async with CmaCgmClient.from_env() as client:
    result = await client.search_offers(spot_request)
    for offer in result.offers:  # SpotOffer items
        ...  # offer.quote_line.* rates are Decimal, never float
    for message in result.messages:  # Message items (no-offer reasons)
        ...
    if result.partial:  # HTTP 206: more rows via range_
        ...
```

## Auth

OAuth2 client-credentials (RFC 6749 §4.4): the client exchanges
`Basic base64(id:secret)` for a bearer token at
`https://auth.cma-cgm.com/as/token.oauth2` with scope
`instantquote:read:be`, caches it in memory, and refetches it 30 s before its
nominal expiry. The clock is injectable for tests.

## Configuration (env vars)

| Var | Required | Default |
|---|---|---|
| `CMA_CGM_CLIENT_ID` | yes | — OAuth client id from api-portal.cma-cgm.com |
| `CMA_CGM_CLIENT_SECRET` | yes | — OAuth client secret |
| `CMA_CGM_BASE_URL` | no | `https://apis.cma-cgm.net/pricing/commercial/instantquote/v2` |
| `CMA_CGM_TOKEN_URL` | no | `https://auth.cma-cgm.com/as/token.oauth2` |
| `CMA_CGM_TIMEOUT_SECONDS` | no | `30` |

The vars are registered (names only) in the root `.env.example`.

## Search options

`search_offers(request, *, behalf_of=None, range_=None)`:

- `behalf_of` — `?behalfOf=` query param; mandatory for third-party partners
  (end-customer code from the referential API).
- `range_` — `range` header for pagination (spec default `0-4`, max 5 rows
  per page). A `206` reply means more rows exist.

## Errors

`CmaCgmTransportError` (network), `CmaCgmAuthError` (token endpoint non-200 or
search 401/403), `CmaCgmApiError` (400/416/500/503 and other errors, carries
the parsed `Fault`), `CmaCgmSchemaError` (success body violating the schema),
`CmaCgmConfigError`.

No retries and no payload logging on purpose — River owns retry policy and
tenant content must not reach logs.

## Tests

Synthetic fixtures in `tests/fixtures/cma_cgm/` (real recorded offers are not
committed per RFC-003 Q9 / ADR-008). `FakeCmaCgmTransport` injects canned
responses for both the token and search endpoints — no live network.
