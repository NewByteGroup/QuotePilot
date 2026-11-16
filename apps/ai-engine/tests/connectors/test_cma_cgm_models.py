"""Model tests for the cma_cgm connector: schema fidelity and Decimal money."""

from __future__ import annotations

import json
from decimal import Decimal

import pytest
from pydantic import ValidationError

from connectors.cma_cgm.models import (
    SPOT_SEARCH_ADAPTER,
    Message,
    OfferType,
    SpotOffer,
    SpotQuotationRequest,
)
from tests.helpers import fixture_json, fixture_text


def _request() -> SpotQuotationRequest:
    return SpotQuotationRequest.model_validate_json(
        fixture_text("cma_cgm", "old", "spoton_search_request.json")
    )


def _items() -> list[Message | SpotOffer]:
    return SPOT_SEARCH_ADAPTER.validate_json(
        fixture_text("cma_cgm", "old", "spoton_search_response_200.json")
    )


def test_request_round_trips_fixture() -> None:
    payload = fixture_text("cma_cgm", "old", "spoton_search_request.json")
    request = SpotQuotationRequest.model_validate_json(payload)
    assert request.model_dump(
        mode="json", by_alias=True, exclude_none=True
    ) == json.loads(payload)


def test_request_requires_requested_equipments() -> None:
    body = fixture_json("cma_cgm", "old", "spoton_search_request.json")
    del body["requestedEquipments"]
    with pytest.raises(ValidationError):
        SpotQuotationRequest.model_validate(body)


def test_request_defaults_location_codification() -> None:
    request = SpotQuotationRequest.model_validate(
        {
            "requestedEquipments": [
                {
                    "equipmentGroupIsoCode": "40GP",
                    "numberOfContainers": 1,
                    "weightPerContainer": 18000,
                }
            ]
        }
    )
    assert request.location_codification_type == "UNLOCODE"


def test_search_response_discriminates_offers_and_messages() -> None:
    items = _items()
    assert len(items) == 3
    assert isinstance(items[0], SpotOffer)
    assert isinstance(items[1], SpotOffer)
    # A bare {code, description} item is a no-offer reason, not an offer.
    message = items[2]
    assert isinstance(message, Message)
    assert message.code == "INQ_TRN"


def test_search_offer_parses_fixture() -> None:
    items = _items()
    offer = items[0]
    assert isinstance(offer, SpotOffer)
    assert offer.offer_type is OfferType.EXACT_ROUTING
    assert offer.transit_time == 28
    assert offer.departure_date is not None
    assert offer.departure_date.year == 2026
    leg = (offer.routing_legs or [])[0]
    assert leg.vessel_name == "CMA CGM PUGET"
    assert leg.leg_from is not None
    assert leg.leg_from.terminal is not None
    assert leg.leg_from.terminal.code == "BTP"
    quote_line = offer.quote_line
    assert quote_line is not None
    assert quote_line.commodity is not None
    assert quote_line.commodity.code == "FAK"
    assert quote_line.routing is not None
    # The spec spells this field all-lowercase; it must still parse.
    assert quote_line.routing.routing_information == "direct service"


def test_message_only_response_parses() -> None:
    items = SPOT_SEARCH_ADAPTER.validate_json(
        fixture_text("cma_cgm", "old", "spoton_search_response_message_only.json")
    )
    messages = [item for item in items if isinstance(item, Message)]
    assert len(messages) == len(items)
    assert [m.code for m in messages] == ["INQ_SPN", "INQ_BLK"]


def test_money_fields_parse_as_decimal_never_float() -> None:
    items = _items()
    offer = items[0]
    assert isinstance(offer, SpotOffer)
    quote_line = offer.quote_line
    assert quote_line is not None
    assert quote_line.surcharges is not None
    per_equipment = quote_line.surcharges.matching_surcharges_per_equipment_types
    assert per_equipment is not None
    equipment_rates = per_equipment[0]
    assert isinstance(equipment_rates.all_in_rate, Decimal)
    assert not isinstance(equipment_rates.all_in_rate, float)
    assert equipment_rates.all_in_rate == Decimal("2350")
    cargo = equipment_rates.matching_cargo_surcharges or []
    # 19.99 has no exact binary representation: only a parse path that never
    # materializes a float yields Decimal("19.99") here.
    assert cargo[0].amount == Decimal("19.99")
    bl = quote_line.surcharges.matching_bl_surcharges or []
    assert bl[0].amount == Decimal("75.5")


def test_weight_per_container_serializes_as_json_number() -> None:
    body = fixture_json("cma_cgm", "old", "spoton_search_request.json")
    body["requestedEquipments"][0]["weightPerContainer"] = 18000.5
    request = SpotQuotationRequest.model_validate(body)
    dumped = request.model_dump(mode="json", by_alias=True, exclude_none=True)
    assert dumped["requestedEquipments"][0]["weightPerContainer"] == 18000.5


def test_search_response_rejects_malformed_item() -> None:
    with pytest.raises(ValidationError):
        SPOT_SEARCH_ADAPTER.validate_json(
            fixture_text("cma_cgm", "old", "spoton_search_response_malformed.json")
        )


def test_search_response_ignores_unknown_fields() -> None:
    items = fixture_json("cma_cgm", "old", "spoton_search_response_200.json")
    items[0]["futureField"] = {"nested": True}
    parsed = SPOT_SEARCH_ADAPTER.validate_python(items)
    assert len(parsed) == 3
