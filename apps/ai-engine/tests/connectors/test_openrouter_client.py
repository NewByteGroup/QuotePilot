"""Client tests for the openrouter connector: endpoints, auth, error mapping."""

from __future__ import annotations

import asyncio
import json
from decimal import Decimal
from typing import Any

import httpx
import pytest

from connectors.openrouter import (
    PROD_BASE_URL,
    PROD_DECISIONS_URL,
    ChatCompletionRequest,
    DecideRequest,
    EmbeddingRequest,
    OpenRouterClient,
)
from connectors.openrouter.errors import (
    OpenRouterApiError,
    OpenRouterAuthError,
    OpenRouterConfigError,
    OpenRouterSchemaError,
    OpenRouterTransportError,
)
from connectors.openrouter.models import (
    BoxAnswer,
    ChoiceAnswer,
    NoulAnswer,
    ScoreAnswer,
)
from tests.helpers import FakeOpenRouterTransport, fixture_json, fixture_text

EMBEDDING_DIMS = 4  # dimensions of the vectors in embeddings_response.json


def _decide_request() -> DecideRequest:
    return DecideRequest.model_validate_json(
        fixture_text("openrouter", "old", "decisions_request.json")
    )


def _chat_request() -> ChatCompletionRequest:
    return ChatCompletionRequest.model_validate_json(
        fixture_text("openrouter", "old", "chat_request.json")
    )


def _embed_request() -> EmbeddingRequest:
    return EmbeddingRequest(
        model="qwen/qwen3-embedding-8b",
        input=["2x40GP Santos to Hamburg", "FAK 36000 KG"],
    )


def _client(transport: FakeOpenRouterTransport, **kwargs: Any) -> OpenRouterClient:
    return OpenRouterClient("test-key", transport=transport, **kwargs)


def _good_transport(**kwargs: Any) -> FakeOpenRouterTransport:
    kwargs.setdefault(
        "decisions_body",
        fixture_text("openrouter", "old", "decisions_response.json"),
    )
    kwargs.setdefault(
        "chat_body",
        fixture_text("openrouter", "old", "chat_response.json"),
    )
    kwargs.setdefault(
        "embeddings_body",
        fixture_text("openrouter", "old", "embeddings_response.json"),
    )
    return FakeOpenRouterTransport(**kwargs)


def test_decide_posts_to_decisions_url_with_bearer_auth() -> None:
    transport = _good_transport()
    client = _client(transport)

    asyncio.run(client.decide(_decide_request()))

    assert len(transport.decisions_requests) == 1
    sent = transport.decisions_requests[0]
    assert sent.method == "POST"
    assert str(sent.url) == PROD_DECISIONS_URL
    assert sent.headers["authorization"] == "Bearer test-key"
    assert json.loads(sent.content) == fixture_json(
        "openrouter", "old", "decisions_request.json"
    )


def test_decide_omits_unset_optional_fields() -> None:
    transport = _good_transport()
    client = _client(transport)
    request = DecideRequest(
        model="typesafe/jev-1.14",
        state={"subject": "plain state"},
        questions={"is_bug": {"type": "noul", "instructions": "Is it a bug?"}},
    )

    asyncio.run(client.decide(request))

    sent = json.loads(transport.decisions_requests[0].content)
    assert "session_id" not in sent
    assert "user" not in sent
    assert "provider" not in sent
    assert "trace" not in sent
    assert "criteria" not in sent["questions"]["is_bug"]


def test_decide_returns_discriminated_answers_and_decimal_cost() -> None:
    transport = _good_transport()
    client = _client(transport)

    result = asyncio.run(client.decide(_decide_request()))

    assert result.id == "gen-dec-9f3a2b1c4d5e"
    assert result.provider == "TypeSafe"
    is_bug = result.answers["is_bug"]
    assert isinstance(is_bug, NoulAnswer)
    assert is_bug.noul == 0.96
    team = result.answers["team"]
    assert isinstance(team, ChoiceAnswer)
    assert team.choice == "payments"
    urgency = result.answers["urgency"]
    assert isinstance(urgency, ScoreAnswer)
    assert urgency.score == 1.99
    region = result.answers["region"]
    assert isinstance(region, BoxAnswer)
    assert region.box.right == 0.9
    assert result.usage is not None
    assert isinstance(result.usage.cost, Decimal)
    assert result.usage.cost == Decimal("0.000019992")


def test_decide_uses_configured_decisions_url() -> None:
    transport = _good_transport()
    client = _client(transport, decisions_url="https://example.invalid/alpha/decisions")

    asyncio.run(client.decide(_decide_request()))

    assert str(transport.decisions_requests[0].url) == (
        "https://example.invalid/alpha/decisions"
    )


def test_chat_completion_posts_with_auth_and_parses() -> None:
    transport = _good_transport()
    client = _client(transport)

    result = asyncio.run(client.chat_completion(_chat_request()))

    assert len(transport.chat_requests) == 1
    sent = transport.chat_requests[0]
    assert str(sent.url) == f"{PROD_BASE_URL}/chat/completions"
    assert sent.headers["authorization"] == "Bearer test-key"
    assert json.loads(sent.content) == fixture_json(
        "openrouter", "old", "chat_request.json"
    )
    assert result.model == "qwen/qwen3-32b"
    content = result.choices[0].message.content
    assert content is not None
    assert json.loads(content)["port_of_loading"] == "BRSSZ"
    assert result.usage is not None
    assert result.usage.prompt_tokens == 120
    assert result.usage.cost == Decimal("0.0000123")


def test_chat_completion_uses_configured_base_url() -> None:
    transport = _good_transport()
    client = _client(transport, base_url="https://example.invalid/v1")

    asyncio.run(client.chat_completion(_chat_request()))

    assert str(transport.chat_requests[0].url) == (
        "https://example.invalid/v1/chat/completions"
    )


def test_embed_posts_input_list_with_auth_and_validates_dims() -> None:
    transport = _good_transport()
    client = _client(transport)

    result = asyncio.run(
        client.embed(_embed_request(), expected_dimensions=EMBEDDING_DIMS)
    )

    assert len(transport.embeddings_requests) == 1
    sent = transport.embeddings_requests[0]
    assert str(sent.url) == f"{PROD_BASE_URL}/embeddings"
    assert sent.headers["authorization"] == "Bearer test-key"
    assert json.loads(sent.content) == {
        "model": "qwen/qwen3-embedding-8b",
        "input": ["2x40GP Santos to Hamburg", "FAK 36000 KG"],
        "dimensions": EMBEDDING_DIMS,
    }
    assert len(result.data) == 2
    assert all(len(item.embedding) == EMBEDDING_DIMS for item in result.data)


def test_embed_accepts_single_string_input() -> None:
    transport = _good_transport(embeddings_body=_embeddings_body([_embedding_item(0)]))
    client = _client(transport)
    request = EmbeddingRequest(model="qwen/qwen3-embedding-8b", input="one single text")

    asyncio.run(client.embed(request, expected_dimensions=EMBEDDING_DIMS))

    sent = json.loads(transport.embeddings_requests[0].content)
    assert sent["input"] == "one single text"


def test_embed_validates_1024_dim_vectors() -> None:
    # The committed fixture is 4-dim; a 1024-dim reply (qwen3-embedding-8b)
    # is generated here instead of committing a giant fixture.
    body = json.dumps(
        {
            "data": [
                {"object": "embedding", "index": i, "embedding": [0.001 * i] * 1024}
                for i in range(2)
            ],
            "model": "qwen/qwen3-embedding-8b",
            "usage": {"prompt_tokens": 24, "total_tokens": 24},
        }
    )
    transport = _good_transport(embeddings_body=body)
    client = _client(transport)

    result = asyncio.run(client.embed(_embed_request(), expected_dimensions=1024))

    assert len(result.data) == 2
    assert len(result.data[0].embedding) == 1024


def test_embed_rejects_wrong_dimensions() -> None:
    transport = _good_transport()
    client = _client(transport)

    with pytest.raises(OpenRouterSchemaError) as excinfo:
        asyncio.run(client.embed(_embed_request(), expected_dimensions=1024))

    assert "4-dim" in str(excinfo.value)
    assert "1024" in str(excinfo.value)


def _embeddings_body(data: list[dict[str, Any]]) -> str:
    return json.dumps(
        {
            "object": "list",
            "data": data,
            "model": "qwen/qwen3-embedding-8b",
            "usage": {"prompt_tokens": 4, "total_tokens": 4},
        }
    )


def _embedding_item(index: int | None, fill: float = 0.01) -> dict[str, Any]:
    item: dict[str, Any] = {
        "object": "embedding",
        "embedding": [fill] * EMBEDDING_DIMS,
    }
    if index is not None:
        item["index"] = index
    return item


def test_embed_asks_model_for_the_validated_dimension_count() -> None:
    # qwen3-embedding-8b is natively 4096-dim; the MRL hint must go on the
    # wire or the response-length check can never see a 1024-dim vector.
    body = _embeddings_body(
        [
            {"object": "embedding", "index": 0, "embedding": [0.001] * 1024},
            {"object": "embedding", "index": 1, "embedding": [0.002] * 1024},
        ]
    )
    transport = _good_transport(embeddings_body=body)
    client = _client(transport)

    asyncio.run(client.embed(_embed_request(), expected_dimensions=1024))

    sent = json.loads(transport.embeddings_requests[0].content)
    assert sent["dimensions"] == 1024


def test_embed_rejects_request_dimensions_conflicting_with_expected() -> None:
    transport = _good_transport()
    client = _client(transport)
    request = EmbeddingRequest(
        model="qwen/qwen3-embedding-8b",
        input=["a", "b"],
        dimensions=4096,
    )

    with pytest.raises(OpenRouterConfigError) as excinfo:
        asyncio.run(client.embed(request, expected_dimensions=1024))

    assert "4096" in str(excinfo.value)
    assert "1024" in str(excinfo.value)
    assert len(transport.embeddings_requests) == 0


def test_embed_rejects_fewer_items_than_inputs() -> None:
    transport = _good_transport(embeddings_body=_embeddings_body([_embedding_item(0)]))
    client = _client(transport)

    with pytest.raises(OpenRouterSchemaError) as excinfo:
        asyncio.run(client.embed(_embed_request(), expected_dimensions=EMBEDDING_DIMS))

    assert "1 item(s) for 2 input" in str(excinfo.value)


def test_embed_rejects_duplicate_indexes() -> None:
    transport = _good_transport(
        embeddings_body=_embeddings_body([_embedding_item(0), _embedding_item(0)])
    )
    client = _client(transport)

    with pytest.raises(OpenRouterSchemaError) as excinfo:
        asyncio.run(client.embed(_embed_request(), expected_dimensions=EMBEDDING_DIMS))

    assert "[0, 0]" in str(excinfo.value)


def test_embed_returns_batch_sorted_into_input_order() -> None:
    first = {"object": "embedding", "index": 0, "embedding": [0.1] * EMBEDDING_DIMS}
    second = {"object": "embedding", "index": 1, "embedding": [0.2] * EMBEDDING_DIMS}
    transport = _good_transport(embeddings_body=_embeddings_body([second, first]))
    client = _client(transport)

    result = asyncio.run(
        client.embed(_embed_request(), expected_dimensions=EMBEDDING_DIMS)
    )

    assert [item.index for item in result.data] == [0, 1]
    assert result.data[0].embedding == [0.1] * EMBEDDING_DIMS
    assert result.data[1].embedding == [0.2] * EMBEDDING_DIMS


def test_embed_rejects_missing_index_in_batch() -> None:
    transport = _good_transport(
        embeddings_body=_embeddings_body([_embedding_item(0), _embedding_item(None)])
    )
    client = _client(transport)

    with pytest.raises(OpenRouterSchemaError) as excinfo:
        asyncio.run(client.embed(_embed_request(), expected_dimensions=EMBEDDING_DIMS))

    assert "without an index" in str(excinfo.value)


def test_embed_accepts_missing_index_for_single_input() -> None:
    transport = _good_transport(
        embeddings_body=_embeddings_body([_embedding_item(None)])
    )
    client = _client(transport)
    request = EmbeddingRequest(model="qwen/qwen3-embedding-8b", input="one text")

    result = asyncio.run(client.embed(request, expected_dimensions=EMBEDDING_DIMS))

    assert len(result.data) == 1
    assert len(result.data[0].embedding) == EMBEDDING_DIMS


def test_decide_401_raises_auth_error_with_parsed_body() -> None:
    transport = _good_transport(
        decisions_status=401,
        decisions_body=fixture_text("openrouter", "old", "error_401.json"),
    )
    client = _client(transport)

    with pytest.raises(OpenRouterAuthError) as excinfo:
        asyncio.run(client.decide(_decide_request()))

    assert excinfo.value.status == 401
    assert excinfo.value.error is not None
    assert excinfo.value.error.message == "Invalid API key provided"


def test_chat_429_raises_api_error_with_parsed_body() -> None:
    transport = _good_transport(
        chat_status=429,
        chat_body=fixture_text("openrouter", "old", "error_429.json"),
    )
    client = _client(transport)

    with pytest.raises(OpenRouterApiError) as excinfo:
        asyncio.run(client.chat_completion(_chat_request()))

    assert not isinstance(excinfo.value, OpenRouterAuthError)
    assert excinfo.value.status == 429
    assert excinfo.value.error is not None
    assert excinfo.value.error.code == 429
    assert excinfo.value.error.metadata is not None
    assert excinfo.value.error.metadata["retry_after_seconds"] == 20


def test_chat_403_raises_auth_error() -> None:
    transport = _good_transport(chat_status=403, chat_body="")
    client = _client(transport)

    with pytest.raises(OpenRouterAuthError) as excinfo:
        asyncio.run(client.chat_completion(_chat_request()))

    assert excinfo.value.status == 403
    assert excinfo.value.error is None


def test_embed_500_raises_api_error_without_envelope() -> None:
    transport = _good_transport(
        embeddings_status=500,
        embeddings_body="upstream exploded",
        embeddings_headers={"Content-Type": "text/plain"},
    )
    client = _client(transport)

    with pytest.raises(OpenRouterApiError) as excinfo:
        asyncio.run(client.embed(_embed_request(), expected_dimensions=EMBEDDING_DIMS))

    assert excinfo.value.status == 500
    assert excinfo.value.error is None


def test_decide_malformed_success_body_raises_schema_error() -> None:
    transport = _good_transport(
        decisions_body=fixture_text(
            "openrouter", "old", "decisions_response_malformed.json"
        )
    )
    client = _client(transport)

    with pytest.raises(OpenRouterSchemaError):
        asyncio.run(client.decide(_decide_request()))


def test_chat_malformed_success_body_raises_schema_error() -> None:
    transport = _good_transport(
        chat_body=fixture_text(
            "openrouter", "old", "chat_response_schema_violation.json"
        )
    )
    client = _client(transport)

    with pytest.raises(OpenRouterSchemaError):
        asyncio.run(client.chat_completion(_chat_request()))


def test_decide_transport_error() -> None:
    transport = _good_transport(
        decisions_error=httpx.ConnectError("connection refused")
    )
    client = _client(transport)

    with pytest.raises(OpenRouterTransportError):
        asyncio.run(client.decide(_decide_request()))


def test_chat_transport_error() -> None:
    transport = _good_transport(chat_error=httpx.ConnectError("connection refused"))
    client = _client(transport)

    with pytest.raises(OpenRouterTransportError):
        asyncio.run(client.chat_completion(_chat_request()))


def test_embed_transport_error() -> None:
    transport = _good_transport(
        embeddings_error=httpx.ConnectError("connection refused")
    )
    client = _client(transport)

    with pytest.raises(OpenRouterTransportError):
        asyncio.run(client.embed(_embed_request(), expected_dimensions=EMBEDDING_DIMS))


def test_init_rejects_empty_api_key() -> None:
    transport = _good_transport()

    with pytest.raises(OpenRouterConfigError):
        OpenRouterClient("", transport=transport)


def test_from_env_requires_api_key(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("OPENROUTER_API_KEY", raising=False)

    with pytest.raises(OpenRouterConfigError) as excinfo:
        OpenRouterClient.from_env()

    assert "OPENROUTER_API_KEY" in str(excinfo.value)


def test_from_env_rejects_empty_api_key(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("OPENROUTER_API_KEY", "")

    with pytest.raises(OpenRouterConfigError) as excinfo:
        OpenRouterClient.from_env()

    assert "OPENROUTER_API_KEY" in str(excinfo.value)


def test_from_env_builds_client(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("OPENROUTER_API_KEY", "env-key")
    monkeypatch.setenv("OPENROUTER_TIMEOUT_SECONDS", "5")

    client = OpenRouterClient.from_env()

    assert isinstance(client, OpenRouterClient)
    asyncio.run(client.aclose())


def test_from_env_uses_url_overrides(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("OPENROUTER_API_KEY", "env-key")
    monkeypatch.setenv("OPENROUTER_BASE_URL", "https://example.invalid/v2")
    monkeypatch.setenv(
        "OPENROUTER_DECISIONS_URL", "https://decisions.example.invalid/alpha/decisions"
    )
    transport = _good_transport()

    client = OpenRouterClient.from_env(transport=transport)

    async def _run() -> None:
        await client.decide(_decide_request())
        await client.chat_completion(_chat_request())

    asyncio.run(_run())

    assert str(transport.decisions_requests[0].url) == (
        "https://decisions.example.invalid/alpha/decisions"
    )
    assert str(transport.chat_requests[0].url) == (
        "https://example.invalid/v2/chat/completions"
    )
    assert transport.decisions_requests[0].headers["authorization"] == (
        "Bearer env-key"
    )


def test_from_env_blank_optional_urls_fall_back_to_production() -> None:
    # .env.example exports OPENROUTER_BASE_URL= / OPENROUTER_DECISIONS_URL=
    # empty; blank must behave like unset, not override the prod default.
    transport = _good_transport()
    client = OpenRouterClient.from_env(
        env={
            "OPENROUTER_API_KEY": "env-key",
            "OPENROUTER_BASE_URL": "",
            "OPENROUTER_DECISIONS_URL": "",
        },
        transport=transport,
    )

    async def _run() -> None:
        await client.decide(_decide_request())
        await client.chat_completion(_chat_request())

    asyncio.run(_run())

    assert str(transport.decisions_requests[0].url) == PROD_DECISIONS_URL
    assert str(transport.chat_requests[0].url) == f"{PROD_BASE_URL}/chat/completions"
    asyncio.run(client.aclose())


@pytest.mark.parametrize("bad_timeout", ["abc", "-1", "0", "nan", "inf"])
def test_from_env_rejects_bad_timeout(
    monkeypatch: pytest.MonkeyPatch, bad_timeout: str
) -> None:
    monkeypatch.setenv("OPENROUTER_API_KEY", "env-key")
    monkeypatch.setenv("OPENROUTER_TIMEOUT_SECONDS", bad_timeout)

    with pytest.raises(OpenRouterConfigError) as excinfo:
        OpenRouterClient.from_env()

    assert "OPENROUTER_TIMEOUT_SECONDS" in str(excinfo.value)


def test_injected_http_client_still_sends_auth_and_is_not_closed() -> None:
    async def _run() -> httpx.AsyncClient:
        transport = _good_transport()
        shared = httpx.AsyncClient(base_url=PROD_BASE_URL, transport=transport)
        client = OpenRouterClient("id-key", http_client=shared)

        await client.chat_completion(_chat_request())
        await client.aclose()

        assert transport.chat_requests[0].headers["authorization"] == ("Bearer id-key")
        return shared

    shared = asyncio.run(_run())
    assert shared.is_closed is False
    asyncio.run(shared.aclose())


def test_client_context_manager_closes() -> None:
    async def _run() -> None:
        transport = _good_transport()
        async with OpenRouterClient("key", transport=transport) as client:
            await client.decide(_decide_request())
            await client.chat_completion(_chat_request())
            await client.embed(_embed_request(), expected_dimensions=EMBEDDING_DIMS)
        assert len(transport.requests) == 3

    asyncio.run(_run())
