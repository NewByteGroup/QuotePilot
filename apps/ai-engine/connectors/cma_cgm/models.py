"""Pydantic models for the CMA CGM "API SpotOn" (instantquote v2.7.0).

Faithful to the OpenAPI spec published at api-portal.cma-cgm.com
(``pricing.commercial.instantquote.v2``, ``POST /spotOn/search``).

The search response is an array mixing ``SpotOffer`` and ``Message`` items
(messages carry the reason an offer could not be produced). ``SpotResultItem``
discriminates on the presence of ``code``, which is required on ``Message``
and never emitted by ``SpotOffer``.

Money and quantity fields are ``Decimal``; ``model_validate_json`` parses JSON
numbers without ever materializing a float (untrusted-response rule,
AGENTS.md). Datetimes are ``AwareDatetime``: the spec marks every date-time
example with ``Z``, so a naive upstream value fails validation loudly at the
connector boundary instead of being silently treated as UTC.
"""

from __future__ import annotations

from collections.abc import Mapping
from datetime import date
from decimal import Decimal
from enum import StrEnum
from typing import Annotated

from pydantic import (
    AwareDatetime,
    BaseModel,
    ConfigDict,
    Discriminator,
    Field,
    PlainSerializer,
    Tag,
    TypeAdapter,
)
from pydantic.alias_generators import to_camel


def _decimal_as_json_number(value: Decimal) -> int | float:
    """Serialize a Decimal back to a JSON ``number`` (pydantic emits str)."""
    if value == value.to_integral_value():
        return int(value)
    return float(value)


JsonNumber = Annotated[
    Decimal,
    PlainSerializer(
        _decimal_as_json_number,
        return_type=int | float,
        when_used="json",
    ),
]
"""Decimal that serializes as a JSON number, not a quoted decimal string."""


class CamelModel(BaseModel):
    """Base model: camelCase JSON aliases, immutable, unknown keys ignored."""

    model_config = ConfigDict(
        alias_generator=to_camel,
        populate_by_name=True,
        extra="ignore",
        frozen=True,
    )


class LocationCodificationType(StrEnum):
    """Referential used for ports and inland places on the search request."""

    CMA_CGM = "CMACGM"
    UNLOCODE = "UNLOCODE"


class OfferType(StrEnum):
    """Whether an offer's routing fully or partially matches the request."""

    EXACT_ROUTING = "ExactRouting"
    ALTERNATE_ROUTING = "AlternateRouting"


class RequestedEquipment(CamelModel):
    """Equipment group, count and estimated net weight per container (KGM)."""

    equipment_group_iso_code: str
    number_of_containers: int
    weight_per_container: JsonNumber = Field(ge=0, allow_inf_nan=False)


class SpotQuotationRequest(CamelModel):
    """Request body of ``POST /spotOn/search`` (``SpotQuotationRequest``).

    Only ``requested_equipments`` is required by the spec. ``commodity_code``
    has no client default: it must not be sent for empty-container requests,
    so the server-side default ("FAK") applies only when the caller sets it.
    """

    requested_equipments: Annotated[list[RequestedEquipment], Field(min_length=1)]
    departure_date: date | None = None
    location_codification_type: LocationCodificationType = (
        LocationCodificationType.UNLOCODE
    )
    port_of_loading: str | None = None
    port_of_discharge: str | None = None
    commodity_code: str | None = None
    spot_ddsm_conditions_only: bool | None = None
    non_operating_reefer: bool | None = None
    shipper_owned_container: bool | None = None
    empty_container: bool | None = None
    with_residue: bool | None = None
    hazardous: bool | None = None
    deciding_party: str | None = None
    place_of_origin: str | None = None
    place_of_origin_type: str | None = None
    place_of_delivery: str | None = None
    place_of_delivery_type: str | None = None
    shipping_company: str | None = None


class TokenResponse(CamelModel):
    """OAuth2 access-token reply from ``auth.cma-cgm.com/as/token.oauth2``."""

    access_token: str
    token_type: str | None = None
    expires_in: int = 3600


class Fault(CamelModel):
    """Error body on 4xx/5xx (spec schema ``Fault``; ``reason`` is required)."""

    reason: str
    code: str | None = None
    description: str | None = None


class Message(CamelModel):
    """Inline reason an offer could not be produced (e.g. ``INQ_SPN``)."""

    code: str
    description: str


class CodedName(CamelModel):
    """{code, name} lookup objects (Currency, Charge, Terminal, Service...)."""

    code: str | None = None
    name: str | None = None


class DecidingParty(CamelModel):
    """Business owner of the quotation on an offer's quote line."""

    business_deciding_party_code: str | None = None
    business_deciding_party_name: str | None = None
    business_deciding_party_country: str | None = None
    business_deciding_party_city: str | None = None


class LocationCodification(CamelModel):
    """One codification of a point (e.g. UN/LOCODE ``FRFOS``)."""

    codification_type: str | None = None
    codification: str | None = None


class Point(CamelModel):
    """A place in CMA CGM's referential plus its codifications."""

    name: str | None = None
    internal_code: str | None = None
    location_codifications: list[LocationCodification] | None = None


class LegFromTo(CamelModel):
    """Endpoint of a routing leg: place, port flag, country, terminal."""

    place: Point | None = None
    port: bool | None = None
    place_country: CodedName | None = None
    terminal: CodedName | None = None


class RoutingLeg(CamelModel):
    """One leg of an offer's routing with dates, service and vessel."""

    leg_number: int | None = None
    carbon_footprint: Decimal | None = None
    departure_date: AwareDatetime | None = None
    arrival_date: AwareDatetime | None = None
    port_cut_off_date: AwareDatetime | None = None
    vgm_cut_off_date: AwareDatetime | None = None
    booking_cut_off_date: AwareDatetime | None = None
    service: CodedName | None = None
    vessel_name: str | None = None
    feeder: bool | None = None
    voyage_reference: str | None = None
    leg_from: LegFromTo | None = None
    leg_to: LegFromTo | None = None


class Journey(CamelModel):
    """Origin/destination summary of a quote line's routing."""

    receipt_place: Point | None = None
    port_of_loading: Point | None = None
    port_of_discharge: Point | None = None
    delivery_place: Point | None = None
    # The spec spells this field all-lowercase, so to_camel would be wrong.
    routing_information: str | None = Field(default=None, alias="routinginformation")


class Cargo(CamelModel):
    """Special-cargo capability flags on a quote line."""

    hazardous: bool | None = None
    oversize: bool | None = None
    reefer: bool | None = None
    non_operating_reefer: bool | None = None
    controlled_atmosphere: bool | None = None
    shipper_owned_container: bool | None = None


class Commodity(CamelModel):
    """Commodity resolved by CMA CGM for a quote line."""

    commodity_group: bool | None = None
    code: str | None = None
    name: str | None = None


class ReeferInfo(CamelModel):
    """Reefer temperature (°C) and ventilation (Cbm/h) attributes."""

    temperature: Decimal | None = None
    ventilation: Decimal | None = None


class SpotEquipment(CamelModel):
    """Offered equipment group: availability, count and max net weight (TONS)."""

    equipment_group_iso_code: str | None = None
    available_rate: bool | None = None
    number_of_container_requested: int | None = None
    max_net_weight: Decimal | None = None
    reefer_attributes: ReeferInfo | None = None


class Surcharge(CamelModel):
    """A charge line: ``charge`` is required; amounts are ``Decimal``."""

    charge: CodedName
    charge_currency: CodedName | None = None
    amount: Decimal | None = None
    percentage_rate: str | None = None
    pivot_currency: CodedName | None = None
    pivot_amount: Decimal | None = None
    payment_method: CodedName | None = None


class SurchargePerBl(Surcharge):
    """Charge applicable once for the whole BL (spec ``allOf`` composition)."""

    surcharge: bool | None = None
    fixed_by_pricer: bool | None = None
    comment_on_charge_rule: str | None = None
    customer_comment: str | None = None


class SurchargePerCargo(SurchargePerBl):
    """Cargo-level charge; ``included_in_basic_freight`` marks bundle-in."""

    included_in_basic_freight: bool | None = None


class SurchargePerEquipmentType(CamelModel):
    """Per-equipment pricing block: all-in and basic ocean freight rates."""

    equipment_group_iso_code: str | None = None
    all_in_rate: Decimal | None = None
    basic_ocean_freight_rate: Decimal | None = None
    currency: CodedName | None = None
    matching_cargo_surcharges: list[SurchargePerCargo] | None = None


class SurchargeDefinedAsComment(CamelModel):
    """Charge expressed as free text only (no amount)."""

    charge: CodedName
    charge_defined_as_comment_txt: str | None = None


class CustomerGeneratedSurcharge(CamelModel):
    """Surcharge simulation block of a quote line."""

    simulation_date: date | None = None
    matching_surcharges_per_equipment_types: list[SurchargePerEquipmentType] | None = (
        None
    )
    matching_bl_surcharges: list[SurchargePerBl] | None = None
    matching_surcharges_defined_as_comments: list[SurchargeDefinedAsComment] | None = (
        None
    )


class SpotQuoteLine(CamelModel):
    """Commercial content of an offer: validity, equipment, routing, charges."""

    shipping_company: str | None = None
    validity_from: date | None = None
    validity_to: date | None = None
    deciding_parties: list[DecidingParty] | None = None
    export_haulage_mode: CodedName | None = None
    import_haulage_mode: CodedName | None = None
    export_mode_of_transport: CodedName | None = None
    import_mode_of_transport: CodedName | None = None
    special_cargo: Cargo | None = None
    spot_equipments: list[SpotEquipment] | None = None
    routing: Journey | None = None
    commodity: Commodity | None = None
    comment_to_customer: str | None = None
    surcharges: CustomerGeneratedSurcharge | None = None


class FreeDays(CamelModel):
    """Demurrage/detention free days and how they are counted."""

    number: int | None = None
    calculation_type: str | None = None


class ConditionsByEquipment(CamelModel):
    """Free-days condition for one equipment group."""

    equipment_group_iso_code: str | None = None
    free_days: FreeDays | None = None


class Tariff(CamelModel):
    """DDSM tariff reference (DET/DEM/MER/STO/MON) with its validity window."""

    code: str | None = None
    name: str | None = None
    tariff_origin: CodedName | None = None
    validity_date_from: date | None = None
    validity_date_to: date | None = None


class DdsmCondition(CamelModel):
    """One detention/demurrage/storage/monitoring condition of an offer."""

    movement_type: CodedName | None = None
    tariff: Tariff | None = None
    conditions_by_equipment: list[ConditionsByEquipment] | None = None
    remark: str | None = None


class Allocation(CamelModel):
    """Allocation status for the requested equipment groups."""

    equipment_group_iso_code: list[str] | None = None
    allocation: bool | None = None
    nb_of_containers_available: int | None = None
    partial_allocation: bool | None = None
    nb_of_available_teus: int | None = None


class PotentialFees(CamelModel):
    """Fee that may apply on booking amendment or cancellation."""

    fee_type: str | None = None


class SpotOffer(CamelModel):
    """A bookable SpotOn offer (spec schema ``SpotOffer``).

    ``offer_id`` may also carry sentinel strings (``no-offer``, ``Sold-out``,
    ``Quantities cannot be confirmed``, ``Offer cannot be created under FMC
    scope``) per the spec — callers must not assume it is always a reference.
    """

    offer_id: str | None = None
    offer_type: OfferType | None = None
    solution_number: int | None = None
    solution_description: str | None = None
    schedule_number: int | None = None
    transit_time: int | None = None
    total_carbon_footprint: Decimal | None = None
    departure_date: AwareDatetime | None = None
    arrival_date: AwareDatetime | None = None
    routing_legs: list[RoutingLeg] | None = None
    ddsm_conditions: list[DdsmCondition] | None = None
    allocation: list[Allocation] | None = None
    potential_fees: list[PotentialFees] | None = None
    quote_line: SpotQuoteLine | None = None


def _spot_result_tag(value: object) -> str:
    """Discriminate ``Message`` vs ``SpotOffer`` array items.

    ``code`` is required on ``Message`` and is never a ``SpotOffer`` field,
    so an object containing it is a message; everything else validates as an
    offer (whose fields are all optional in the spec).

    Residual: a spec-violating item missing ``code`` (e.g. a bare
    ``{"description": ...}``) parses as an all-None ``SpotOffer`` instead of
    failing — accepted as a spec-fidelity tradeoff, since ``SpotOffer`` has
    no required fields to anchor on.
    """
    if isinstance(value, Mapping) and "code" in value:
        return "message"
    return "offer"


SpotResultItem = Annotated[
    Annotated[Message, Tag("message")] | Annotated[SpotOffer, Tag("offer")],
    Discriminator(_spot_result_tag),
]
"""One item of the ``POST /spotOn/search`` array: a ``Message`` or ``SpotOffer``."""

SPOT_SEARCH_ADAPTER: TypeAdapter[list[SpotResultItem]] = TypeAdapter(
    list[SpotResultItem]
)
"""Validates the search response array straight from JSON (float-free)."""
