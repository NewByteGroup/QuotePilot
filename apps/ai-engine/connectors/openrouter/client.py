"""Async client for the OpenRouter API.

Three surfaces from the ADR-006 model matrix:

- ``decide()`` — ``POST {decisions_url}`` (the TypeSafe/Jev "System One"
  decision API, ``typesafe/jev-1.14``) used by the classifier and the
  confidence auditor;
- ``chat_completion()`` — OpenAI-compatible ``POST /chat/completions``
  used by the RFQ extractor (``response_format`` carries the JSON schema);
- ``embed()`` — ``POST /embeddings`` (``qwen/qwen3-embedding-8b``, 1024
  dims); every returned vector is checked against the caller-supplied
  ``expected_dimensions``.

Every response is untrusted input: bodies are schema-validated into
pydantic models before reaching the caller, and ``usage.cost`` arrives
as ``Decimal`` (USD) — never a float.

The connector does no retries and no payload logging: retry/backoff
policy belongs to the River stage that calls ai-engine (ADR-004), and
tenant content must not reach logs (ADR-001).
"""

from __future__ import annotations

import os
from collections.abc import Mapping
from types import TracebackType
from typing import Self

import httpx
from pydantic import ValidationError

from connectors._common import parse_timeout_seconds
from connectors.openrouter.errors import (
    OpenRouterApiError,
    OpenRouterAuthError,
    OpenRouterConfigError,
    OpenRouterSchemaError,
    OpenRouterTransportError,
)
from connectors.openrouter.models import (
    ChatCompletionRequest,
    ChatCompletionResponse,
    DecideRequest,
    DecideResponse,
    EmbeddingItem,
    EmbeddingRequest,
    EmbeddingResponse,
    ErrorEnvelope,
    OpenRouterModel,
)

PROD_BASE_URL = "https://openrouter.ai/api/v1"
PROD_DECISIONS_URL = "https://openrouter.ai/api/alpha/decisions"
DEFAULT_TIMEOUT_SECONDS = 30.0

_ENV_API_KEY = "OPENROUTER_API_KEY"
_ENV_BASE_URL = "OPENROUTER_BASE_URL"
_ENV_DECISIONS_URL = "OPENROUTER_DECISIONS_URL"
_ENV_TIMEOUT = "OPENROUTER_TIMEOUT_SECONDS"


class OpenRouterClient:
    """Client for the OpenRouter decisions, chat and embeddings APIs.

    Args:
        api_key: OpenRouter API key (``sk-or-...``) from openrouter.ai/keys.
        base_url: OpenAI-compatible API root; defaults to production.
        decisions_url: TypeSafe/Jev decisions endpoint (an absolute URL,
            independent of ``base_url``); defaults to production.
        timeout_seconds: total request timeout; retries are the caller's job.
        transport: httpx transport override — tests inject a fake here.
        http_client: shared ``httpx.AsyncClient``; when given, ``base_url``,
            ``timeout_seconds`` and ``transport`` are ignored and the client
            is not closed by ``aclose()``. Auth still goes per-request, so
            an injected client does not need headers configured.

    Example:
        >>> async with OpenRouterClient.from_env() as client:
        ...     decision = await client.decide(decide_request)
        ...     noul = decision.answers["is_bug"]  # NoulAnswer
    """

    def __init__(
        self,
        api_key: str,
        *,
        base_url: str = PROD_BASE_URL,
        decisions_url: str = PROD_DECISIONS_URL,
        timeout_seconds: float = DEFAULT_TIMEOUT_SECONDS,
        transport: httpx.AsyncBaseTransport | None = None,
        http_client: httpx.AsyncClient | None = None,
    ) -> None:
        if not api_key:
            raise OpenRouterConfigError(
                "api_key must be non-empty; "
                "expected an OpenRouter API key from openrouter.ai/keys"
            )
        # Auth goes on each request so an injected http_client works as-is.
        self._request_headers = {
            "Authorization": f"Bearer {api_key}",
            "Accept": "application/json",
        }
        self._decisions_url = decisions_url
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
        """Build a client from ``OPENROUTER_*`` environment variables.

        Required: ``OPENROUTER_API_KEY``.
        Optional: ``OPENROUTER_BASE_URL``, ``OPENROUTER_DECISIONS_URL``
        (both default to production), ``OPENROUTER_TIMEOUT_SECONDS``
        (default 30).

        Example:
            >>> client = OpenRouterClient.from_env()  # reads os.environ
        """
        source = os.environ if env is None else env
        if not source.get(_ENV_API_KEY):
            raise OpenRouterConfigError(
                f"missing required environment variable: {_ENV_API_KEY}; "
                "expected an OpenRouter API key from openrouter.ai/keys"
            )
        return cls(
            api_key=source[_ENV_API_KEY],
            # ``or`` so exported-but-empty URL vars still mean production.
            base_url=source.get(_ENV_BASE_URL) or PROD_BASE_URL,
            decisions_url=source.get(_ENV_DECISIONS_URL) or PROD_DECISIONS_URL,
            timeout_seconds=parse_timeout_seconds(
                source.get(_ENV_TIMEOUT),
                env_name=_ENV_TIMEOUT,
                default=DEFAULT_TIMEOUT_SECONDS,
                error_type=OpenRouterConfigError,
            ),
            transport=transport,
            http_client=http_client,
        )

    async def decide(self, request: DecideRequest) -> DecideResponse:
        """Ask the TypeSafe/Jev decision API (``POST {decisions_url}``).

        Args:
            request: model id, arbitrary-JSON ``state`` and the ``questions``
                map (noul/choice/score/box).

        Returns:
            ``DecideResponse`` with ``answers`` discriminated per question
            type. ``probabilities``, ``confidence`` and ``legend`` are
            optional upstream — presence-check before reading.

        Raises:
            OpenRouterApiError: non-2xx response (error body parsed when
                it matches the ``{"error": {...}}`` envelope).
            OpenRouterAuthError: 401/403 — check the API key.
            OpenRouterSchemaError: success body violating the schema.
            OpenRouterTransportError: DNS/TLS/timeout/protocol failure.
        """
        raw = await self._post(self._decisions_url, request, operation="decisions")
        if not _is_success(raw):
            raise _to_api_error(raw, operation="decisions")
        return _validated(raw, DecideResponse, "decisions")

    async def chat_completion(
        self, request: ChatCompletionRequest
    ) -> ChatCompletionResponse:
        """Call an OpenAI-compatible chat model (``POST /chat/completions``).

        Args:
            request: model id, ``messages``, optional ``temperature``,
                ``response_format`` (JSON-schema structured output) and
                ``max_tokens``.

        Returns:
            ``ChatCompletionResponse``; the assistant text is
            ``response.choices[0].message.content`` (``None`` means the
            model answered with a refusal/tool call instead).

        Raises:
            OpenRouterApiError: non-2xx response (error body parsed when
                it matches the ``{"error": {...}}`` envelope).
            OpenRouterAuthError: 401/403 — check the API key.
            OpenRouterSchemaError: success body violating the schema.
            OpenRouterTransportError: DNS/TLS/timeout/protocol failure.
        """
        raw = await self._post("chat/completions", request, operation="chat")
        if not _is_success(raw):
            raise _to_api_error(raw, operation="chat")
        return _validated(raw, ChatCompletionResponse, "chat")

    async def embed(
        self, request: EmbeddingRequest, *, expected_dimensions: int
    ) -> EmbeddingResponse:
        """Embed one or more texts (``POST /embeddings``).

        Args:
            request: model id (e.g. ``qwen/qwen3-embedding-8b``) and
                ``input`` — a string or a list of strings.
            expected_dimensions: vector length the embedding model must
                produce (1024 for qwen3-embedding-8b); a mismatch means the
                upstream model is not what the pipeline was configured for.

        Returns:
            ``EmbeddingResponse`` with one ``EmbeddingItem`` per input.

        Raises:
            OpenRouterApiError: non-2xx response (error body parsed when
                it matches the ``{"error": {...}}`` envelope).
            OpenRouterAuthError: 401/403 — check the API key.
            OpenRouterConfigError: ``request.dimensions`` set but different
                from ``expected_dimensions``.
            OpenRouterSchemaError: success body violating the schema, an
                item count/index mismatch against the inputs, or a vector
                whose length differs from ``expected_dimensions``.
            OpenRouterTransportError: DNS/TLS/timeout/protocol failure.
        """
        if request.dimensions is not None and request.dimensions != expected_dimensions:
            raise OpenRouterConfigError(
                f"EmbeddingRequest.dimensions={request.dimensions} conflicts "
                f"with expected_dimensions={expected_dimensions}; set it to "
                "the validated size or leave it unset"
            )
        # Always ask the model for exactly what will be validated: MRL
        # models like qwen3-embedding-8b truncate to ``dimensions`` on
        # request, and silence means the full-size vector comes back.
        outbound = request.model_copy(update={"dimensions": expected_dimensions})
        raw = await self._post("embeddings", outbound, operation="embeddings")
        if not _is_success(raw):
            raise _to_api_error(raw, operation="embeddings")
        response = _validated(raw, EmbeddingResponse, "embeddings")
        data = _aligned_items(response.data, request.input)
        for item in data:
            if len(item.embedding) != expected_dimensions:
                raise OpenRouterSchemaError(
                    f"POST embeddings returned a {len(item.embedding)}-dim "
                    f"vector at index {item.index}; "
                    f"expected {expected_dimensions} dims — check the "
                    "configured embedding model"
                )
        return response.model_copy(update={"data": data})

    async def _post(
        self, url: str, request: OpenRouterModel, *, operation: str
    ) -> httpx.Response:
        try:
            return await self._client.post(
                url,
                content=request.model_dump_json(exclude_none=True),
                headers={
                    **self._request_headers,
                    "Content-Type": "application/json",
                },
            )
        except httpx.HTTPError as exc:
            raise OpenRouterTransportError(
                f"POST {operation} ({url}) transport failure: {exc}"
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


def _is_success(response: httpx.Response) -> bool:
    return 200 <= response.status_code < 300


def _aligned_items(
    data: list[EmbeddingItem], request_input: str | list[str]
) -> list[EmbeddingItem]:
    """Check ``data`` aligns with ``request_input`` and return it in input order.

    The response ``data`` array is untrusted: it must carry exactly one item
    per input text. For batches, every item must also carry an ``index`` and
    the indexes must be a permutation of ``0..count-1`` — the caller always
    receives items sorted into input order. A single input may omit
    ``index`` upstream.
    """
    expected_count = 1 if isinstance(request_input, str) else len(request_input)
    if len(data) != expected_count:
        raise OpenRouterSchemaError(
            f"POST embeddings returned {len(data)} item(s) for "
            f"{expected_count} input text(s); expected one vector per input"
        )
    if expected_count == 1:
        return data
    indexed: list[tuple[int, EmbeddingItem]] = []
    for item in data:
        if item.index is None:
            raise OpenRouterSchemaError(
                "POST embeddings returned an item without an index in a "
                f"{expected_count}-input batch; cannot align vectors to inputs"
            )
        indexed.append((item.index, item))
    indexes = [index for index, _ in indexed]
    if sorted(indexes) != list(range(expected_count)):
        raise OpenRouterSchemaError(
            f"POST embeddings returned item indexes {indexes}; expected a "
            f"permutation of 0..{expected_count - 1} for {expected_count} inputs"
        )
    return [item for _, item in sorted(indexed, key=lambda pair: pair[0])]


def _validated[ResponseT: OpenRouterModel](
    response: httpx.Response, model: type[ResponseT], operation: str
) -> ResponseT:
    try:
        return model.model_validate_json(response.text)
    except ValidationError as exc:
        raise OpenRouterSchemaError(
            f"POST {operation} returned success but the body violates the "
            f"{model.__name__} schema: {exc}"
        ) from exc


def _to_api_error(response: httpx.Response, *, operation: str) -> OpenRouterApiError:
    try:
        envelope = ErrorEnvelope.model_validate_json(response.text)
    except ValidationError:
        envelope = None
    body = envelope.error if envelope else None
    summary = (body.message or "") if body else ""
    message = (
        f"POST {operation} failed: HTTP {response.status_code} "
        f"— {summary or response.text[:200] or 'no details in response'}"
    )
    if response.status_code in (401, 403):
        return OpenRouterAuthError(message, status=response.status_code, error=body)
    return OpenRouterApiError(message, status=response.status_code, error=body)
