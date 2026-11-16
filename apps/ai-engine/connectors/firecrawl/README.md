# firecrawl connector

Client for the **Firecrawl v2** API (`api.firecrawl.dev`). Only `POST /scrape`
with `formats: ["markdown"]` is implemented: QuotePilot scrapes carrier tariff
pages into markdown for the extraction agents. Crawl, batch scrape, map and
the other Firecrawl endpoints are out of scope for now.

## Usage

```python
async with FirecrawlClient.from_env() as client:
    result = await client.scrape_markdown("https://carrier.example/tariff")
    if result.page_failed:
        ...  # 200 envelope, but the target page itself returned >= 400
    markdown = result.markdown
```

Beware: Firecrawl answers HTTP 200 with `success: true` even when the *target*
page failed — the scraped page's status lives in
`result.metadata.status_code`. Always check `result.page_failed` before
trusting the markdown.

## Configuration (env vars)

| Var | Required | Default |
|---|---|---|
| `FIRECRAWL_API_KEY` | yes | — API key from the firecrawl.dev dashboard |
| `FIRECRAWL_BASE_URL` | no | `https://api.firecrawl.dev/v2` |
| `FIRECRAWL_TIMEOUT_SECONDS` | no | `60` (scrapes are slower than API calls) |

The vars are also registered (names only) in the root `.env.example`.

## Errors

`FirecrawlTransportError` (network), `FirecrawlAuthError` (401/403),
`FirecrawlApiError` (other non-2xx — 402/429/5xx — carrying the parsed error
body), `FirecrawlSchemaError` (200 body violating the scrape envelope),
`FirecrawlConfigError`.

No retries and no payload logging on purpose — River owns retry policy and
tenant content must not reach logs.

## Tests

Synthetic fixtures in `tests/fixtures/firecrawl/old/` (real recorded pages are
not committed per RFC-003 Q9 / ADR-008). `FakeFirecrawlTransport` injects
canned responses — no live network.
