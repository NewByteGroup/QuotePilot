"""Async client for the Banco Central do Brasil PTAX OData API.

Calls ``GET CotacaoDolarDia(dataCotacao=@dataCotacao)`` on the public Olinda
open-data service (no auth, no key). The response is untrusted input: it is
schema-validated into pydantic models before reaching the caller, and money
arrives as ``Decimal`` parsed straight from JSON lexemes — never floats.

The endpoint answers zero or one row per business date: an empty ``value``
means no bulletin for that day (weekends/holidays), so ``rate`` is ``None``
rather than an error or a fallback. More than one row is a schema violation.

The connector does no retries and no payload logging: retry/backoff policy
belongs to the River stage that calls ai-engine (ADR-004), and tenant content
must not reach logs (ADR-001).
"""

from __future__ import annotations

import hashlib
import json
import math
from dataclasses import dataclass
from datetime import UTC, date, datetime
from decimal import Decimal
from types import TracebackType
from typing import Self

import httpx
from pydantic import ValidationError

from connectors.bcb_ptax.errors import (
    BcbPtaxApiError,
    BcbPtaxConfigError,
    BcbPtaxSchemaError,
    BcbPtaxTransportError,
)
from connectors.bcb_ptax.models import PtaxRate, PtaxResponse

PROD_BASE_URL = "https://olinda.bcb.gov.br/olinda/servico/PTAX/versao/v1/odata/"
DEFAULT_TIMEOUT_SECONDS = 30.0

_DAILY_QUOTE_PATH = "CotacaoDolarDia(dataCotacao=@dataCotacao)"
_BRL_START_DATE = date(1994, 7, 1)


@dataclass(frozen=True)
class PtaxResult:
    """Outcome of one ``get_dollar_rate`` call.

    ``rate`` is ``None`` when PTAX has no bulletin for ``requested_date``;
    ``raw_content`` and ``source_content_hash`` keep the verbatim response so
    the caller can audit exactly what the API returned.
    """

    requested_date: date
    rate: PtaxRate | None
    source_url: str
    fetched_at: datetime
    raw_content: bytes
    source_content_hash: str


class BcbPtaxClient:
    """Client for the public BCB PTAX OData daily dollar quote.

    Args:
        base_url: API root; defaults to production Olinda endpoint.
        timeout_seconds: total request timeout; retries are the caller's job.
        transport: httpx transport override — tests inject a fake here.
        http_client: shared ``httpx.AsyncClient``; when given, ``base_url``,
            ``timeout_seconds`` and ``transport`` are ignored and the client
            is not closed by ``aclose()``.

    Example:
        >>> async with BcbPtaxClient() as client:
        ...     result = await client.get_dollar_rate(date(2026, 11, 13))
        ...     result.rate.buy_rate  # Decimal
    """

    def __init__(
        self,
        *,
        base_url: str = PROD_BASE_URL,
        timeout_seconds: float = DEFAULT_TIMEOUT_SECONDS,
        transport: httpx.AsyncBaseTransport | None = None,
        http_client: httpx.AsyncClient | None = None,
    ) -> None:
        if http_client is not None:
            self._client = http_client
            self._owns_client = False
        else:
            _validate_timeout(timeout_seconds)
            self._client = httpx.AsyncClient(
                base_url=base_url,
                timeout=timeout_seconds,
                transport=transport,
            )
            self._owns_client = True

    async def get_dollar_rate(self, requested_date: date) -> PtaxResult:
        """Fetch the PTAX USD-BRL bulletin for ``requested_date``.

        Args:
            requested_date: a plain ``datetime.date`` (not ``datetime`` or
                ``str``); PTAX quotes are keyed by business date. Must be
                on or after 1994-07-01, when the Brazilian Real was
                introduced — earlier dates cannot have a USD-BRL quote.

        Returns:
            ``PtaxResult`` with ``rate`` set when a bulletin exists for the
            date, ``None`` on weekends/holidays. Always carries the raw
            response bytes, their SHA-256 and the effective request URL.

        Raises:
            BcbPtaxConfigError: ``requested_date`` is not a ``date``, or
                precedes 1994-07-01 (the start of the Real).
            BcbPtaxApiError: any non-200 HTTP status.
            BcbPtaxSchemaError: 200 body violating the PTAX shape or quoting
                a different date.
            BcbPtaxTransportError: DNS/TLS/timeout/protocol failure.
        """
        if type(requested_date) is not date:
            raise BcbPtaxConfigError(
                "requested_date must be a datetime.date instance, got "
                f"{type(requested_date).__name__}"
            )
        if requested_date < _BRL_START_DATE:
            raise BcbPtaxConfigError(
                f"requested_date {requested_date.isoformat()} precedes "
                f"{_BRL_START_DATE.isoformat()}, the start of the Brazilian "
                "Real; PTAX has no USD-BRL quote before that date"
            )
        raw = await self._get_daily_quote(requested_date)
        if raw.status_code != 200:
            raise BcbPtaxApiError(
                "GET CotacaoDolarDia returned HTTP "
                f"{raw.status_code} for {requested_date.isoformat()}",
                status=raw.status_code,
            )
        content = raw.content
        return PtaxResult(
            requested_date=requested_date,
            rate=_parse_rate(content, requested_date),
            source_url=str(raw.request.url),
            fetched_at=datetime.now(UTC),
            raw_content=content,
            source_content_hash=hashlib.sha256(content).hexdigest(),
        )

    async def _get_daily_quote(self, requested_date: date) -> httpx.Response:
        params = {
            "@dataCotacao": f"'{requested_date:%m-%d-%Y}'",
            "$format": "json",
        }
        try:
            return await self._client.get(_DAILY_QUOTE_PATH, params=params)
        except httpx.HTTPError as exc:
            raise BcbPtaxTransportError(
                f"GET {self._client.base_url}{_DAILY_QUOTE_PATH} "
                f"transport failure: {exc}"
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


def _validate_timeout(timeout_seconds: object) -> None:
    if (
        isinstance(timeout_seconds, bool)
        or not isinstance(timeout_seconds, (int, float))
        or not math.isfinite(timeout_seconds)
        or timeout_seconds <= 0
    ):
        raise BcbPtaxConfigError(
            f"timeout_seconds must be a finite number > 0, got {timeout_seconds!r}"
        )


def _parse_rate(content: bytes, requested_date: date) -> PtaxRate | None:
    try:
        payload = json.loads(content, parse_float=Decimal, parse_int=Decimal)
    except ValueError as exc:
        raise BcbPtaxSchemaError(
            "PTAX response is not valid JSON for "
            f"{requested_date.isoformat()} ({type(exc).__name__})"
        ) from exc
    if not isinstance(payload, dict):
        raise BcbPtaxSchemaError(
            "PTAX response must be a JSON object with a 'value' list for "
            f"{requested_date.isoformat()}, got {type(payload).__name__}"
        )
    try:
        response = PtaxResponse.model_validate(payload)
    except ValidationError as exc:
        raise BcbPtaxSchemaError(
            "PTAX body failed schema validation "
            f"({exc.error_count()} error(s)) for {requested_date.isoformat()}"
        ) from exc
    if not response.value:
        return None
    rate = response.value[0]
    if rate.quoted_at.date() != requested_date:
        raise BcbPtaxSchemaError(
            f"dataHoraCotacao date {rate.quoted_at.date().isoformat()} does "
            f"not match requested {requested_date.isoformat()}"
        )
    return rate
