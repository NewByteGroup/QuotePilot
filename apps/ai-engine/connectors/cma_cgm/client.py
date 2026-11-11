"""Async client for the CMA CGM "API SpotOn" (instantquote v2).

Calls ``POST /spotOn/search`` to fetch bookable spot offers. The response is
untrusted input: it is schema-validated into pydantic models before reaching
the caller, and monetary amounts arrive as ``Decimal`` — never floats.

Auth is OAuth2 client-credentials (RFC 6749 §4.4): a bearer token is fetched
from ``auth.cma-cgm.com/as/token.oauth2`` with HTTP Basic credentials, cached
in memory, and refreshed when it approaches expiry. The clock is injectable
so tests can control expiry without sleeping.

The connector does no retries and no payload logging: retry/backoff policy
belongs to the River stage that calls ai-engine (ADR-004), and tenant content
must not reach logs (ADR-001).
"""

from __future__ import annotations

import math
import os
import time
from base64 import b64encode
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from types import TracebackType
from typing import Self

import httpx
from pydantic import ValidationError

from connectors.cma_cgm.errors import (
    CmaCgmApiError,
    CmaCgmAuthError,
    CmaCgmConfigError,
    CmaCgmSchemaError,
    CmaCgmTransportError,
)
from connectors.cma_cgm.models import (
    SPOT_SEARCH_ADAPTER,
    Fault,
    Message,
    SpotOffer,
    SpotQuotationRequest,
    SpotResultItem,
    TokenResponse,
)

PROD_BASE_URL = "https://apis.cma-cgm.net/pricing/commercial/instantquote/v2"
PROD_TOKEN_URL = "https://auth.cma-cgm.com/as/token.oauth2"
SEARCH_SCOPE = "instantquote:read:be"
DEFAULT_TIMEOUT_SECONDS = 30.0
# Refetch the token this many seconds before its nominal expiry so an
# in-flight request never carries a token that dies mid-flight.
TOKEN_EXPIRY_SKEW_SECONDS = 30.0

_ENV_CLIENT_ID = "CMA_CGM_CLIENT_ID"
_ENV_CLIENT_SECRET = "CMA_CGM_CLIENT_SECRET"
_ENV_BASE_URL = "CMA_CGM_BASE_URL"
_ENV_TOKEN_URL = "CMA_CGM_TOKEN_URL"
_ENV_TIMEOUT = "CMA_CGM_TIMEOUT_SECONDS"

_SUCCESS_STATUSES = (200, 206)


@dataclass(frozen=True)
class SpotSearchResult:
    """A parsed ``POST /spotOn/search`` reply.

    ``status`` is 200 (full page) or 206 (partial per the ``range`` header);
    ``partial`` is a convenience flag for the 206 case.
    """

    items: list[SpotResultItem]
    status: int

    @property
    def partial(self) -> bool:
        return self.status == 206

    @property
    def offers(self) -> list[SpotOffer]:
        """Only the offer items of the reply array."""
        return [item for item in self.items if isinstance(item, SpotOffer)]

    @property
    def messages(self) -> list[Message]:
        """Only the no-offer reason items of the reply array."""
        return [item for item in self.items if isinstance(item, Message)]


class CmaCgmClient:
    """Client for the SpotOn instant-quote search API.

    Args:
        client_id: OAuth client id from api-portal.cma-cgm.com.
        client_secret: OAuth client secret for the subscription.
        base_url: API root; defaults to production.
        token_url: OAuth token endpoint; defaults to production.
        timeout_seconds: total request timeout; retries are the caller's job.
        transport: httpx transport override — tests inject a fake here.
        http_client: shared ``httpx.AsyncClient``; when given, ``base_url``,
            ``timeout_seconds`` and ``transport`` are ignored and the client is
            not closed by ``aclose()``. Auth still goes per-request, so an
            injected client does not need headers configured.
        clock: monotonic clock for token expiry; tests inject a fake.

    Note:
        Concurrent ``search_offers`` calls on a cold client may issue
        duplicate token requests (benign: both tokens are valid, last write
        wins). Add an ``asyncio.Lock`` around ``_bearer_token`` if fan-out
        concurrency ever materializes in the calling stage.

    Example:
        >>> async with CmaCgmClient.from_env() as client:
        ...     result = await client.search_offers(spot_request)
        ...     for offer in result.offers:
        ...         ...  # offer.quote_line.* amounts are Decimal
    """

    def __init__(
        self,
        client_id: str,
        client_secret: str,
        *,
        base_url: str = PROD_BASE_URL,
        token_url: str = PROD_TOKEN_URL,
        timeout_seconds: float = DEFAULT_TIMEOUT_SECONDS,
        transport: httpx.AsyncBaseTransport | None = None,
        http_client: httpx.AsyncClient | None = None,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        if not client_id or not client_secret:
            raise CmaCgmConfigError(
                "client_id and client_secret must be non-empty; "
                "expected credentials from an api-portal.cma-cgm.com application"
            )
        self._basic_auth = b64encode(f"{client_id}:{client_secret}".encode()).decode(
            "ascii"
        )
        self._token_url = token_url
        self._clock = clock
        self._token: str | None = None
        self._token_expires_at = 0.0
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
        clock: Callable[[], float] = time.monotonic,
    ) -> Self:
        """Build a client from ``CMA_CGM_*`` environment variables.

        Required: ``CMA_CGM_CLIENT_ID``, ``CMA_CGM_CLIENT_SECRET``.
        Optional: ``CMA_CGM_BASE_URL``, ``CMA_CGM_TOKEN_URL`` (both default to
        production), ``CMA_CGM_TIMEOUT_SECONDS`` (default 30).

        Example:
            >>> client = CmaCgmClient.from_env()  # reads os.environ
        """
        source = os.environ if env is None else env
        missing = [
            name
            for name in (_ENV_CLIENT_ID, _ENV_CLIENT_SECRET)
            if not source.get(name)
        ]
        if missing:
            raise CmaCgmConfigError(
                f"missing required environment variables: {', '.join(missing)}; "
                "expected the OAuth client id/secret pair issued by an "
                "api-portal.cma-cgm.com application subscription"
            )
        return cls(
            client_id=source[_ENV_CLIENT_ID],
            client_secret=source[_ENV_CLIENT_SECRET],
            base_url=source.get(_ENV_BASE_URL, PROD_BASE_URL),
            token_url=source.get(_ENV_TOKEN_URL, PROD_TOKEN_URL),
            timeout_seconds=_timeout_from_env(source.get(_ENV_TIMEOUT)),
            transport=transport,
            http_client=http_client,
            clock=clock,
        )

    async def search_offers(
        self,
        request: SpotQuotationRequest,
        *,
        behalf_of: str | None = None,
        range_: str | None = None,
    ) -> SpotSearchResult:
        """Search SpotOn spot offers for a routing (``POST /spotOn/search``).

        Args:
            request: validated search request (ports/places, equipment,
                commodity, flags). Only ``requested_equipments`` is mandatory.
            behalf_of: end-customer code; mandatory for third-party partners.
            range_: pagination header (spec default ``0-4``, max 5 rows per
                page; a 206 reply means more rows exist).

        Returns:
            ``SpotSearchResult`` with discriminated ``SpotOffer``/``Message``
            items; ``result.partial`` is True on a 206 reply.

        Raises:
            CmaCgmApiError: 400/416/500/503 and other errors with a ``Fault``.
            CmaCgmAuthError: 401/403 on search, or a rejected token request.
            CmaCgmSchemaError: success body violating the published schema.
            CmaCgmTransportError: DNS/TLS/timeout/protocol failure.
        """
        raw = await self._post_search(request, behalf_of=behalf_of, range_=range_)
        if raw.status_code == 401:
            # A cached-but-rejected token must be dropped so a caller retry
            # re-authenticates instead of replaying the dead bearer.
            self._token = None
            self._token_expires_at = 0.0
        if raw.status_code not in _SUCCESS_STATUSES:
            raise _to_api_error(raw)
        return SpotSearchResult(items=_validated_items(raw), status=raw.status_code)

    async def _post_search(
        self,
        request: SpotQuotationRequest,
        *,
        behalf_of: str | None,
        range_: str | None,
    ) -> httpx.Response:
        headers = {
            "Authorization": f"Bearer {await self._bearer_token()}",
            "Accept": "application/json",
            "Content-Type": "application/json",
        }
        if range_ is not None:
            headers["range"] = range_
        params = {"behalfOf": behalf_of} if behalf_of else None
        try:
            return await self._client.post(
                "spotOn/search",
                content=request.model_dump_json(by_alias=True, exclude_none=True),
                headers=headers,
                params=params,
            )
        except httpx.HTTPError as exc:
            raise CmaCgmTransportError(
                f"POST {self._client.base_url}spotOn/search transport failure: {exc}"
            ) from exc

    async def _bearer_token(self) -> str:
        """Return a valid bearer token, fetching one when missing or expiring."""
        if self._token is not None and self._clock() < self._token_expires_at:
            return self._token
        token = await self._fetch_token()
        self._token = token.access_token
        self._token_expires_at = self._clock() + max(
            0.0, token.expires_in - TOKEN_EXPIRY_SKEW_SECONDS
        )
        return self._token

    async def _fetch_token(self) -> TokenResponse:
        """Exchange client credentials for an access token (RFC 6749 §4.4)."""
        try:
            raw = await self._client.post(
                self._token_url,
                headers={
                    "Authorization": f"Basic {self._basic_auth}",
                    "Content-Type": "application/x-www-form-urlencoded",
                    "Accept": "application/json",
                },
                data={
                    "grant_type": "client_credentials",
                    "scope": SEARCH_SCOPE,
                },
            )
        except httpx.HTTPError as exc:
            raise CmaCgmTransportError(
                f"POST {self._token_url} transport failure: {exc}"
            ) from exc
        if raw.status_code != 200:
            message = (
                f"token endpoint returned HTTP {raw.status_code} — "
                f"{raw.text[:200] or 'no details in response'}"
            )
            if raw.status_code in (400, 401, 403):
                raise CmaCgmAuthError(
                    f"{message}; check the OAuth client id/secret and "
                    "subscription scope",
                    status=raw.status_code,
                )
            raise CmaCgmApiError(message, status=raw.status_code)
        try:
            return TokenResponse.model_validate_json(raw.text)
        except ValidationError as exc:
            raise CmaCgmSchemaError(
                "token endpoint returned 200 but the body violates the "
                f"OAuth token schema: {exc}"
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
        raise CmaCgmConfigError(
            f"{_ENV_TIMEOUT} must be a number of seconds, got {raw!r}"
        ) from exc
    if not math.isfinite(timeout) or timeout <= 0:
        raise CmaCgmConfigError(
            f"{_ENV_TIMEOUT} must be a finite number > 0, got {raw!r}"
        )
    return timeout


def _validated_items(response: httpx.Response) -> list[SpotResultItem]:
    try:
        return SPOT_SEARCH_ADAPTER.validate_json(response.text)
    except ValidationError as exc:
        raise CmaCgmSchemaError(
            "POST spotOn/search returned success but the body violates the "
            f"SpotOffer/Message schema: {exc}"
        ) from exc


def _to_api_error(response: httpx.Response) -> CmaCgmApiError:
    try:
        fault = Fault.model_validate_json(response.text)
    except ValidationError:
        fault = None
    summary = (fault.description or fault.reason) if fault else ""
    message = (
        f"POST spotOn/search failed: HTTP {response.status_code} "
        f"— {summary or response.text[:200] or 'no details in response'}"
    )
    if response.status_code in (401, 403):
        return CmaCgmAuthError(message, status=response.status_code, fault=fault)
    return CmaCgmApiError(message, status=response.status_code, fault=fault)
