"""Pydantic models for the Hapag-Lloyd QBE "External Prices" API.

Faithful to the published OpenAPI spec ("Prices API", version 2.1.3,
``api-portal.hlag.com`` → Quotation/Booking Engine → External Prices).
Two typos in the spec are tolerated on input: ``offeredEquipment`` vs
``equipments`` (required list vs property name) and the PascalCase address
fields ``StreetNumber``/``Floor``/``PostCode``.

Enums are strict on purpose: an unknown value from upstream fails validation
(HapagSchemaError at the client boundary) rather than silently misclassifying a
charge, per the untrusted-response rule in AGENTS.md.

Money fields are ``Decimal``; ``model_validate_json`` parses JSON numbers
without ever materializing a float.
"""

from __future__ import annotations

from datetime import date
from decimal import Decimal
from enum import StrEnum
from typing import Annotated

from pydantic import (
    AliasChoices,
    AwareDatetime,
    BaseModel,
    ConfigDict,
    Field,
    StringConstraints,
)
from pydantic.alias_generators import to_camel

Locode = Annotated[
    str,
    StringConstraints(min_length=5, max_length=5, pattern=r"^[a-zA-Z0-9]{5}$"),
]
UnNumber = Annotated[str, StringConstraints(pattern=r"^(\d{4})?$")]


class CamelModel(BaseModel):
    """Base model: camelCase JSON aliases, immutable, unknown keys ignored."""

    model_config = ConfigDict(
        alias_generator=to_camel,
        populate_by_name=True,
        extra="ignore",
        frozen=True,
    )


class ServiceType(StrEnum):
    """CY - Container Yard (incl. rail ramp); SD - Store door."""

    CY = "CY"
    SD = "SD"


class ProductIdentifier(StrEnum):
    """Hapag-Lloyd rate products offered by the Prices API."""

    QUICK_QUOTES = "QUICK_QUOTES"
    QUICK_QUOTES_SPOT = "QUICK_QUOTES_SPOT"


class WeightUnit(StrEnum):
    """Cargo gross weight unit for the offer request."""

    KG = "KG"
    LBS = "LBS"


class RequestModeOfTransport(StrEnum):
    """Mode of transport allowed on the offer request (no combined classes)."""

    VESSEL = "VESSEL"
    TRUCK = "TRUCK"
    RAIL = "RAIL"
    WATERWAY = "WATERWAY"
    COMBINED_RAIL = "COMBINED_RAIL"
    COMBINED_WATERWAY = "COMBINED_WATERWAY"


class ModeOfTransport(StrEnum):
    """Mode of transport on response legs and rates (includes group classes)."""

    VESSEL = "VESSEL"
    TRUCK = "TRUCK"
    RAIL = "RAIL"
    WATERWAY = "WATERWAY"
    COMBINED_RAIL = "COMBINED_RAIL"
    COMBINED_WATERWAY = "COMBINED_WATERWAY"
    INLAND_TR_CR_CW = "INLAND_TR_CR_CW"
    OVERLAND_TR_CR = "OVERLAND_TR_CR"


class MotDirectionIndicator(StrEnum):
    """Whether the transport section is PRE, POST or the LOCAL main leg."""

    LOCAL = "LOCAL"
    PRE = "PRE"
    POST = "POST"


class LocationProposal(StrEnum):
    """Where a charge is normally applied along the route."""

    BASE_PORT_FROM = "BASE_PORT_FROM"
    BASE_PORT_TO = "BASE_PORT_TO"
    MAIN_CARRIAGE = "MAIN_CARRIAGE"
    ON_CARRIAGE = "ON_CARRIAGE"
    PC_CARRIAGE = "PC_CARRIAGE"


class RateBaseType(StrEnum):
    """Whether the rate applies per container, cargo item or document."""

    CONTAINER = "CONTAINER"
    CARGO_ITEM = "CARGO_ITEM"
    DOCUMENT = "DOCUMENT"


class UnitOfMeasure(StrEnum):
    """Unit of measurement a rate amount is expressed in."""

    BL = "BL"
    CUBIC_METER = "CUBIC_METER"
    CONTAINER = "CONTAINER"
    DAY = "DAY"
    FREIGHT_FREE = "FREIGHT_FREE"
    FREIGHT_TONS = "FREIGHT_TONS"
    HOUR = "HOUR"
    KILOGRAM = "KILOGRAM"
    POUND_US = "POUND_US"
    LITRE = "LITRE"
    PERCENT = "PERCENT"
    SHORT_TON = "SHORT_TON"
    METRIC_TON = "METRIC_TON"
    TON = "TON"
    UNIT = "UNIT"


class PcIndicator(StrEnum):
    """Freight payment term on a rate line."""

    PREPAID = "PREPAID"
    COLLECT = "COLLECT"


class ReeferOperationMode(StrEnum):
    """Reefer mode a rate applies to."""

    OPERATIVE = "OPERATIVE"
    NON_OPERATIVE = "NON_OPERATIVE"
    NOT_RELEVANT = "NOT_RELEVANT"


class GraduatedCalculationRule(StrEnum):
    """Calculation rule number for graduated rates."""

    ONE = "ONE"
    TWO = "TWO"
    THREE = "THREE"
    FOUR = "FOUR"
    FIVE = "FIVE"
    SIX = "SIX"
    SEVEN = "SEVEN"


class GraduatedUnitOfMeasure(StrEnum):
    """Unit of measure for graduated rate ranges."""

    CBM = "CBM"
    CTR = "CTR"
    KMT = "KMT"
    SMI = "SMI"
    TON = "TON"


class GraduatedUnitOfMeasureType(StrEnum):
    """Dimension a graduated rate range measures."""

    DISTANCE = "DISTANCE"
    QUANTITY = "QUANTITY"
    VOLUME = "VOLUME"
    WEIGHT = "WEIGHT"


class GraduatedLowerUpperUom(StrEnum):
    """Alternative unit of measure for graduated bounds."""

    CUBIC_METER = "CUBIC_METER"
    CONTAINER = "CONTAINER"
    KMT = "KMT"
    SMI = "SMI"
    TON = "TON"


class FacilityCodeListProvider(StrEnum):
    """Registry that issued a facility code."""

    BIC = "BIC"
    SMDG = "SMDG"


class ShippingInstructionReceival(StrEnum):
    """How the shipping instruction is received for a rate."""

    NON_ELECTRONICALLY = "NON_ELECTRONICALLY"
    ELECTRONICALLY = "ELECTRONICALLY"


class ErrorReason(StrEnum):
    """Why an offer or equipment could not be priced (spec enum, verbatim)."""

    MISSING_INFORMATION = "MISSING_INFORMATION"
    RESTRICTIONS = "RESTRICTIONS"
    CAN_NOT_BE_OFFERED = "CAN_NOT_BE_OFFERED"
    USER_UNKNOWN = "USER_UNKNOWN"
    MISSING_PERMISSIONS = "MISSING_PERMISSIONS"
    TECHNICAL_ERROR = "TECHNICAL_ERROR"
    JONES_ACT = "JONES_ACT"
    OPTIMIZED_ROUTING = "OPTIMIZED_ROUTING"
    INLAND_UNAVAILABLE = "INLAND_UNAVAILABLE"
    CAPACITY = "CAPACITY"
    INLAND_RESTRICTIONS = "INLAND_RESTRICTIONS"
    DANGEROUS_GOODS_RESTRICTION = "DANGEROUS_GOODS_RESTRICTION"
    COMMODITY_PROHIBITED_FOR_REEFER_AND_SPOT = (
        "COMMODITY_PROHIBITED_FOR_REEFER_AND_SPOT"
    )
    IN_GAUGE_PROHIBITED = "IN_GAUGE_PROHIBITED"
    QQS_CAN_NOT_BE_OFFERED_FOR_LIGHT_USER = "QQS_CAN_NOT_BE_OFFERED_FOR_LIGHT_USER"
    SOC_PROHIBITED_FOR_SPOT = "SOC_PROHIBITED_FOR_SPOT"
    NO_ROUTE_FOR_EQUIPMENT = "NO_ROUTE_FOR_EQUIPMENT"
    DEPARTURE_DATE_TO_EARLY = "DEPARTURE_DATE_TO_EARLY"


class Location(CamelModel):
    """UN/LOCODE wrapper used on the offer request."""

    locode: Locode


class RequestedEquipment(CamelModel):
    """Equipment size/type and unit count requested for the cargo."""

    requested_equipment_size_type: Annotated[str, StringConstraints(max_length=4)]
    requested_equipment_units: Annotated[int, Field(ge=1, le=20)]
    is_non_operating_reefer: bool
    shippers_owned_container: bool


class RequestCommodity(CamelModel):
    """Commodity details for the offer request; only weight is mandatory."""

    cargo_gross_weight: int
    cargo_gross_weight_unit: WeightUnit
    commodity_type_group: str | None = None
    commodity_type_group_number: int | None = None
    is_hazardous: bool | None = None
    imo_class: str | None = None
    un_number: UnNumber | None = None


class OfferRequest(CamelModel):
    """Request body of ``POST /prices`` (spec schema ``OfferRequest``)."""

    place_of_receipt: Location
    place_of_delivery: Location
    requested_equipment: RequestedEquipment
    commodity: RequestCommodity
    receipt_type_at_origin: ServiceType
    delivery_type_at_destination: ServiceType
    earliest_departure_date: date
    product_identifiers: Annotated[list[ProductIdentifier], Field(min_length=1)]
    customer_identifier: str
    port_of_loading: Location | None = None
    port_of_discharge: Location | None = None
    export_mot: RequestModeOfTransport | None = None
    import_mot: RequestModeOfTransport | None = None
    latest_arrival_date: date | None = None


class DangerousGoods(CamelModel):
    """Hazmat details echoed on an offered equipment."""

    is_hazardous: bool
    imo_class: str
    un_number: str


class OfferCommodity(CamelModel):
    """Commodity resolved by Hapag-Lloyd for an offered equipment."""

    commodity_type_group: str
    commodity_type_group_number: int
    name: str | None = None
    description: str | None = None


class BaseRate(CamelModel):
    """Base charge a graduated/VAS rate derives from."""

    charge_code: str
    charge_text: str


class Rate(CamelModel):
    """A single charge line of an offer (spec schema ``Rate``).

    ``currency`` is required but may be null when ``unit_of_measure`` is
    ``PERCENT``; ``amount`` is a Decimal parsed straight from the JSON literal.
    """

    charge_type_code: str
    currency: str | None
    unit_of_measure: UnitOfMeasure
    amount: Decimal
    subject_to_gri: bool
    included: bool
    chargeable: bool
    exclusive_rate: bool
    location_proposal: LocationProposal
    mode_of_transport: ModeOfTransport
    mot_direction_indicator: MotDirectionIndicator
    rate_base_type: RateBaseType
    sea_freight_indicator: bool
    charge_type_short_description: str | None = None
    graduated_low_value: Decimal | None = None
    graduated_high_value: Decimal | None = None
    graduated_calculation_rule: GraduatedCalculationRule | None = None
    graduated_unit_of_measure: GraduatedUnitOfMeasure | None = None
    graduated_unit_of_measure_type: GraduatedUnitOfMeasureType | None = None
    carrier_service_name: (
        Annotated[str, StringConstraints(min_length=3, max_length=3)] | None
    ) = None
    reefer_operation_mode: ReeferOperationMode | None = None
    base_charges: list[BaseRate] | None = None
    charge_type_class: int | None = None
    charge_type_sequence_in_class: int | None = None
    graduated_lower_upper_uom: GraduatedLowerUpperUom | None = None
    dangerous_goods_status: str | None = None
    invoice_item_group: Decimal | None = None
    pc_indicator: PcIndicator | None = None
    base_amount: Decimal | None = None
    shipping_instruction_receival: ShippingInstructionReceival | None = None


class Address(CamelModel):
    """Postal address; spec uses PascalCase on three fields (tolerated)."""

    name: str | None = None
    street: str | None = None
    street_number: str | None = Field(
        default=None, validation_alias=AliasChoices("streetNumber", "StreetNumber")
    )
    floor: str | None = Field(
        default=None, validation_alias=AliasChoices("floor", "Floor")
    )
    post_code: str | None = Field(
        default=None, validation_alias=AliasChoices("postCode", "PostCode")
    )
    city: str | None = None
    state_region: str | None = None
    country: str | None = None


class LegLocation(CamelModel):
    """Endpoint of a route leg: UN/LOCODE, facility or free address."""

    location_name: str | None = None
    un_location_code: Locode | None = None
    facility_code: str | None = None
    facility_code_list_provider: FacilityCodeListProvider | None = None
    address: Address | None = None


class Leg(CamelModel):
    """A route section; vessel fields are set on Ocean legs only."""

    departure_location: LegLocation
    departure_date_time: AwareDatetime
    arrival_location: LegLocation
    arrival_date_time: AwareDatetime
    mode_of_transport: ModeOfTransport | None = None
    vessel_name: str | None = None
    vessel_year_built: int | None = None
    vessel_call_sign: str | None = None
    vessel_flag: Annotated[str, StringConstraints(max_length=2)] | None = None
    vessel_imo_number: Annotated[str, StringConstraints(max_length=7)] | None = None
    carrier_service_name: (
        Annotated[str, StringConstraints(min_length=3, max_length=3)] | None
    ) = None
    schedule_voyage_number: Annotated[str, StringConstraints(max_length=50)] | None = (
        None
    )


class ValueAddedService(CamelModel):
    """Optional VAS (e.g. Ship Green, Live Position) selectable on an offer."""

    vas_id: str | None = None
    product_configurator_vas_id: str | None = None
    vas_group: str | None = None
    vas_code: Annotated[str, StringConstraints(max_length=4)] | None = None
    name: str | None = None
    description: str | None = None
    vas_rates: list[Rate] | None = None
    included: bool | None = None
    freetime_days: int | None = None


class OfferEquipment(CamelModel):
    """An offered equipment with its rates, fees and selectable services."""

    requested_equipment: RequestedEquipment
    rates: list[Rate]
    dangerous_goods: DangerousGoods | None = None
    commodity: OfferCommodity | None = None
    cancellation_fee_amount: Decimal | None = None
    cancellation_fee_currency: str | None = None
    cancellation_fee_uo_m: UnitOfMeasure | None = None
    no_show_fee_amount: Decimal | None = None
    no_show_fee_currency: str | None = None
    no_show_fee_uo_m: UnitOfMeasure | None = None
    compensation_fee_amount: Decimal | None = None
    compensation_fee_currency: str | None = None
    compensation_fee_uo_m: UnitOfMeasure | None = None
    value_added_services: list[ValueAddedService] | None = None
    error_reason: ErrorReason | None = None


class Offer(CamelModel):
    """A bookable offer: routing, schedule, equipment rates and validity."""

    carrier_offer_request_reference: str
    product_identifier: ProductIdentifier
    place_of_receipt: Locode
    place_of_receipt_date: AwareDatetime
    place_of_delivery: Locode
    place_of_delivery_date_time: AwareDatetime
    port_of_loading: Locode
    port_of_loading_date_time: AwareDatetime
    port_of_discharge: Locode
    port_of_discharge_date_time: AwareDatetime
    receipt_type_at_origin: ServiceType
    delivery_type_at_destination: ServiceType
    is_origin_location_exchanged_with_twin_city: bool | None = None
    is_destination_location_exchanged_with_twin_city: bool | None = None
    transit_time: int
    cut_off_date_time: AwareDatetime | None = None
    documentation_closure_date_time: AwareDatetime | None = None
    verified_gross_mass_date_time: AwareDatetime | None = None
    offer_valid_to: AwareDatetime
    terms_and_conditions: list[str]
    legs: list[Leg]
    equipments: list[OfferEquipment] = Field(
        validation_alias=AliasChoices("equipments", "offeredEquipment")
    )
    error_reason: ErrorReason | None = None
    potential_quotation_valid_from: date
    potential_quotation_valid_to: date
    potential_quotation_expiry_date: date


class OfferResponse(CamelModel):
    """Body of a ``201`` reply to ``POST /prices`` (``OfferResponse``)."""

    carrier_price_request_reference: str | None = None
    request: OfferRequest | None = None
    offers: list[Offer] | None = None
    overarching_error_reason: ErrorReason | None = None


class Problem(CamelModel):
    """RFC 7807 problem details returned on 4xx/5xx (spec schema ``Problem``)."""

    status: int
    title: str
    type: str | None = None
    detail: str | None = None
    global_transaction_id: str | None = None
