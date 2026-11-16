"""Async client for the Firecrawl v2 ``POST /scrape`` API.

Calls ``POST /scrape`` to fetch a page rendered as markdown — QuotePilot uses
it to pull carrier tariff pages into text the extraction agents can parse.
The response is untrusted input: it is schema-validated into pydantic models
before reaching the caller.

Beware the envelope semantics: Firecrawl answers HTTP 200 with
``success: true`` even when the *target* page returned non-2xx — the scraped
page's own status lives in ``data.metadata.statusCode``. Check
``ScrapeResult.page_failed`` before trusting the markdown.

The connector does no retries and no payload logging: retry/backoff policy
belongs to the River stage that calls ai-engine (ADR-004), and tenant content
must not reach logs (ADR-001).
"""

from __future__ import annotations

import os
from collections.abc import Mapping
from dataclasses import dataclass
from types import TracebackType
from typing import Self

import httpx
from pydantic import ValidationError

from connectors._common import parse_timeout_seconds
from connectors.firecrawl.errors import (
    FirecrawlApiError,
    FirecrawlAuthError,
    FirecrawlConfigError,
    FirecrawlSchemaError,
    FirecrawlTransportError,
)
from connectors.firecrawl.models import (
    ScrapeErrorBody,
    ScrapeMetadata,
    ScrapeResponse,
)

PROD_BASE_URL = "https://api.firecrawl.dev/v2"
DEFAULT_TIMEOUT_SECONDS = 60.0

_ENV_API_KEY = "FIRECRAWL_API_KEY"
_ENV_BASE_URL = "FIRECRAWL_BASE_URL"
_ENV_TIMEOUT = "FIRECRAWL_TIMEOUT_SECONDS"


@dataclass(frozen=True)
class ScrapeResult:
    """A parsed ``POST /scrape`` reply plus the raw HTTP status."""

    markdown: str
    metadata: ScrapeMetadata
    raw_status_code: int

    @property
    def page_failed(self) -> bool:
        """True when the scraped page itself returned an HTTP error (>= 400).

        Firecrawl wraps dead pages in a 200 envelope, so this is the only
        reliable way to detect a dead tariff page.
        """
        status = self.metadata.status_code
        return status is not None and status >= 400


class FirecrawlClient:
    """Client for the Firecrawl v2 scrape endpoint.

    Args:
        api_key: Bearer key from the firecrawl.dev dashboard.
        base_url: API root; defaults to ``https://api.firecrawl.dev/v2``.
        timeout_seconds: total request timeout (default 60 — scrapes are
            slower than API calls); retries are the caller's job.
        transport: httpx transport override — tests inject a fake here.
        http_client: shared ``httpx.AsyncClient``; when given, ``base_url``,
            ``timeout_seconds`` and ``transport`` are ignored and the client is
            not closed by ``aclose()``. The auth header is still sent
            per-request, so an injected client does not need it configured.

    Example:
        >>> async with FirecrawlClient.from_env() as client:
        ...     result = await client.scrape_markdown("https://carrier.example/tariff")
        ...     if not result.page_failed:
        ...         markdown = result.markdown
    """

    def __init__(
        self,
        api_key: str,
        *,
        base_url: str = PROD_BASE_URL,
        timeout_seconds: float = DEFAULT_TIMEOUT_SECONDS,
        transport: httpx.AsyncBaseTransport | None = None,
        http_client: httpx.AsyncClient | None = None,
    ) -> None:
        if not api_key:
            raise FirecrawlConfigError(
                "api_key must be non-empty; expected a key from the "
                "firecrawl.dev dashboard"
            )
        # Auth goes on each request so an injected http_client works as-is.
        self._request_headers = {
            "Authorization": f"Bearer {api_key}",
            "Accept": "application/json",
        }
        if http_client is not None:
            self._client = http_client
            self._owns_client = False
        else:
            self._client = httpx.AsyncClient(
                base_url=base_url,
                timeout=timeout_seconds,
                transport=transport,
            )
            self._owns_client = True

    @classmethod
    def from_env(
        cls,
        env: Mapping[str, str] | None = None,
        *,
        transport: httpx.AsyncBaseTransport | None = None,
        http_client: httpx.AsyncClient | None = None,
    ) -> Self:
        """Build a client from ``FIRECRAWL_*`` environment variables.

        Required: ``FIRECRAWL_API_KEY``.
        Optional: ``FIRECRAWL_BASE_URL`` (default
        ``https://api.firecrawl.dev/v2``), ``FIRECRAWL_TIMEOUT_SECONDS``
        (default 60).

        Example:
            >>> client = FirecrawlClient.from_env()  # reads os.environ
        """
        source = os.environ if env is None else env
        if not source.get(_ENV_API_KEY):
            raise FirecrawlConfigError(
                f"missing required environment variable: {_ENV_API_KEY}; "
                "expected an API key from the firecrawl.dev dashboard"
            )
        timeout = parse_timeout_seconds(
            source.get(_ENV_TIMEOUT),
            env_name=_ENV_TIMEOUT,
            default=DEFAULT_TIMEOUT_SECONDS,
            error_type=FirecrawlConfigError,
        )
        return cls(
            api_key=source[_ENV_API_KEY],
            # ``or`` so an exported-but-empty FIRECRAWL_BASE_URL= means prod.
            base_url=source.get(_ENV_BASE_URL) or PROD_BASE_URL,
            timeout_seconds=timeout,
            transport=transport,
            http_client=http_client,
        )

    async def scrape_markdown(
        self,
        url: str,
        *,
        only_main_content: bool = True,
        timeout_seconds: float | None = None,
    ) -> ScrapeResult:
        """Scrape ``url`` and return its markdown (``POST /scrape``).

        Args:
            url: page to scrape (carrier tariff page, schedule page, ...).
            only_main_content: strip nav/footers; keep ``True`` for tariffs.
            timeout_seconds: per-call timeout override; ``None`` keeps the
                client-level default.

        Returns:
            ``ScrapeResult`` with the page markdown, its metadata and the raw
            HTTP status. Check ``result.page_failed`` — a 200 reply can still
            wrap a dead target page.

        Raises:
            FirecrawlApiError: non-2xx response (402/429/5xx and others),
                carrying the parsed error body.
            FirecrawlAuthError: 401/403 — check the API key.
            FirecrawlSchemaError: 200 body violating the expected envelope
                (missing ``success``/``data``/``markdown``, or
                ``success: false``).
            FirecrawlTransportError: DNS/TLS/timeout/protocol failure.
        """
        raw = await self._post_scrape(url, only_main_content, timeout_seconds)
        if raw.status_code != 200:
            raise _to_api_error(raw)
        envelope = _validated_scrape(raw)
        if not envelope.success:
            raise FirecrawlSchemaError(
                "POST scrape returned 200 but success is false; the body "
                f"does not match the expected scrape envelope: {raw.text[:200]}"
            )
        return ScrapeResult(
            markdown=envelope.data.markdown,
            metadata=envelope.data.metadata,
            raw_status_code=raw.status_code,
        )

    async def _post_scrape(
        self,
        url: str,
        only_main_content: bool,
        timeout_seconds: float | None,
    ) -> httpx.Response:
        try:
            return await self._client.post(
                "scrape",
                json={
                    "url": url,
                    "formats": ["markdown"],
                    "onlyMainContent": only_main_content,
                },
                headers=self._request_headers,
                timeout=(
                    timeout_seconds
                    if timeout_seconds is not None
                    else httpx.USE_CLIENT_DEFAULT
                ),
            )
        except httpx.HTTPError as exc:
            raise FirecrawlTransportError(
                f"POST {self._client.base_url}scrape transport failure: {exc}"
            ) from exc

    async def aclose(self) -> None:
        """Close the underlying HTTP client if this instance created it."""
        if self._owns_client:
            await self._client.aclose()

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        exc_value: BaseException | None,
        traceback: TracebackType | None,
    ) -> None:
        await self.aclose()


def _validated_scrape(response: httpx.Response) -> ScrapeResponse:
    try:
        return ScrapeResponse.model_validate_json(response.text)
    except ValidationError as exc:
        raise FirecrawlSchemaError(
            "POST scrape returned 200 but the body violates the expected "
            f"scrape envelope: {exc}"
        ) from exc


def _to_api_error(response: httpx.Response) -> FirecrawlApiError:
    try:
        body = ScrapeErrorBody.model_validate_json(response.text)
    except ValidationError:
        body = None
    summary = ""
    if body:
        summary = body.error or body.message or body.code or ""
    message = (
        f"POST scrape failed: HTTP {response.status_code} "
        f"— {summary or response.text[:200] or 'no details in response'}"
    )
    if response.status_code in (401, 403):
        return FirecrawlAuthError(message, status=response.status_code, body=body)
    return FirecrawlApiError(message, status=response.status_code, body=body)
