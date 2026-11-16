"""Client tests for the hapag_lloyd connector: transport, auth, error mapping."""

from __future__ import annotations

import asyncio
import json
from typing import Any

import httpx
import pytest

from connectors.hapag_lloyd import (
    PROD_BASE_URL,
    HapagLloydClient,
    OfferRequest,
    ProductIdentifier,
)
from connectors.hapag_lloyd.errors import (
    HapagApiError,
    HapagAuthError,
    HapagConfigError,
    HapagSchemaError,
    HapagTransportError,
)
from tests.helpers import FakeHapagTransport, fixture_json, fixture_text


def _request() -> OfferRequest:
    return OfferRequest.model_validate_json(
        fixture_text("hapag_lloyd", "old", "prices_request.json")
    )


def _client(transport: FakeHapagTransport, **kwargs: Any) -> HapagLloydClient:
    return HapagLloydClient("test-id", "test-secret", transport=transport, **kwargs)


def test_get_offers_posts_to_prices_with_auth_headers() -> None:
    transport = FakeHapagTransport(
        body=fixture_text("hapag_lloyd", "old", "prices_response_201.json")
    )
    client = _client(transport)

    asyncio.run(client.get_offers(_request()))

    assert len(transport.requests) == 1
    sent = transport.requests[0]
    assert sent.method == "POST"
    assert str(sent.url) == f"{PROD_BASE_URL}/prices"
    assert sent.headers["x-ibm-client-id"] == "test-id"
    assert sent.headers["x-ibm-client-secret"] == "test-secret"
    assert json.loads(sent.content) == fixture_json(
        "hapag_lloyd", "old", "prices_request.json"
    )


def test_get_offers_returns_parsed_offers_and_headers() -> None:
    transport = FakeHapagTransport(
        body=fixture_text("hapag_lloyd", "old", "prices_response_201.json"),
        headers={
            "Content-Type": "application/json",
            "Location": "https://api.hlag.com/hlag/external/v2/quotation-booking-engine/external/prices/24595eb0-5cfc-4381-9c3a-cedc1975e9aa",
            "Expires": "Wed, 01 Jul 2026 00:00:00 GMT",
        },
    )
    client = _client(transport)

    result = asyncio.run(client.get_offers(_request()))

    assert result.response.offers is not None
    assert len(result.response.offers) == 2
    assert result.response.offers[0].product_identifier is (
        ProductIdentifier.QUICK_QUOTES_SPOT
    )
    assert result.location is not None
    assert result.location.endswith("/prices/24595eb0-5cfc-4381-9c3a-cedc1975e9aa")
    assert result.expires is not None
    assert result.expires.year == 2026


def test_get_offers_accepts_mock_server_200() -> None:
    # The official mock answers 200 (prod answers 201); both are success.
    transport = FakeHapagTransport(
        status=200,
        body=fixture_text("hapag_lloyd", "old", "prices_response_201.json"),
    )
    client = _client(transport)

    result = asyncio.run(client.get_offers(_request()))

    assert result.response.offers is not None


def test_get_offers_uses_configured_base_url() -> None:
    transport = FakeHapagTransport(body="{}")
    client = _client(
        transport,
        base_url="https://mock.api-portal.hlag.com/v2/quotation-booking-engine/external",
    )

    asyncio.run(client.get_offers(_request()))

    assert str(transport.requests[0].url) == (
        "https://mock.api-portal.hlag.com/v2/quotation-booking-engine/external/prices"
    )


@pytest.mark.parametrize(
    ("fixture_name", "status", "expected_error"),
    [
        ("problem_400.json", 400, HapagApiError),
        ("problem_401.json", 401, HapagAuthError),
        ("problem_500.json", 500, HapagApiError),
    ],
)
def test_get_offers_maps_problem_responses(
    fixture_name: str, status: int, expected_error: type[Exception]
) -> None:
    transport = FakeHapagTransport(
        status=status,
        body=fixture_text("hapag_lloyd", "old", fixture_name),
        headers={"Content-Type": "application/problem+json"},
    )
    client = _client(transport)

    with pytest.raises(expected_error) as excinfo:
        asyncio.run(client.get_offers(_request()))

    assert excinfo.value.status == status  # type: ignore[attr-defined]
    assert excinfo.value.problem is not None  # type: ignore[attr-defined]
    assert excinfo.value.problem.status == status  # type: ignore[attr-defined]


def test_get_offers_raises_schema_error_on_bad_success_body() -> None:
    transport = FakeHapagTransport(
        body=fixture_text("hapag_lloyd", "old", "prices_response_malformed.json")
    )
    client = _client(transport)

    with pytest.raises(HapagSchemaError):
        asyncio.run(client.get_offers(_request()))


def test_get_offers_raises_transport_error_on_network_failure() -> None:
    transport = FakeHapagTransport(error=httpx.ConnectError("connection refused"))
    client = _client(transport)

    with pytest.raises(HapagTransportError):
        asyncio.run(client.get_offers(_request()))


def test_get_offers_api_error_without_problem_body() -> None:
    transport = FakeHapagTransport(
        status=409,
        body="upstream conflict",
        headers={"Content-Type": "text/plain"},
    )
    client = _client(transport)

    with pytest.raises(HapagApiError) as excinfo:
        asyncio.run(client.get_offers(_request()))

    assert excinfo.value.status == 409
    assert excinfo.value.problem is None


def test_get_offers_tolerates_unparseable_expires_header() -> None:
    transport = FakeHapagTransport(
        body=fixture_text("hapag_lloyd", "old", "prices_response_201.json"),
        headers={"Content-Type": "application/json", "Expires": "not-a-date"},
    )
    client = _client(transport)

    result = asyncio.run(client.get_offers(_request()))

    assert result.expires is None


def test_from_env_requires_credentials(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.delenv("HAPAG_CLIENT_ID", raising=False)
    monkeypatch.delenv("HAPAG_CLIENT_SECRET", raising=False)

    with pytest.raises(HapagConfigError) as excinfo:
        HapagLloydClient.from_env()

    assert "HAPAG_CLIENT_ID" in str(excinfo.value)
    assert "HAPAG_CLIENT_SECRET" in str(excinfo.value)


def test_from_env_builds_client(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("HAPAG_CLIENT_ID", "env-id")
    monkeypatch.setenv("HAPAG_CLIENT_SECRET", "env-secret")
    monkeypatch.setenv("HAPAG_TIMEOUT_SECONDS", "5")

    client = HapagLloydClient.from_env()

    assert isinstance(client, HapagLloydClient)
    asyncio.run(client.aclose())


def test_from_env_blank_base_url_falls_back_to_production() -> None:
    # .env.example exports HAPAG_BASE_URL= empty; blank must behave like
    # unset, not override the production default.
    transport = FakeHapagTransport(body="{}")
    client = HapagLloydClient.from_env(
        env={
            "HAPAG_CLIENT_ID": "env-id",
            "HAPAG_CLIENT_SECRET": "env-secret",
            "HAPAG_BASE_URL": "",
        },
        transport=transport,
    )

    asyncio.run(client.get_offers(_request()))

    assert str(transport.requests[0].url) == f"{PROD_BASE_URL}/prices"
    asyncio.run(client.aclose())


@pytest.mark.parametrize("bad_timeout", ["abc", "-1", "0", "nan", "inf"])
def test_from_env_rejects_bad_timeout(
    monkeypatch: pytest.MonkeyPatch, bad_timeout: str
) -> None:
    monkeypatch.setenv("HAPAG_CLIENT_ID", "env-id")
    monkeypatch.setenv("HAPAG_CLIENT_SECRET", "env-secret")
    monkeypatch.setenv("HAPAG_TIMEOUT_SECONDS", bad_timeout)

    with pytest.raises(HapagConfigError) as excinfo:
        HapagLloydClient.from_env()

    assert "HAPAG_TIMEOUT_SECONDS" in str(excinfo.value)


def test_injected_http_client_still_sends_auth_and_is_not_closed() -> None:
    async def _run() -> httpx.AsyncClient:
        transport = FakeHapagTransport(
            body=fixture_text("hapag_lloyd", "old", "prices_response_201.json")
        )
        shared = httpx.AsyncClient(base_url=PROD_BASE_URL, transport=transport)
        client = HapagLloydClient("id", "secret", http_client=shared)

        await client.get_offers(_request())
        await client.aclose()

        assert transport.requests[0].headers["x-ibm-client-id"] == "id"
        assert transport.requests[0].headers["x-ibm-client-secret"] == "secret"
        return shared

    shared = asyncio.run(_run())
    assert shared.is_closed is False
    asyncio.run(shared.aclose())


def test_client_context_manager_closes() -> None:
    async def _run() -> None:
        transport = FakeHapagTransport(body="{}")
        async with HapagLloydClient("id", "secret", transport=transport) as client:
            await client.get_offers(_request())
        assert len(transport.requests) == 1

    asyncio.run(_run())
