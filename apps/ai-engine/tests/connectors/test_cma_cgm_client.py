"""Client tests for the cma_cgm connector: OAuth flow, transport, error mapping."""

from __future__ import annotations

import asyncio
import json
from base64 import b64encode
from typing import Any
from urllib.parse import parse_qs

import httpx
import pytest

from connectors.cma_cgm import (
    PROD_BASE_URL,
    PROD_TOKEN_URL,
    CmaCgmClient,
    SpotQuotationRequest,
)
from connectors.cma_cgm.errors import (
    CmaCgmApiError,
    CmaCgmAuthError,
    CmaCgmConfigError,
    CmaCgmSchemaError,
    CmaCgmTransportError,
)
from tests.helpers import FakeCmaCgmTransport, fixture_json, fixture_text


def _request() -> SpotQuotationRequest:
    return SpotQuotationRequest.model_validate_json(
        fixture_text("cma_cgm", "old", "spoton_search_request.json")
    )


def _client(transport: FakeCmaCgmTransport, **kwargs: Any) -> CmaCgmClient:
    return CmaCgmClient("test-id", "test-secret", transport=transport, **kwargs)


def _good_transport(**kwargs: Any) -> FakeCmaCgmTransport:
    kwargs.setdefault(
        "token_body", fixture_text("cma_cgm", "old", "token_response.json")
    )
    kwargs.setdefault(
        "search_body",
        fixture_text("cma_cgm", "old", "spoton_search_response_200.json"),
    )
    return FakeCmaCgmTransport(**kwargs)


def test_search_offers_fetches_token_then_posts_search() -> None:
    transport = _good_transport()
    client = _client(transport)

    asyncio.run(client.search_offers(_request()))

    assert len(transport.token_requests) == 1
    token_req = transport.token_requests[0]
    assert token_req.method == "POST"
    assert str(token_req.url) == PROD_TOKEN_URL
    expected_basic = b64encode(b"test-id:test-secret").decode("ascii")
    assert token_req.headers["authorization"] == f"Basic {expected_basic}"
    form = parse_qs(token_req.content.decode())
    assert form["grant_type"] == ["client_credentials"]
    assert form["scope"] == ["instantquote:read:be"]

    assert len(transport.search_requests) == 1
    search_req = transport.search_requests[0]
    assert str(search_req.url) == f"{PROD_BASE_URL}/spotOn/search"
    assert search_req.headers["authorization"] == (
        "Bearer eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9"
        ".synthetic-token-for-tests.signature"
    )
    assert json.loads(search_req.content) == fixture_json(
        "cma_cgm", "old", "spoton_search_request.json"
    )


def test_search_offers_returns_offers_and_messages() -> None:
    transport = _good_transport()
    client = _client(transport)

    result = asyncio.run(client.search_offers(_request()))

    assert result.status == 200
    assert result.partial is False
    assert len(result.items) == 3
    assert len(result.offers) == 2
    assert len(result.messages) == 1
    assert result.messages[0].code == "INQ_TRN"
    assert result.offers[0].quote_line is not None


def test_search_offers_accepts_partial_206() -> None:
    transport = _good_transport(search_status=206)
    client = _client(transport)

    result = asyncio.run(client.search_offers(_request()))

    assert result.status == 206
    assert result.partial is True


def test_search_offers_sends_behalf_of_and_range() -> None:
    transport = _good_transport()
    client = _client(transport)

    asyncio.run(client.search_offers(_request(), behalf_of="0000999888", range_="5-9"))

    sent = transport.search_requests[0]
    assert sent.url.params["behalfOf"] == "0000999888"
    assert sent.headers["range"] == "5-9"


def test_bearer_token_is_cached_across_searches() -> None:
    transport = _good_transport()
    client = _client(transport)

    async def _run() -> None:
        await client.search_offers(_request())
        await client.search_offers(_request())

    asyncio.run(_run())

    assert len(transport.token_requests) == 1
    assert len(transport.search_requests) == 2


def test_bearer_token_refreshes_after_expiry() -> None:
    transport = _good_transport()
    now = [0.0]
    client = _client(transport, clock=lambda: now[0])

    async def _run() -> None:
        await client.search_offers(_request())
        now[0] += 3600 - 30 + 1  # past the skew-adjusted expiry
        await client.search_offers(_request())

    asyncio.run(_run())

    assert len(transport.token_requests) == 2
    assert len(transport.search_requests) == 2


def test_bearer_token_not_refreshed_before_skew_expiry() -> None:
    transport = _good_transport()
    now = [0.0]
    client = _client(transport, clock=lambda: now[0])

    async def _run() -> None:
        await client.search_offers(_request())
        now[0] += 3600 - 30 - 1  # still inside the skew-adjusted expiry
        await client.search_offers(_request())

    asyncio.run(_run())

    assert len(transport.token_requests) == 1


def test_search_offers_maps_fault_responses() -> None:
    transport = _good_transport(
        search_status=400,
        search_body=fixture_text("cma_cgm", "old", "fault_400.json"),
    )
    client = _client(transport)

    with pytest.raises(CmaCgmApiError) as excinfo:
        asyncio.run(client.search_offers(_request()))

    assert excinfo.value.status == 400
    assert excinfo.value.fault is not None
    assert excinfo.value.fault.reason == "Bad Request"


def test_search_offers_maps_503_fault() -> None:
    transport = _good_transport(
        search_status=503,
        search_body=fixture_text("cma_cgm", "old", "fault_503.json"),
    )
    client = _client(transport)

    with pytest.raises(CmaCgmApiError) as excinfo:
        asyncio.run(client.search_offers(_request()))

    assert excinfo.value.status == 503
    assert excinfo.value.fault is not None
    assert excinfo.value.fault.reason == "Service Unavailable"


def test_search_offers_403_raises_auth_error() -> None:
    transport = _good_transport(search_status=403, search_body="")
    client = _client(transport)

    with pytest.raises(CmaCgmAuthError) as excinfo:
        asyncio.run(client.search_offers(_request()))

    assert excinfo.value.status == 403
    assert excinfo.value.fault is None


def test_401_search_invalidates_cached_token() -> None:
    transport = _good_transport(search_status=401, search_body="")
    client = _client(transport)

    async def _run() -> None:
        with pytest.raises(CmaCgmAuthError):
            await client.search_offers(_request())
        # The dead bearer was dropped: the next call re-authenticates.
        transport.search_status = 200
        transport.search_body = fixture_text(
            "cma_cgm", "old", "spoton_search_response_200.json"
        )
        await client.search_offers(_request())

    asyncio.run(_run())

    assert len(transport.token_requests) == 2
    assert len(transport.search_requests) == 2


def test_token_endpoint_401_raises_auth_error() -> None:
    transport = _good_transport(
        token_status=401,
        token_body=fixture_text("cma_cgm", "old", "token_error_401.json"),
    )
    client = _client(transport)

    with pytest.raises(CmaCgmAuthError) as excinfo:
        asyncio.run(client.search_offers(_request()))

    assert excinfo.value.status == 401
    assert len(transport.search_requests) == 0


def test_token_endpoint_500_raises_api_error_not_auth() -> None:
    transport = _good_transport(token_status=500, token_body="server error")
    client = _client(transport)

    with pytest.raises(CmaCgmApiError) as excinfo:
        asyncio.run(client.search_offers(_request()))

    assert not isinstance(excinfo.value, CmaCgmAuthError)
    assert excinfo.value.status == 500


def test_token_endpoint_malformed_body_raises_schema_error() -> None:
    transport = _good_transport(token_body='{"unexpected": true}')
    client = _client(transport)

    with pytest.raises(CmaCgmSchemaError):
        asyncio.run(client.search_offers(_request()))


def test_token_endpoint_transport_error() -> None:
    transport = _good_transport(token_error=httpx.ConnectError("connection refused"))
    client = _client(transport)

    with pytest.raises(CmaCgmTransportError):
        asyncio.run(client.search_offers(_request()))


def test_search_transport_error() -> None:
    transport = _good_transport(search_error=httpx.ConnectError("connection refused"))
    client = _client(transport)

    with pytest.raises(CmaCgmTransportError):
        asyncio.run(client.search_offers(_request()))


def test_search_offers_raises_schema_error_on_bad_success_body() -> None:
    transport = _good_transport(
        search_body=fixture_text(
            "cma_cgm", "old", "spoton_search_response_malformed.json"
        )
    )
    client = _client(transport)

    with pytest.raises(CmaCgmSchemaError):
        asyncio.run(client.search_offers(_request()))


def test_search_offers_uses_configured_base_and_token_url() -> None:
    transport = _good_transport()
    client = _client(
        transport,
        base_url="https://example.invalid/v2",
        token_url="https://auth.example.invalid/token.oauth2",
    )

    asyncio.run(client.search_offers(_request()))

    assert str(transport.token_requests[0].url) == (
        "https://auth.example.invalid/token.oauth2"
    )
    assert str(transport.search_requests[0].url) == (
        "https://example.invalid/v2/spotOn/search"
    )


def test_init_rejects_empty_credentials() -> None:
    transport = _good_transport()

    with pytest.raises(CmaCgmConfigError):
        CmaCgmClient("", "secret", transport=transport)
    with pytest.raises(CmaCgmConfigError):
        CmaCgmClient("id", "", transport=transport)


def test_from_env_requires_credentials(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("CMA_CGM_CLIENT_ID", raising=False)
    monkeypatch.delenv("CMA_CGM_CLIENT_SECRET", raising=False)

    with pytest.raises(CmaCgmConfigError) as excinfo:
        CmaCgmClient.from_env()

    assert "CMA_CGM_CLIENT_ID" in str(excinfo.value)
    assert "CMA_CGM_CLIENT_SECRET" in str(excinfo.value)


def test_from_env_builds_client(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("CMA_CGM_CLIENT_ID", "env-id")
    monkeypatch.setenv("CMA_CGM_CLIENT_SECRET", "env-secret")
    monkeypatch.setenv("CMA_CGM_TIMEOUT_SECONDS", "5")

    client = CmaCgmClient.from_env()

    assert isinstance(client, CmaCgmClient)
    asyncio.run(client.aclose())


def test_from_env_uses_default_timeout(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("CMA_CGM_CLIENT_ID", "env-id")
    monkeypatch.setenv("CMA_CGM_CLIENT_SECRET", "env-secret")
    monkeypatch.delenv("CMA_CGM_TIMEOUT_SECONDS", raising=False)

    client = CmaCgmClient.from_env()

    assert isinstance(client, CmaCgmClient)
    asyncio.run(client.aclose())


def test_from_env_blank_optional_urls_fall_back_to_production() -> None:
    # .env.example exports CMA_CGM_BASE_URL= / CMA_CGM_TOKEN_URL= empty;
    # blank must behave like unset, not override the production defaults.
    transport = _good_transport()
    client = CmaCgmClient.from_env(
        env={
            "CMA_CGM_CLIENT_ID": "env-id",
            "CMA_CGM_CLIENT_SECRET": "env-secret",
            "CMA_CGM_BASE_URL": "",
            "CMA_CGM_TOKEN_URL": "",
        },
        transport=transport,
    )

    asyncio.run(client.search_offers(_request()))

    assert str(transport.token_requests[0].url) == PROD_TOKEN_URL
    assert str(transport.search_requests[0].url) == f"{PROD_BASE_URL}/spotOn/search"
    asyncio.run(client.aclose())


@pytest.mark.parametrize("bad_timeout", ["abc", "-1", "0", "nan", "inf"])
def test_from_env_rejects_bad_timeout(
    monkeypatch: pytest.MonkeyPatch, bad_timeout: str
) -> None:
    monkeypatch.setenv("CMA_CGM_CLIENT_ID", "env-id")
    monkeypatch.setenv("CMA_CGM_CLIENT_SECRET", "env-secret")
    monkeypatch.setenv("CMA_CGM_TIMEOUT_SECONDS", bad_timeout)

    with pytest.raises(CmaCgmConfigError) as excinfo:
        CmaCgmClient.from_env()

    assert "CMA_CGM_TIMEOUT_SECONDS" in str(excinfo.value)


def test_injected_http_client_still_sends_auth_and_is_not_closed() -> None:
    async def _run() -> httpx.AsyncClient:
        transport = _good_transport()
        shared = httpx.AsyncClient(base_url=PROD_BASE_URL, transport=transport)
        client = CmaCgmClient("id", "secret", http_client=shared)

        await client.search_offers(_request())
        await client.aclose()

        assert (
            transport.search_requests[0].headers["authorization"].startswith("Bearer ")
        )
        return shared

    shared = asyncio.run(_run())
    assert shared.is_closed is False
    asyncio.run(shared.aclose())


def test_client_context_manager_closes() -> None:
    async def _run() -> None:
        transport = _good_transport()
        async with CmaCgmClient("id", "secret", transport=transport) as client:
            await client.search_offers(_request())
        assert len(transport.search_requests) == 1

    asyncio.run(_run())
