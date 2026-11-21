"""Behavior tests for the hand-written JSON Schemas under schemas/.

These prove the contract accepts the shapes the pipelines are specified to
emit (RFC-001 section 4/5, RFC-004 section 3) — e.g. an AIR tariff row priced
per kilogram with equipment_or_unit "KG", and the optional declared
freight/insurance inputs to the import CIF base. They run on the system
python3 (same interpreter the Makefile `check` target uses) plus the
jsonschema package: python3 tests/test_contract_schemas.py -v
"""

import json
import unittest
from pathlib import Path

import jsonschema
from referencing import Registry, Resource
from referencing.jsonschema import DRAFT202012

SCHEMAS_DIR = Path(__file__).resolve().parents[1] / "schemas"


def _load(name: str) -> dict:
    return json.loads((SCHEMAS_DIR / name).read_text(encoding="utf-8"))


_COMMON = _load("common.schema.json")
_EXTRACTION = _load("extraction_result.schema.json")
_TARIFFS = _load("private_tariffs.schema.json")

# Relative $refs ("./common.schema.json#/$defs/...") resolve against each
# schema's $id, so the registry maps every contract's canonical URI.
_REGISTRY = Registry().with_resources(
    [
        (schema["$id"], Resource.from_contents(schema, DRAFT202012))
        for schema in (_COMMON, _EXTRACTION, _TARIFFS)
    ]
)


def _validator(schema: dict) -> jsonschema.Draft202012Validator:
    return jsonschema.Draft202012Validator(schema, registry=_REGISTRY)


def _field_value(value: str, quote: str = "as written in the email") -> dict:
    """A valid FieldValue envelope (common.schema.json $defs/FieldValue)."""
    return {
        "value": value,
        "evidence": {"source": "EMAIL_BODY", "exact_quote": quote},
        "inferred": False,
        "confidence": 0.9,
        "reasons": [],
    }


def _extraction(fields: dict) -> dict:
    """A minimal valid ExtractionResult carrying the given fields object."""
    return {
        "contract_version": "1.0.0",
        "classification": {
            "label": "RFQ",
            "probabilities": {"RFQ": 0.9, "TARIFF_UPDATE": 0.05, "OTHER": 0.05},
        },
        "fields": fields,
        "reference_dataset_version": "2026-11-10",
        "model_info": {
            "extractor_model": "anthropic/claude-haiku-5.5",
            "auditor_model": "perplexity/pplx-decider-v1.2",
        },
    }


def _air_tariff_row(equipment_or_unit: str) -> dict:
    """A minimal valid priced PrivateTariff row (RFC-002 step 3 wire shape)."""
    return {
        "carrier_or_agent": "HAPAG",
        "modal": "AIR",
        "charge_category": "AIR_FREIGHT",
        "charge_code": "AIRF",
        "origin_locode": "DEHAM",
        "dest_locode": "BRGRU",
        "equipment_or_unit": equipment_or_unit,
        "is_removed": False,
        "currency": "USD",
        "net_amount": "4.20",
        "min_amount": "100.00",
        "valid_from": "2026-01-01",
        "valid_until": "2026-12-31",
        "search_description": "Air freight DEHAM-BRGRU per chargeable kg",
        "source": {
            "schema_version": 1,
            "kind": "RESEND_INBOUND",
            "ref": "resend_msg_123",
            "fetched_at": "2026-11-16T12:00:00Z",
            "content_hash": "9f86d081884c7d659a2feaa0c55ad015",
        },
    }


class EquipmentOrUnitTests(unittest.TestCase):
    """RFC-001 section 5: air freight is priced per chargeable kilogram, so
    KG must be a legal EquipmentOrUnit value on AIR tariff rows."""

    def setUp(self) -> None:
        self.tariffs = _validator(_TARIFFS)
        self.extraction = _validator(_EXTRACTION)

    def test_kg_validates_on_air_tariff_row(self) -> None:
        self.tariffs.validate(_air_tariff_row("KG"))

    def test_kg_validates_on_extraction_equipment_field(self) -> None:
        doc = _extraction({"equipment_or_unit": _field_value("KG", "per kg")})
        self.extraction.validate(doc)

    def test_unknown_unit_still_rejected_on_tariff_row(self) -> None:
        with self.assertRaises(jsonschema.ValidationError):
            self.tariffs.validate(_air_tariff_row("AWB"))


class DeclaredFreightInsuranceTests(unittest.TestCase):
    """RFC-001 section 4: under the CIF family the seller-paid freight and
    the declared insurance enter the import CIF base as document numbers,
    never as charge lines — extraction carries them as optional fields."""

    def setUp(self) -> None:
        self.validator = _validator(_EXTRACTION)

    def test_declared_fields_validate_as_decimal_strings(self) -> None:
        doc = _extraction(
            {
                "declared_freight_amount": _field_value("1200.50", "freight USD 1200.50"),
                "declared_freight_currency": _field_value("USD", "freight USD 1200.50"),
                "declared_insurance_amount": _field_value("0.00", "no insurance"),
                "declared_insurance_currency": _field_value("USD", "USD"),
            }
        )
        self.validator.validate(doc)

    def test_declared_fields_are_not_required(self) -> None:
        doc = _extraction(
            {
                "incoterm": _field_value("CIF", "CIF Santos"),
                "cargo_value_amount": _field_value("45000", "value 45000"),
                "cargo_value_currency": _field_value("USD", "USD"),
            }
        )
        self.validator.validate(doc)

    def test_declared_field_still_needs_a_valid_envelope(self) -> None:
        doc = _extraction(
            {
                "declared_freight_amount": {
                    "value": "1200.50",
                    # missing evidence/inferred/confidence/reasons
                }
            }
        )
        with self.assertRaises(jsonschema.ValidationError):
            self.validator.validate(doc)

    def test_declared_amount_rejects_non_string_value(self) -> None:
        """Money is a decimal string, never a JSON number (ADR-008)."""
        bad = _field_value(1200.50)  # type: ignore[arg-type]
        doc = _extraction({"declared_freight_amount": bad})
        with self.assertRaises(jsonschema.ValidationError):
            self.validator.validate(doc)


if __name__ == "__main__":
    unittest.main()
