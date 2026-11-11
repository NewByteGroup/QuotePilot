"""Async client for the Hapag-Lloyd QBE "External Prices" API.

Calls ``POST /prices`` to fetch bookable rate offers (Quick Quotes and
Quick Quotes Spot). The response is untrusted input: it is schema-validated
into pydantic models before reaching the caller, and monetary amounts arrive
as ``Decimal`` — never floats.

The connector does no retries and no payload logging: retry/backoff policy
belongs to the River stage that calls ai-engine (ADR-004), and tenant content
must not reach logs (ADR-001).
"""

from __future__ import annotations

import math
import os
from collections.abc import Mapping
from dataclasses import dataclass
from datetime import UTC, datetime
from email.utils import parsedate_to_datetime
from types import TracebackType
from typing import Self

import httpx
from pydantic import ValidationError

from connectors.hapag_lloyd.errors import (
    HapagApiError,
    HapagAuthError,
    HapagConfigError,
    HapagSchemaError,
    HapagTransportError,
)
from connectors.hapag_lloyd.models import OfferRequest, OfferResponse, Problem

PROD_BASE_URL = (
    "https://api.hlag.com/hlag/external/v2/quotation-booking-engine/external"
)
MOCK_BASE_URL = "https://mock.api-portal.hlag.com/v2/quotation-booking-engine/external"
DEFAULT_TIMEOUT_SECONDS = 30.0

_ENV_CLIENT_ID = "HAPAG_CLIENT_ID"
_ENV_CLIENT_SECRET = "HAPAG_CLIENT_SECRET"
_ENV_BASE_URL = "HAPAG_BASE_URL"
_ENV_TIMEOUT = "HAPAG_TIMEOUT_SECONDS"


@dataclass(frozen=True)
class OfferResult:
    """A parsed ``POST /prices`` reply plus its response headers."""

    response: OfferResponse
    location: str | None
    expires: datetime | None


class HapagLloydClient:
    """Client for the External Prices API.

    Args:
        client_id: application Client-ID from api-portal.hlag.com.
        client_secret: application Client-Secret for the subscription.
        base_url: API root; defaults to production, use ``MOCK_BASE_URL``
            against Hapag-Lloyd's mock server.
        timeout_seconds: total request timeout; retries are the caller's job.
        transport: httpx transport override — tests inject a fake here.
        http_client: shared ``httpx.AsyncClient``; when given, ``base_url``,
            ``timeout_seconds`` and ``transport`` are ignored and the client is
            not closed by ``aclose()``. Auth headers are still sent per-request,
            so an injected client does not need them configured.

    Example:
        >>> async with HapagLloydClient.from_env() as client:
        ...     result = await client.get_offers(offer_request)
        ...     offers = result.response.offers or []
    """

    def __init__(
        self,
        client_id: str,
        client_secret: str,
        *,
        base_url: str = PROD_BASE_URL,
        timeout_seconds: float = DEFAULT_TIMEOUT_SECONDS,
        transport: httpx.AsyncBaseTransport | None = None,
        http_client: httpx.AsyncClient | None = None,
    ) -> None:
        if not client_id or not client_secret:
            raise HapagConfigError(
                "client_id and client_secret must be non-empty; "
                "expected credentials from an api-portal.hlag.com application"
            )
        # Auth goes on each request so an injected http_client works as-is.
        self._request_headers = {
            "X-IBM-Client-Id": client_id,
            "X-IBM-Client-Secret": client_secret,
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
        """Build a client from ``HAPAG_*`` environment variables.

        Required: ``HAPAG_CLIENT_ID``, ``HAPAG_CLIENT_SECRET``.
        Optional: ``HAPAG_BASE_URL`` (default production),
        ``HAPAG_TIMEOUT_SECONDS`` (default 30).

        Example:
            >>> client = HapagLloydClient.from_env()  # reads os.environ
        """
        source = os.environ if env is None else env
        missing = [
            name
            for name in (_ENV_CLIENT_ID, _ENV_CLIENT_SECRET)
            if not source.get(name)
        ]
        if missing:
            raise HapagConfigError(
                f"missing required environment variables: {', '.join(missing)}; "
                "expected the Client-ID/Client-Secret pair issued by an "
                "api-portal.hlag.com application subscription"
            )
        timeout = _timeout_from_env(source.get(_ENV_TIMEOUT))
        return cls(
            client_id=source[_ENV_CLIENT_ID],
            client_secret=source[_ENV_CLIENT_SECRET],
            base_url=source.get(_ENV_BASE_URL, PROD_BASE_URL),
            timeout_seconds=timeout,
            transport=transport,
            http_client=http_client,
        )

    async def get_offers(self, request: OfferRequest) -> OfferResult:
        """Create and return price offers for a routing (``POST /prices``).

        Args:
            request: validated offer request (locations, equipment, commodity,
                dates, product identifiers and customer e-mail).

        Returns:
            ``OfferResult`` with the parsed ``OfferResponse`` plus the
            ``Location``/``Expires`` headers Hapag-Lloyd sets on 201.

        Raises:
            HapagApiError: non-2xx response with a parseable Problem body.
            HapagAuthError: 401/403 — check the application credentials.
            HapagSchemaError: success body violating the published schema.
            HapagTransportError: DNS/TLS/timeout/protocol failure.
        """
        raw = await self._post_prices(request)
        # The published spec declares 201; the official mock server answers
        # 200. Both carry an OfferResponse body, so accept either.
        if raw.status_code not in (200, 201):
            raise _to_api_error(raw)
        return OfferResult(
            response=_validated_offers(raw),
            location=raw.headers.get("location"),
            expires=_parse_http_date(raw.headers.get("expires")),
        )

    async def _post_prices(self, request: OfferRequest) -> httpx.Response:
        try:
            return await self._client.post(
                "prices",
                content=request.model_dump_json(by_alias=True, exclude_none=True),
                headers={
                    **self._request_headers,
                    "Content-Type": "application/json",
                },
            )
        except httpx.HTTPError as exc:
            raise HapagTransportError(
                f"POST {self._client.base_url}prices transport failure: {exc}"
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


def _timeout_from_env(raw: str | None) -> float:
    if not raw:
        return DEFAULT_TIMEOUT_SECONDS
    try:
        timeout = float(raw)
    except ValueError as exc:
        raise HapagConfigError(
            f"{_ENV_TIMEOUT} must be a number of seconds, got {raw!r}"
        ) from exc
    if not math.isfinite(timeout) or timeout <= 0:
        raise HapagConfigError(
            f"{_ENV_TIMEOUT} must be a finite number > 0, got {raw!r}"
        )
    return timeout


def _validated_offers(response: httpx.Response) -> OfferResponse:
    try:
        return OfferResponse.model_validate_json(response.text)
    except ValidationError as exc:
        raise HapagSchemaError(
            "POST prices returned 201 but the body violates the "
            f"OfferResponse schema: {exc}"
        ) from exc


def _to_api_error(response: httpx.Response) -> HapagApiError:
    try:
        problem = Problem.model_validate_json(response.text)
    except ValidationError:
        problem = None
    summary = (problem.detail or problem.title) if problem else ""
    message = (
        f"POST prices failed: HTTP {response.status_code} "
        f"— {summary or response.text[:200] or 'no details in response'}"
    )
    if response.status_code in (401, 403):
        return HapagAuthError(message, status=response.status_code, problem=problem)
    return HapagApiError(message, status=response.status_code, problem=problem)


def _parse_http_date(value: str | None) -> datetime | None:
    if not value:
        return None
    parsed: datetime | None
    try:
        parsed = parsedate_to_datetime(value)
    except (TypeError, ValueError):
        try:
            parsed = datetime.fromisoformat(value)
        except ValueError:
            return None
    # "-0000" zones and naive ISO strings mean "no offset given"; callers
    # always get an aware datetime so comparisons never explode (UTC assumed).
    return parsed if parsed.tzinfo else parsed.replace(tzinfo=UTC)
