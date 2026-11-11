"""Model tests for the hapag_lloyd connector: schema fidelity and Decimal money."""

from __future__ import annotations

import json
from decimal import Decimal

import pytest
from pydantic import ValidationError

from connectors.hapag_lloyd.models import (
    OfferRequest,
    OfferResponse,
    ProductIdentifier,
)
from tests.helpers import fixture_json, fixture_text


def _response() -> OfferResponse:
    return OfferResponse.model_validate_json(
        fixture_text("hapag_lloyd", "old", "prices_response_201.json")
    )


def test_offer_request_round_trips_fixture() -> None:
    payload = fixture_text("hapag_lloyd", "old", "prices_request.json")
    request = OfferRequest.model_validate_json(payload)
    assert request.model_dump(
        mode="json", by_alias=True, exclude_none=True
    ) == json.loads(payload)


def test_offer_request_requires_mandatory_fields() -> None:
    body = fixture_json("hapag_lloyd", "old", "prices_request.json")
    del body["requestedEquipment"]
    with pytest.raises(ValidationError):
        OfferRequest.model_validate(body)


def test_offer_request_rejects_invalid_locode() -> None:
    body = fixture_json("hapag_lloyd", "old", "prices_request.json")
    body["placeOfReceipt"]["locode"] = "HAMBURG"
    with pytest.raises(ValidationError):
        OfferRequest.model_validate(body)


def test_offer_response_parses_fixture() -> None:
    response = _response()
    assert response.carrier_price_request_reference == (
        "3fa85f64-5717-4562-b3fc-2c963f66afa6"
    )
    assert response.offers is not None
    assert len(response.offers) == 2

    spot = response.offers[0]
    assert spot.product_identifier is ProductIdentifier.QUICK_QUOTES_SPOT
    assert spot.transit_time == 27
    assert spot.legs[0].vessel_imo_number == "9321483"
    assert spot.legs[0].carrier_service_name == "ASA"

    spot_equipment = spot.equipments[0]
    assert spot_equipment.rates[0].charge_type_code == "SEA"
    assert spot_equipment.rates[0].sea_freight_indicator is True
    assert spot_equipment.cancellation_fee_amount == Decimal("50.0")
    assert spot_equipment.value_added_services is not None
    assert spot_equipment.value_added_services[0].vas_code == "BI25"


def test_offer_response_accepts_offered_equipment_alias() -> None:
    # The published spec marks the field required as `offeredEquipment` while the
    # property is named `equipments`; both spellings must parse (spec typo).
    response = _response()
    assert response.offers is not None
    assert len(response.offers[1].equipments) == 1


def test_amounts_parse_as_decimal_never_float() -> None:
    response = _response()
    assert response.offers is not None
    for offer in response.offers:
        for equipment in offer.equipments:
            for rate in equipment.rates:
                assert isinstance(rate.amount, Decimal)
                assert not isinstance(rate.amount, float)
    spot_rate = response.offers[0].equipments[0].rates[0]
    assert spot_rate.amount == Decimal("1750.5")
    # 119.99 has no exact binary representation: only a parse path that never
    # materializes a float yields Decimal("119.99") here.
    doc_fee = response.offers[0].equipments[0].rates[2]
    assert doc_fee.amount == Decimal("119.99")


def test_offer_response_rejects_malformed_body() -> None:
    with pytest.raises(ValidationError):
        OfferResponse.model_validate_json(
            fixture_text("hapag_lloyd", "old", "prices_response_malformed.json")
        )


def test_offer_response_ignores_unknown_fields() -> None:
    body = fixture_json("hapag_lloyd", "old", "prices_response_201.json")
    body["offers"][0]["futureField"] = {"nested": True}
    response = OfferResponse.model_validate(body)
    assert response.offers is not None and len(response.offers) == 2
