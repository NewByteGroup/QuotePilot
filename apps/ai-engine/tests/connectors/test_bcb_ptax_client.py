"""Client tests for the bcb_ptax connector: transport, parsing, error mapping."""

from __future__ import annotations

import asyncio
import hashlib
from datetime import UTC, date, datetime
from decimal import Decimal
from typing import Any

import httpx
import pytest

from connectors.bcb_ptax import (
    DEFAULT_TIMEOUT_SECONDS,
    PROD_BASE_URL,
    BcbPtaxClient,
    PtaxResult,
)
from connectors.bcb_ptax.errors import (
    BcbPtaxApiError,
    BcbPtaxConfigError,
    BcbPtaxSchemaError,
    BcbPtaxTransportError,
)
from tests.connectors.fakes import FakeBcbPtaxTransport
from tests.helpers import fixture_text

REQUESTED = date(2026, 11, 13)
WEEKEND = date(2026, 11, 14)


def _client(transport: FakeBcbPtaxTransport) -> BcbPtaxClient:
    return BcbPtaxClient(transport=transport)


def _fetch(client: BcbPtaxClient, day: date = REQUESTED) -> PtaxResult:
    return asyncio.run(client.get_dollar_rate(day))


def _body(value: str) -> str:
    return '{"@odata.context":"x","value":' + value + "}"


def _row(
    buy: str = "5.65120",
    sell: str = "5.65180",
    quoted_at: str = '"2026-11-13 13:03:27.375047"',
) -> str:
    return (
        '[{"cotacaoCompra":'
        + buy
        + ',"cotacaoVenda":'
        + sell
        + ',"dataHoraCotacao":'
        + quoted_at
        + "}]"
    )


def test_get_dollar_rate_sends_get_with_odata_params_and_no_auth() -> None:
    transport = FakeBcbPtaxTransport(
        body=fixture_text("bcb_ptax", "old", "daily_quote_200.json")
    )
    client = _client(transport)

    _fetch(client)

    assert len(transport.requests) == 1
    sent = transport.requests[0]
    assert sent.method == "GET"
    assert sent.url.path == (
        "/olinda/servico/PTAX/versao/v1/odata/CotacaoDolarDia(dataCotacao=@dataCotacao)"
    )
    assert sent.url.params.get("@dataCotacao") == "'11-13-2026'"
    assert sent.url.params.get("$format") == "json"
    assert "authorization" not in sent.headers


def test_get_dollar_rate_parses_public_fixture_as_exact_decimals() -> None:
    body = fixture_text("bcb_ptax", "old", "daily_quote_200.json")
    transport = FakeBcbPtaxTransport(body=body)
    client = _client(transport)

    result = _fetch(client)

    assert result.requested_date == REQUESTED
    assert result.rate is not None
    assert result.rate.buy_rate == Decimal("5.65120")
    assert result.rate.sell_rate == Decimal("5.65180")
    assert result.rate.buy_rate.as_tuple().exponent == -5
    assert result.rate.quoted_at == datetime(2026, 11, 13, 13, 3, 27, 375047)
    assert result.rate.quoted_at.tzinfo is None


def test_get_dollar_rate_returns_result_metadata() -> None:
    body = fixture_text("bcb_ptax", "old", "daily_quote_200.json")
    transport = FakeBcbPtaxTransport(body=body)
    client = _client(transport)

    result = _fetch(client)

    sent = transport.requests[0]
    assert result.source_url == str(sent.url)
    assert result.fetched_at.tzinfo is UTC
    assert result.raw_content == body.encode("utf-8")
    assert (
        result.source_content_hash == hashlib.sha256(body.encode("utf-8")).hexdigest()
    )


def test_get_dollar_rate_preserves_high_precision_lexeme() -> None:
    transport = FakeBcbPtaxTransport(
        body=_body(_row(buy="5.1234567890123456789", sell="5.1234567890123456789"))
    )
    client = _client(transport)

    result = _fetch(client)

    assert result.rate is not None
    assert result.rate.buy_rate == Decimal("5.1234567890123456789")


def test_get_dollar_rate_empty_value_returns_no_rate_single_request() -> None:
    transport = FakeBcbPtaxTransport(
        body=fixture_text("bcb_ptax", "old", "daily_quote_empty_200.json")
    )
    client = _client(transport)

    result = _fetch(client, WEEKEND)

    assert result.rate is None
    assert result.raw_content != b""
    assert len(transport.requests) == 1


@pytest.mark.parametrize(
    "body",
    [
        '{"@odata.context":"x"}',
        '{"value":null}',
        '{"value":{}}',
        '{"value":[{"cotacaoCompra":5.0,"dataHoraCotacao":"2026-11-13 10:00:00"}]}',
        "not-json{",
        "<html><body>Erro</body></html>",
        "[1,2]",
        '"hello"',
    ],
)
def test_get_dollar_rate_schema_error_on_malformed_body(body: str) -> None:
    transport = FakeBcbPtaxTransport(body=body)
    client = _client(transport)

    with pytest.raises(BcbPtaxSchemaError):
        _fetch(client)


def test_get_dollar_rate_schema_error_on_invalid_utf8() -> None:
    transport = FakeBcbPtaxTransport(body=b"\xff\xfe{")
    client = _client(transport)

    with pytest.raises(BcbPtaxSchemaError):
        _fetch(client)


def test_get_dollar_rate_schema_error_on_multiple_rows() -> None:
    transport = FakeBcbPtaxTransport(body=_body(_row()[:-1] + "," + _row()[1:]))
    client = _client(transport)

    with pytest.raises(BcbPtaxSchemaError):
        _fetch(client)


@pytest.mark.parametrize(
    "quoted_at",
    [
        '"2026-11-14 10:00:00"',
        '"2026-11-13"',
        "1731456000",
        '"2026-11-13 13:03:27.375047+00:00"',
        '"2026-11-13T13:03:27Z"',
        '"2026-13-99 10:00:00"',
    ],
)
def test_get_dollar_rate_schema_error_on_bad_quoted_at(quoted_at: str) -> None:
    transport = FakeBcbPtaxTransport(body=_body(_row(quoted_at=quoted_at)))
    client = _client(transport)

    with pytest.raises(BcbPtaxSchemaError):
        _fetch(client)


@pytest.mark.parametrize(
    "buy,sell",
    [
        ("0", "5.0"),
        ("-1.5", "5.0"),
        ("NaN", "5.0"),
        ("Infinity", "5.0"),
        ("true", "5.0"),
        ('"5.65"', "5.0"),
        ("5.90", "5.80"),
    ],
)
def test_get_dollar_rate_schema_error_on_bad_amounts(buy: str, sell: str) -> None:
    transport = FakeBcbPtaxTransport(body=_body(_row(buy=buy, sell=sell)))
    client = _client(transport)

    with pytest.raises(BcbPtaxSchemaError):
        _fetch(client)


def test_get_dollar_rate_accepts_harmless_unknown_fields() -> None:
    transport = FakeBcbPtaxTransport(
        body=_body(
            '[{"cotacaoCompra":5.65120,"cotacaoVenda":5.65180,'
            '"dataHoraCotacao":"2026-11-13 13:03:27.375047",'
            '"extraField":"ignored"}]'
        )
    )
    client = _client(transport)

    result = _fetch(client)

    assert result.rate is not None


@pytest.mark.parametrize("status", [204, 302, 401, 403, 429, 500])
def test_get_dollar_rate_maps_non_200_to_api_error(status: int) -> None:
    transport = FakeBcbPtaxTransport(status=status, body="{}")
    client = _client(transport)

    with pytest.raises(BcbPtaxApiError) as excinfo:
        _fetch(client)
    assert excinfo.value.status == status


@pytest.mark.parametrize(
    "error",
    [
        httpx.ConnectError("dns failed"),
        httpx.ReadTimeout("slow"),
    ],
)
def test_get_dollar_rate_maps_transport_failures_without_retry(
    error: httpx.HTTPError,
) -> None:
    transport = FakeBcbPtaxTransport(error=error)
    client = _client(transport)

    with pytest.raises(BcbPtaxTransportError):
        _fetch(client)
    assert len(transport.requests) == 1


@pytest.mark.parametrize(
    "bad_date",
    [
        "2026-11-13",
        datetime(2026, 11, 13, 10, 0, 0),
        1731456000,
        None,
    ],
)
def test_get_dollar_rate_rejects_non_date_before_network(bad_date: Any) -> None:
    transport = FakeBcbPtaxTransport()
    client = _client(transport)

    with pytest.raises(BcbPtaxConfigError):
        asyncio.run(client.get_dollar_rate(bad_date))
    assert transport.requests == []


def test_injected_http_client_is_not_closed() -> None:
    transport = FakeBcbPtaxTransport(
        body=fixture_text("bcb_ptax", "old", "daily_quote_200.json")
    )
    http_client = httpx.AsyncClient(base_url=PROD_BASE_URL, transport=transport)
    client = BcbPtaxClient(http_client=http_client)

    _fetch(client)
    asyncio.run(client.aclose())

    assert not http_client.is_closed
    asyncio.run(http_client.aclose())


def test_own_client_is_closed_by_aclose_and_context_manager() -> None:
    transport = FakeBcbPtaxTransport(
        body=fixture_text("bcb_ptax", "old", "daily_quote_200.json")
    )
    client = _client(transport)

    _fetch(client)
    asyncio.run(client.aclose())
    assert client._client.is_closed

    async def _with() -> BcbPtaxClient:
        async with BcbPtaxClient(transport=transport) as inner:
            return inner

    inner = asyncio.run(_with())
    assert inner._client.is_closed


@pytest.mark.parametrize(
    "timeout",
    [True, 0, -1, float("nan"), float("inf"), "30"],
)
def test_invalid_timeout_raises_config_error(timeout: Any) -> None:
    with pytest.raises(BcbPtaxConfigError):
        BcbPtaxClient(timeout_seconds=timeout)


def test_default_timeout_is_30_seconds() -> None:
    assert DEFAULT_TIMEOUT_SECONDS == 30.0


BRL_START = date(1994, 7, 1)
CANARY = "canary-9f3b-secret"


@pytest.mark.parametrize("bad_date", [date(1994, 6, 30), date(1, 1, 1)])
def test_get_dollar_rate_rejects_pre_real_dates_before_network(
    bad_date: date,
) -> None:
    transport = FakeBcbPtaxTransport()
    client = _client(transport)

    with pytest.raises(BcbPtaxConfigError) as excinfo:
        asyncio.run(client.get_dollar_rate(bad_date))

    assert bad_date.isoformat() in str(excinfo.value)
    assert BRL_START.isoformat() in str(excinfo.value)
    assert transport.requests == []


def test_get_dollar_rate_accepts_real_start_boundary() -> None:
    transport = FakeBcbPtaxTransport(
        body=fixture_text("bcb_ptax", "old", "daily_quote_empty_200.json")
    )
    client = _client(transport)

    result = asyncio.run(client.get_dollar_rate(BRL_START))

    assert result.rate is None
    assert len(transport.requests) == 1
    sent = transport.requests[0]
    assert sent.url.params.get("@dataCotacao") == "'07-01-1994'"


def test_api_error_message_excludes_raw_body() -> None:
    transport = FakeBcbPtaxTransport(status=500, body=CANARY)
    client = _client(transport)

    with pytest.raises(BcbPtaxApiError) as excinfo:
        _fetch(client)
    assert CANARY not in str(excinfo.value)


@pytest.mark.parametrize(
    "body",
    [
        CANARY + "{",
        _body(_row(buy='"' + CANARY + '"')),
    ],
)
def test_schema_error_message_excludes_raw_body(body: str) -> None:
    transport = FakeBcbPtaxTransport(body=body)
    client = _client(transport)

    with pytest.raises(BcbPtaxSchemaError) as excinfo:
        _fetch(client)
    assert CANARY not in str(excinfo.value)
