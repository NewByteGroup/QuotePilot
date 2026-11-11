"""Client tests for the firecrawl connector: transport, auth, error mapping."""

from __future__ import annotations

import asyncio
import json
from typing import Any

import httpx
import pytest

from connectors.firecrawl import PROD_BASE_URL, FirecrawlClient
from connectors.firecrawl.errors import (
    FirecrawlApiError,
    FirecrawlAuthError,
    FirecrawlConfigError,
    FirecrawlSchemaError,
    FirecrawlTransportError,
)
from tests.helpers import FakeFirecrawlTransport, fixture_text

PAGE_URL = "https://tariffs.example.com/santos-hamburg"


def _client(transport: FakeFirecrawlTransport, **kwargs: Any) -> FirecrawlClient:
    return FirecrawlClient("test-key", transport=transport, **kwargs)


def test_scrape_markdown_posts_to_scrape_with_auth_and_body() -> None:
    transport = FakeFirecrawlTransport(
        body=fixture_text("firecrawl", "old", "scrape_response_ok.json")
    )
    client = _client(transport)

    asyncio.run(client.scrape_markdown(PAGE_URL))

    assert len(transport.requests) == 1
    sent = transport.requests[0]
    assert sent.method == "POST"
    assert str(sent.url) == f"{PROD_BASE_URL}/scrape"
    assert sent.headers["authorization"] == "Bearer test-key"
    assert json.loads(sent.content) == {
        "url": PAGE_URL,
        "formats": ["markdown"],
        "onlyMainContent": True,
    }


def test_scrape_markdown_sends_only_main_content_flag() -> None:
    transport = FakeFirecrawlTransport(
        body=fixture_text("firecrawl", "old", "scrape_response_ok.json")
    )
    client = _client(transport)

    asyncio.run(client.scrape_markdown(PAGE_URL, only_main_content=False))

    assert json.loads(transport.requests[0].content)["onlyMainContent"] is False


def test_scrape_markdown_applies_per_call_timeout() -> None:
    transport = FakeFirecrawlTransport(
        body=fixture_text("firecrawl", "old", "scrape_response_ok.json")
    )
    client = _client(transport)

    asyncio.run(client.scrape_markdown(PAGE_URL, timeout_seconds=5))

    assert transport.requests[0].extensions["timeout"]["read"] == 5


def test_scrape_markdown_returns_markdown_and_metadata() -> None:
    transport = FakeFirecrawlTransport(
        body=fixture_text("firecrawl", "old", "scrape_response_ok.json")
    )
    client = _client(transport)

    result = asyncio.run(client.scrape_markdown(PAGE_URL))

    assert "Ocean Freight" in result.markdown
    assert result.raw_status_code == 200
    assert result.page_failed is False
    assert result.metadata.status_code == 200
    assert result.metadata.title == "Fake Carrier Tariff — Santos to Hamburg"
    assert result.metadata.url == PAGE_URL
    assert (
        result.metadata.description == "Ocean tariff rates for the Santos-Hamburg lane"
    )


def test_scrape_markdown_flags_dead_target_page() -> None:
    # Firecrawl wraps a dead page in a 200 success envelope; the target page's
    # status lives in data.metadata.statusCode.
    transport = FakeFirecrawlTransport(
        body=fixture_text("firecrawl", "old", "scrape_response_page_failed.json")
    )
    client = _client(transport)

    result = asyncio.run(client.scrape_markdown(PAGE_URL))

    assert result.raw_status_code == 200
    assert result.page_failed is True
    assert result.metadata.status_code == 404
    assert result.markdown == ""


def test_scrape_markdown_uses_configured_base_url() -> None:
    transport = FakeFirecrawlTransport(
        body=fixture_text("firecrawl", "old", "scrape_response_ok.json")
    )
    client = _client(transport, base_url="https://firecrawl.internal.example.com/v2")

    asyncio.run(client.scrape_markdown(PAGE_URL))

    assert str(transport.requests[0].url) == (
        "https://firecrawl.internal.example.com/v2/scrape"
    )


@pytest.mark.parametrize(
    ("fixture_name", "status", "expected_error"),
    [
        ("error_401.json", 401, FirecrawlAuthError),
        ("error_429.json", 429, FirecrawlApiError),
        ("error_500.json", 500, FirecrawlApiError),
    ],
)
def test_scrape_markdown_maps_error_responses(
    fixture_name: str, status: int, expected_error: type[Exception]
) -> None:
    transport = FakeFirecrawlTransport(
        status=status,
        body=fixture_text("firecrawl", "old", fixture_name),
    )
    client = _client(transport)

    with pytest.raises(expected_error) as excinfo:
        asyncio.run(client.scrape_markdown(PAGE_URL))

    assert excinfo.value.status == status  # type: ignore[attr-defined]
    assert excinfo.value.body is not None  # type: ignore[attr-defined]
    assert excinfo.value.body.error  # type: ignore[attr-defined]


def test_scrape_markdown_api_error_without_json_body() -> None:
    transport = FakeFirecrawlTransport(
        status=503,
        body="upstream unavailable",
        headers={"Content-Type": "text/plain"},
    )
    client = _client(transport)

    with pytest.raises(FirecrawlApiError) as excinfo:
        asyncio.run(client.scrape_markdown(PAGE_URL))

    assert excinfo.value.status == 503
    assert excinfo.value.body is None


def test_scrape_markdown_raises_schema_error_on_bad_success_body() -> None:
    transport = FakeFirecrawlTransport(
        body=fixture_text("firecrawl", "old", "scrape_response_malformed.json")
    )
    client = _client(transport)

    with pytest.raises(FirecrawlSchemaError):
        asyncio.run(client.scrape_markdown(PAGE_URL))


def test_scrape_markdown_raises_schema_error_on_success_false_200() -> None:
    # success:false inside a well-formed envelope still violates the success
    # contract — it is malformed input, not an API error (non-2xx is ApiError).
    transport = FakeFirecrawlTransport(
        status=200,
        body='{"success": false, "data": {"markdown": ""}, '
        '"error": "scrape failed upstream"}',
    )
    client = _client(transport)

    with pytest.raises(FirecrawlSchemaError):
        asyncio.run(client.scrape_markdown(PAGE_URL))


def test_scrape_markdown_raises_transport_error_on_network_failure() -> None:
    transport = FakeFirecrawlTransport(error=httpx.ConnectError("connection refused"))
    client = _client(transport)

    with pytest.raises(FirecrawlTransportError):
        asyncio.run(client.scrape_markdown(PAGE_URL))


def test_from_env_requires_api_key(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("FIRECRAWL_API_KEY", raising=False)

    with pytest.raises(FirecrawlConfigError) as excinfo:
        FirecrawlClient.from_env()

    assert "FIRECRAWL_API_KEY" in str(excinfo.value)


def test_from_env_rejects_empty_api_key(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("FIRECRAWL_API_KEY", "")

    with pytest.raises(FirecrawlConfigError) as excinfo:
        FirecrawlClient.from_env()

    assert "FIRECRAWL_API_KEY" in str(excinfo.value)


def test_from_env_builds_client(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("FIRECRAWL_API_KEY", "env-key")
    monkeypatch.delenv("FIRECRAWL_TIMEOUT_SECONDS", raising=False)

    client = FirecrawlClient.from_env()

    assert isinstance(client, FirecrawlClient)
    asyncio.run(client.aclose())


def test_from_env_accepts_timeout_override(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setenv("FIRECRAWL_API_KEY", "env-key")
    monkeypatch.setenv("FIRECRAWL_TIMEOUT_SECONDS", "90")

    client = FirecrawlClient.from_env()

    assert client._client.timeout.read == 90
    asyncio.run(client.aclose())


@pytest.mark.parametrize("bad_timeout", ["abc", "-1", "0", "nan", "inf"])
def test_from_env_rejects_bad_timeout(
    monkeypatch: pytest.MonkeyPatch, bad_timeout: str
) -> None:
    monkeypatch.setenv("FIRECRAWL_API_KEY", "env-key")
    monkeypatch.setenv("FIRECRAWL_TIMEOUT_SECONDS", bad_timeout)

    with pytest.raises(FirecrawlConfigError) as excinfo:
        FirecrawlClient.from_env()

    assert "FIRECRAWL_TIMEOUT_SECONDS" in str(excinfo.value)


def test_init_rejects_empty_api_key() -> None:
    with pytest.raises(FirecrawlConfigError):
        FirecrawlClient("")


def test_injected_http_client_still_sends_auth_and_is_not_closed() -> None:
    async def _run() -> httpx.AsyncClient:
        transport = FakeFirecrawlTransport(
            body=fixture_text("firecrawl", "old", "scrape_response_ok.json")
        )
        shared = httpx.AsyncClient(base_url=PROD_BASE_URL, transport=transport)
        client = FirecrawlClient("key", http_client=shared)

        await client.scrape_markdown(PAGE_URL)
        await client.aclose()

        assert transport.requests[0].headers["authorization"] == "Bearer key"
        return shared

    shared = asyncio.run(_run())
    assert shared.is_closed is False
    asyncio.run(shared.aclose())


def test_client_context_manager_closes() -> None:
    async def _run() -> None:
        transport = FakeFirecrawlTransport(
            body=fixture_text("firecrawl", "old", "scrape_response_ok.json")
        )
        async with FirecrawlClient("key", transport=transport) as client:
            await client.scrape_markdown(PAGE_URL)
        assert len(transport.requests) == 1

    asyncio.run(_run())
