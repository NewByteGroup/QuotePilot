"""Model tests for the bcb_ptax connector: strict schema on untrusted JSON."""

from __future__ import annotations

from datetime import UTC, datetime
from decimal import Decimal
from typing import Any

import pytest
from pydantic import ValidationError

from connectors.bcb_ptax.models import PtaxRate, PtaxResponse


def _payload(**overrides: Any) -> dict[str, Any]:
    payload: dict[str, Any] = {
        "cotacaoCompra": Decimal("5.65120"),
        "cotacaoVenda": Decimal("5.65180"),
        "dataHoraCotacao": "2026-11-13 13:03:27.375047",
    }
    payload.update(overrides)
    return payload


def test_ptax_rate_parses_canonical_payload() -> None:
    rate = PtaxRate.model_validate(_payload())

    assert rate.buy_rate == Decimal("5.65120")
    assert rate.sell_rate == Decimal("5.65180")
    assert rate.quoted_at == datetime(2026, 11, 13, 13, 3, 27, 375047)
    assert rate.quoted_at.tzinfo is None


def test_ptax_rate_populates_by_field_name() -> None:
    rate = PtaxRate.model_validate(
        {
            "buy_rate": Decimal("5.0"),
            "sell_rate": Decimal("5.1"),
            "quoted_at": "2026-11-13 10:00:00",
        }
    )

    assert rate.buy_rate == Decimal("5.0")


@pytest.mark.parametrize(
    "value",
    [
        5.65,
        "5.65",
        True,
        Decimal("0"),
        Decimal("-1.5"),
        Decimal("nan"),
        Decimal("Infinity"),
        None,
    ],
)
def test_ptax_rate_rejects_non_strict_amounts(value: Any) -> None:
    with pytest.raises(ValidationError):
        PtaxRate.model_validate(_payload(cotacaoCompra=value))


def test_ptax_rate_rejects_sell_below_buy() -> None:
    with pytest.raises(ValidationError):
        PtaxRate.model_validate(
            _payload(cotacaoCompra=Decimal("5.90"), cotacaoVenda=Decimal("5.80"))
        )


@pytest.mark.parametrize(
    "value",
    [
        "2026-11-13",
        1731456000,
        1731456000.5,
        "2026-11-13 13:03:27+00:00",
        "2026-11-13T13:03:27Z",
        "2026-13-99 10:00:00",
        "not a date",
        datetime(2026, 11, 13, 13, 3, 27),
        datetime(2026, 11, 13, 13, 3, 27, tzinfo=UTC),
        None,
    ],
)
def test_ptax_rate_rejects_non_plain_timestamp(value: Any) -> None:
    with pytest.raises(ValidationError):
        PtaxRate.model_validate(_payload(dataHoraCotacao=value))


def test_ptax_rate_ignores_unknown_fields() -> None:
    rate = PtaxRate.model_validate(_payload(unexpected="ok"))

    assert rate.sell_rate == Decimal("5.65180")


def test_ptax_response_requires_value_and_caps_at_one_row() -> None:
    payload = _payload()

    parsed = PtaxResponse.model_validate({"value": [payload], "other": 1})
    assert len(parsed.value) == 1
    assert parsed.value[0].buy_rate == Decimal("5.65120")

    assert PtaxResponse.model_validate({"value": []}).value == []

    with pytest.raises(ValidationError):
        PtaxResponse.model_validate({})
    with pytest.raises(ValidationError):
        PtaxResponse.model_validate({"value": None})
    with pytest.raises(ValidationError):
        PtaxResponse.model_validate({"value": [payload, payload]})
