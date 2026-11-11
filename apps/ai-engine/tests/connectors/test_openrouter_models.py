"""Model tests for the openrouter connector: schema fidelity and Decimal cost."""

from __future__ import annotations

import json
from decimal import Decimal

import pytest
from pydantic import ValidationError

from connectors.openrouter.models import (
    BoxAnswer,
    BoxQuestion,
    ChatCompletionRequest,
    ChatCompletionResponse,
    ChatMessage,
    ChoiceAnswer,
    ChoiceQuestion,
    DecideRequest,
    DecideResponse,
    EmbeddingRequest,
    EmbeddingResponse,
    ErrorEnvelope,
    NoulAnswer,
    NoulQuestion,
    Question,
    ScoreAnswer,
    ScoreQuestion,
)
from tests.helpers import fixture_json, fixture_text


def test_decide_request_round_trips_fixture() -> None:
    payload = fixture_text("openrouter", "old", "decisions_request.json")
    request = DecideRequest.model_validate_json(payload)
    assert request.model_dump(mode="json", exclude_none=True) == json.loads(payload)


def test_decide_request_requires_questions() -> None:
    with pytest.raises(ValidationError):
        DecideRequest.model_validate({"model": "typesafe/jev-1.14", "state": {"a": 1}})


def test_decide_request_rejects_empty_questions() -> None:
    with pytest.raises(ValidationError):
        DecideRequest.model_validate(
            {"model": "typesafe/jev-1.14", "state": None, "questions": {}}
        )


def test_decide_state_accepts_string_object_and_array() -> None:
    for state in ("plain text", {"nested": {"k": [1, 2]}}, ["a", "b"], None):
        request = DecideRequest(
            model="typesafe/jev-1.14",
            state=state,
            questions={"q": {"type": "noul", "instructions": "i"}},
        )
        assert request.state == state


def test_question_union_serializes_each_type() -> None:
    questions: dict[str, Question] = {
        "n": NoulQuestion(
            type="noul",
            instructions="i",
            criteria={"true": "yes", "false": "no"},
        ),
        "c": ChoiceQuestion(
            type="choice",
            instructions="i",
            criteria={"opt_a": "first", "opt_b": "second"},
        ),
        "s": ScoreQuestion(type="score", instructions="i", criteria=["low", "high"]),
        "b": BoxQuestion(type="box", instructions="i"),
    }
    request = DecideRequest(
        model="typesafe/jev-1.14", state={"x": 1}, questions=questions
    )
    dumped = request.model_dump(mode="json", exclude_none=True)["questions"]
    assert dumped["n"]["criteria"] == {"true": "yes", "false": "no"}
    assert dumped["c"]["criteria"] == {"opt_a": "first", "opt_b": "second"}
    assert dumped["s"]["criteria"] == ["low", "high"]
    assert "criteria" not in dumped["b"]


def test_choice_question_requires_criteria() -> None:
    with pytest.raises(ValidationError):
        ChoiceQuestion.model_validate({"type": "choice", "instructions": "i"})


def test_score_question_requires_criteria() -> None:
    with pytest.raises(ValidationError):
        ScoreQuestion.model_validate({"type": "score", "instructions": "i"})


def test_question_rejects_unknown_type() -> None:
    with pytest.raises(ValidationError):
        DecideRequest.model_validate(
            {
                "model": "m",
                "state": {},
                "questions": {"q": {"type": "essay", "instructions": "i"}},
            }
        )


def _decide_response() -> DecideResponse:
    return DecideResponse.model_validate_json(
        fixture_text("openrouter", "old", "decisions_response.json")
    )


def test_decide_response_discriminates_answers() -> None:
    response = _decide_response()
    assert isinstance(response.answers["is_bug"], NoulAnswer)
    assert isinstance(response.answers["team"], ChoiceAnswer)
    assert isinstance(response.answers["urgency"], ScoreAnswer)
    assert isinstance(response.answers["region"], BoxAnswer)


def test_decide_response_parses_answer_fields() -> None:
    response = _decide_response()
    team = response.answers["team"]
    assert isinstance(team, ChoiceAnswer)
    assert team.choice == "payments"
    assert team.probabilities == {"payments": 0.78, "frontend": 0.22}
    assert team.confidence == 0.67
    urgency = response.answers["urgency"]
    assert isinstance(urgency, ScoreAnswer)
    assert urgency.legend == {"0": "low", "1": "mid", "2": "high"}
    region = response.answers["region"]
    assert isinstance(region, BoxAnswer)
    assert region.box.top == 0.1
    assert region.box.bottom == 0.4
    assert region.box.left == 0.6
    assert region.box.right == 0.9


def test_decide_response_optional_fields_default_to_none() -> None:
    response = _decide_response()
    is_bug = response.answers["is_bug"]
    assert isinstance(is_bug, NoulAnswer)
    # The fixture's noul answer carries only {type, noul}.
    assert is_bug.probabilities is None
    assert is_bug.confidence is None
    region = response.answers["region"]
    assert isinstance(region, BoxAnswer)
    assert region.probabilities is None


def test_decide_response_cost_is_decimal_never_float() -> None:
    response = _decide_response()
    assert response.usage is not None
    # 0.000019992 has no exact binary representation: only a parse path that
    # never materializes a float yields this exact Decimal.
    assert isinstance(response.usage.cost, Decimal)
    assert response.usage.cost == Decimal("0.000019992")


def test_decide_response_rejects_malformed_body() -> None:
    with pytest.raises(ValidationError):
        DecideResponse.model_validate_json(
            fixture_text("openrouter", "old", "decisions_response_malformed.json")
        )


def test_chat_request_round_trips_fixture() -> None:
    payload = fixture_text("openrouter", "old", "chat_request.json")
    request = ChatCompletionRequest.model_validate_json(payload)
    assert request.model_dump(mode="json", exclude_none=True) == json.loads(payload)


def test_chat_request_omits_unset_optional_fields() -> None:
    request = ChatCompletionRequest(
        model="qwen/qwen3-32b",
        messages=[ChatMessage(role="user", content="hi")],
    )
    dumped = request.model_dump(mode="json", exclude_none=True)
    assert dumped == {
        "model": "qwen/qwen3-32b",
        "messages": [{"role": "user", "content": "hi"}],
    }


def test_chat_request_requires_messages() -> None:
    with pytest.raises(ValidationError):
        ChatCompletionRequest.model_validate({"model": "m", "messages": []})


def test_chat_response_parses_fixture() -> None:
    response = ChatCompletionResponse.model_validate_json(
        fixture_text("openrouter", "old", "chat_response.json")
    )
    assert response.model == "qwen/qwen3-32b"
    choice = response.choices[0]
    assert choice.index == 0
    assert choice.finish_reason == "stop"
    assert choice.message.role == "assistant"
    assert choice.message.content is not None
    assert json.loads(choice.message.content)["port_of_discharge"] == "DEHAM"
    assert response.usage is not None
    assert response.usage.total_tokens == 165
    assert isinstance(response.usage.cost, Decimal)


def test_chat_response_rejects_missing_message() -> None:
    with pytest.raises(ValidationError):
        ChatCompletionResponse.model_validate_json(
            fixture_text("openrouter", "old", "chat_response_schema_violation.json")
        )


def test_chat_response_rejects_empty_choices() -> None:
    with pytest.raises(ValidationError):
        ChatCompletionResponse.model_validate({"model": "m", "choices": []})


def test_chat_response_ignores_unknown_fields() -> None:
    body = fixture_json("openrouter", "old", "chat_response.json")
    body["system_fingerprint"] = "fp_abc123"
    body["choices"][0]["logprobs"] = {"content": []}
    response = ChatCompletionResponse.model_validate(body)
    assert response.choices[0].message.content is not None


def test_embedding_request_accepts_str_or_list() -> None:
    single = EmbeddingRequest(model="m", input="one")
    batch = EmbeddingRequest(model="m", input=["a", "b"])
    assert single.model_dump(mode="json")["input"] == "one"
    assert batch.model_dump(mode="json")["input"] == ["a", "b"]


def test_embedding_response_parses_fixture() -> None:
    response = EmbeddingResponse.model_validate_json(
        fixture_text("openrouter", "old", "embeddings_response.json")
    )
    assert response.model == "qwen/qwen3-embedding-8b"
    assert len(response.data) == 2
    assert response.data[0].index == 0
    assert len(response.data[0].embedding) == 4
    assert response.usage is not None
    assert response.usage.prompt_tokens == 24


def test_embedding_response_requires_model() -> None:
    with pytest.raises(ValidationError):
        EmbeddingResponse.model_validate({"data": []})


def test_error_envelope_parses_openrouter_shape() -> None:
    envelope = ErrorEnvelope.model_validate_json(
        fixture_text("openrouter", "old", "error_401.json")
    )
    assert envelope.error.message == "Invalid API key provided"
    assert envelope.error.code == 401
    assert envelope.error.metadata == {"provider_name": None}


def test_error_envelope_tolerates_missing_fields() -> None:
    envelope = ErrorEnvelope.model_validate({"error": {}})
    assert envelope.error.message is None
    assert envelope.error.code is None
    assert envelope.error.metadata is None
