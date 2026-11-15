"""Pydantic models for the BCB PTAX OData API (CotacaoDolarDia endpoint).

Faithful to the observed public response: ``value`` is a list with at most
one ``{cotacaoCompra, cotacaoVenda, dataHoraCotacao}`` record, BRL per 1 USD.
Amounts are ``Decimal`` parsed straight from JSON lexemes — never floats —
and must be finite and positive with sell >= buy. ``dataHoraCotacao`` is a
naive BCB local timestamp; timezone-aware or non-string inputs are rejected.
"""

from __future__ import annotations

import re
from decimal import Decimal
from typing import Annotated, Self

from pydantic import (
    BaseModel,
    BeforeValidator,
    ConfigDict,
    Field,
    NaiveDatetime,
    model_validator,
)

_DATETIME_LIKE = re.compile(r"^\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}")

StrictPositiveDecimal = Annotated[
    Decimal, Field(strict=True, gt=0, allow_inf_nan=False)
]


def _require_datetime_string(value: object) -> str:
    if not isinstance(value, str) or not _DATETIME_LIKE.match(value):
        raise ValueError(
            "expected a naive 'YYYY-MM-DD HH:MM:SS' timestamp string, "
            f"got {type(value).__name__}"
        )
    return value


PlainTextDatetime = Annotated[NaiveDatetime, BeforeValidator(_require_datetime_string)]


class PtaxRate(BaseModel):
    """One ``CotacaoDolarDia`` record: buy/sell USD-BRL rates and timestamp."""

    model_config = ConfigDict(populate_by_name=True, extra="ignore", frozen=True)

    buy_rate: StrictPositiveDecimal = Field(alias="cotacaoCompra")
    sell_rate: StrictPositiveDecimal = Field(alias="cotacaoVenda")
    quoted_at: PlainTextDatetime = Field(alias="dataHoraCotacao")

    @model_validator(mode="after")
    def _sell_not_below_buy(self) -> Self:
        if self.sell_rate < self.buy_rate:
            raise ValueError(
                "cotacaoVenda must be >= cotacaoCompra, got "
                f"{self.sell_rate} < {self.buy_rate}"
            )
        return self


class PtaxResponse(BaseModel):
    """The OData envelope; ``CotacaoDolarDia`` yields zero or one record."""

    model_config = ConfigDict(extra="ignore")

    value: list[PtaxRate] = Field(max_length=1)
