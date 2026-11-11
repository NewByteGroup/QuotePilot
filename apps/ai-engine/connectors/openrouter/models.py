"""Pydantic models for the OpenRouter API surfaces QuotePilot calls.

Three surfaces per the ADR-006 model matrix:

- ``POST /api/alpha/decisions`` — the TypeSafe/Jev "System One" decision
  API (``typesafe/jev-1.14``) used by the classifier and confidence auditor.
  Requests carry arbitrary JSON ``state`` plus a ``questions`` map; answers
  come back as a discriminated union on ``type`` (noul/choice/score/box).
- ``POST /api/v1/chat/completions`` — OpenAI-compatible chat used by the
  RFQ extractor; only the fields the platform consumes are modeled.
- ``POST /api/v1/embeddings`` — OpenAI-style embeddings
  (``qwen/qwen3-embedding-8b``, 1024 dims).

Field names are already snake_case upstream, so no alias generator is
needed; unknown keys are ignored (``extra="ignore"``) both ways.

``usage.cost`` arrives as ``Decimal`` (USD), never a float — the money
rule in AGENTS.md; ``model_validate_json`` parses the JSON literal
without materializing a float. ``state`` accepts arbitrary JSON via
``pydantic.JsonValue``.
"""

from __future__ import annotations

from decimal import Decimal
from typing import Annotated, Literal

from pydantic import BaseModel, ConfigDict, Field, JsonValue


class OpenRouterModel(BaseModel):
    """Base model: immutable, unknown keys ignored (untrusted input)."""

    model_config = ConfigDict(extra="ignore", frozen=True)


# ---------------------------------------------------------------------------
# Decisions API (POST /api/alpha/decisions)
# ---------------------------------------------------------------------------


class NoulQuestion(OpenRouterModel):
    """Polar question answered by a probability ("not only true/false").

    ``criteria`` optionally names the two poles, e.g.
    ``{"true": "...", "false": "..."}``.
    """

    type: Literal["noul"]
    instructions: str
    criteria: dict[str, str] | None = None


class ChoiceQuestion(OpenRouterModel):
    """Single-choice question; ``criteria`` maps each option to a description."""

    type: Literal["choice"]
    instructions: str
    criteria: dict[str, str] = Field(min_length=1)


class ScoreQuestion(OpenRouterModel):
    """Ordinal score question; ``criteria`` lists low→high ordered levels."""

    type: Literal["score"]
    instructions: str
    criteria: list[str] = Field(min_length=1)


class BoxQuestion(OpenRouterModel):
    """Spatial question answered by a bounding box; has no ``criteria``."""

    type: Literal["box"]
    instructions: str


Question = Annotated[
    NoulQuestion | ChoiceQuestion | ScoreQuestion | BoxQuestion,
    Field(discriminator="type"),
]
"""One entry of a ``DecideRequest.questions`` map, discriminated on ``type``."""


class DecideRequest(OpenRouterModel):
    """Request body of ``POST /decisions``.

    ``state`` is arbitrary JSON (string, object or array) describing the
    case under judgment. ``provider`` and ``trace`` are pass-through
    fields whose upstream shape is not fixed, so they accept any JSON.
    """

    model: str
    state: JsonValue
    questions: dict[str, Question] = Field(min_length=1)
    session_id: str | None = None
    user: str | None = None
    provider: JsonValue = None
    trace: JsonValue = None


class DecideUsage(OpenRouterModel):
    """Token accounting of a decision call; ``cost`` is USD as ``Decimal``."""

    input_tokens: int
    output_tokens: int
    cost: Decimal | None = None


class NoulAnswer(OpenRouterModel):
    """Polar answer: ``noul`` is the probability the statement is true.

    ``probabilities``/``confidence`` are optional upstream — callers must
    presence-check before reading them.
    """

    type: Literal["noul"]
    noul: float
    probabilities: dict[str, float] | None = None
    confidence: float | None = None


class ChoiceAnswer(OpenRouterModel):
    """Single-choice answer with the winning option and its distribution."""

    type: Literal["choice"]
    choice: str
    probabilities: dict[str, float] | None = None
    confidence: float | None = None


class ScoreAnswer(OpenRouterModel):
    """Ordinal score answer; ``legend`` maps score indexes to level names."""

    type: Literal["score"]
    score: float
    legend: dict[str, str] | None = None
    probabilities: dict[str, float] | None = None
    confidence: float | None = None


class BoxGeometry(OpenRouterModel):
    """A bounding box as normalized top/bottom/left/right coordinates."""

    top: float
    bottom: float
    left: float
    right: float


class BoxAnswer(OpenRouterModel):
    """Spatial answer: the model drew a bounding box over the state."""

    type: Literal["box"]
    box: BoxGeometry
    probabilities: dict[str, float] | None = None
    confidence: float | None = None


Answer = Annotated[
    NoulAnswer | ChoiceAnswer | ScoreAnswer | BoxAnswer,
    Field(discriminator="type"),
]
"""One entry of a ``DecideResponse.answers`` map, discriminated on ``type``."""


class DecideResponse(OpenRouterModel):
    """Body of a ``2xx`` reply to ``POST /decisions``."""

    id: str
    model: str
    provider: str | None = None
    answers: dict[str, Answer]
    usage: DecideUsage | None = None


# ---------------------------------------------------------------------------
# Chat completions (POST {base_url}/chat/completions, OpenAI-compatible)
# ---------------------------------------------------------------------------


class ChatMessage(OpenRouterModel):
    """One message of a chat-completion request (``role``/``content``)."""

    role: str
    content: str


class ChatCompletionRequest(OpenRouterModel):
    """Request body of ``POST /chat/completions``.

    ``response_format`` is a pass-through object (e.g. OpenAI's
    ``{"type": "json_schema", "json_schema": {...}}``) forwarded verbatim
    so the extractor can pin structured output.
    """

    model: str
    messages: list[ChatMessage] = Field(min_length=1)
    temperature: float | None = None
    response_format: dict[str, JsonValue] | None = None
    max_tokens: int | None = None


class ChatChoiceMessage(OpenRouterModel):
    """Assistant message inside a completion ``choice``.

    ``content`` is optional: upstream may return ``null`` with a refusal
    or tool call, so callers must presence-check.
    """

    role: str | None = None
    content: str | None = None


class ChatChoice(OpenRouterModel):
    """One completion choice; only ``message`` is required upstream."""

    index: int | None = None
    message: ChatChoiceMessage
    finish_reason: str | None = None


class ChatUsage(OpenRouterModel):
    """OpenAI-style token accounting; ``cost`` is USD as ``Decimal``."""

    prompt_tokens: int
    completion_tokens: int
    total_tokens: int
    cost: Decimal | None = None


class ChatCompletionResponse(OpenRouterModel):
    """Body of a ``2xx`` reply to ``POST /chat/completions``.

    ``choices`` must be non-empty: an empty list means upstream produced
    nothing the caller can read, which is a schema violation at the
    connector boundary rather than an ``IndexError`` downstream.
    """

    id: str | None = None
    model: str
    choices: list[ChatChoice] = Field(min_length=1)
    usage: ChatUsage | None = None


# ---------------------------------------------------------------------------
# Embeddings (POST {base_url}/embeddings, OpenAI-style)
# ---------------------------------------------------------------------------


class EmbeddingRequest(OpenRouterModel):
    """Request body of ``POST /embeddings``; ``input`` is one or many texts.

    ``dimensions`` is the OpenAI-compatible dimension hint for MRL models
    (Matryoshka Representation Learning): when set, the model truncates
    each vector to that many dims. ``embed()`` overwrites it with the
    caller's ``expected_dimensions`` so the request always asks for the
    size the response will be validated against.
    """

    model: str
    input: str | list[str]
    dimensions: int | None = None


class EmbeddingItem(OpenRouterModel):
    """One embedding vector of the response ``data`` array."""

    embedding: list[float]
    index: int | None = None
    object: str | None = None


class EmbeddingUsage(OpenRouterModel):
    """Token accounting of an embeddings call (fields optional upstream)."""

    prompt_tokens: int | None = None
    total_tokens: int | None = None


class EmbeddingResponse(OpenRouterModel):
    """Body of a ``2xx`` reply to ``POST /embeddings``."""

    data: list[EmbeddingItem]
    model: str
    usage: EmbeddingUsage | None = None


# ---------------------------------------------------------------------------
# Error envelope (non-2xx bodies)
# ---------------------------------------------------------------------------


class ErrorBody(OpenRouterModel):
    """The ``error`` object OpenRouter returns on non-2xx replies.

    Shape: ``{"error": {"message": "...", "code": 401, "metadata": {...}}}``.
    ``code`` is usually an HTTP status int but string codes occur, so both
    are accepted.
    """

    message: str | None = None
    code: int | str | None = None
    metadata: dict[str, JsonValue] | None = None


class ErrorEnvelope(OpenRouterModel):
    """Non-2xx body: ``{"error": {...}}`` wrapping an ``ErrorBody``."""

    error: ErrorBody
