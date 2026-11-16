# hapag_lloyd connector

Client for the Hapag-Lloyd **Quotation/Booking Engine — External Prices** API
("Prices API", OpenAPI v2.1.3). Only `POST /prices` is implemented: QuotePilot
fetches carrier rate offers here and generates its own quotations, so the
API's `/quotations` endpoint is out of scope by design (RFC-001 stage 4A).

## Usage

```python
async with HapagLloydClient.from_env() as client:
    result = await client.get_offers(offer_request)
    for offer in result.response.offers or []:
        ...  # offer.equipments[*].rates[*].amount is Decimal, never float
```

## Configuration (env vars)

| Var | Required | Default |
|---|---|---|
| `HAPAG_CLIENT_ID` | yes | — application Client-ID from api-portal.hlag.com |
| `HAPAG_CLIENT_SECRET` | yes | — application Client-Secret |
| `HAPAG_BASE_URL` | no | `https://api.hlag.com/hlag/external/v2/quotation-booking-engine/external` |
| `HAPAG_TIMEOUT_SECONDS` | no | `30` |

The vars must also be registered (names only) in the root `.env.example` —
outside Devin's scope, so a maintainer adds them.

## Errors

`HapagTransportError` (network), `HapagAuthError` (401/403),
`HapagApiError` (other non-201, carries the parsed RFC 7807 `Problem`),
`HapagSchemaError` (201 body violating the schema), `HapagConfigError`.

No retries and no payload logging on purpose — River owns retry policy and
tenant content must not reach logs.

## Tests

Synthetic fixtures in `tests/fixtures/hapag_lloyd/` (real recorded offers are
not committed per RFC-003 Q9 / ADR-008). `FakeHapagTransport` injects canned
responses — no live network.
