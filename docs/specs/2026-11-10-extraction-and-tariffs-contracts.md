# Tech spec: `ExtractionResult` and `PrivateTariff` wire contracts (JSON Schema)

- **Status:** Proposed — draft for Pedro's review; **do not merge into `packages/contracts` without his approval** (AGENTS.md)
- **Date:** 2026-11-10
- **Author:** Devin (for Pedro)
- **Governs:** `packages/contracts/schemas/{common,extraction_result,private_tariffs}.schema.json`
- **Inputs:** RFC-004 §3–6 (contract, checks, status model, calibration), ADR-006 §§Stage 3/3/model matrix/env vars (field lists, model pins), data-model v1.3 spec §6 (canonical Postgres enums), RFC-001 §3/§9 (pricing consumption, tariff natural key), RFC-002 step 3 (tariff versioning), Marcelo's 2026-11-10 decision: **tariffs are ingested from the agent/carrier website (Firecrawl), not CSV/XLSX upload**

## 1. Purpose and scope

Draft the two JSON Schemas `core-api` needs from `ai-engine` before Pedro's
contracts task (codegen Go/Pydantic/TS) can start, and before Marcelo's four
IA tasks (Classifier 11-12, Extractor 11-16, Auditor 11-19, Tariff parser 11-20)
can code against real types.

In scope: schema files, shared `$defs`, the provenance extension for
web-sourced tariffs, and the proposed answers to RFC-004's open questions.
Out of scope: codegen configuration and generated code (Pedro's task), the
reference dataset build (`packages/` versioned UN/LOCODE+NCM+Incoterms,
RFC-004 §4, owner Pedro), pipelines and agents themselves.

## 2. File layout

```
packages/contracts/schemas/
  common.schema.json              # $defs: DecimalString, Locode, Iso4217Currency,
                                  # FreightModal, Incoterm, EquipmentOrUnit,
                                  # EvidenceSource, ReasonCode, FieldStatus,
                                  # FieldValue envelope, IsoDate, Embedding(1024)
  extraction_result.schema.json   # Stage-2 classification + per-field envelopes
                                  # + search_description/embedding + model_info
  private_tariffs.schema.json     # one append-only tariff row (+ proposed
                                  # `source` provenance object)
```

JSON Schema draft 2020-12 (these are JSON Schema files, not OpenAPI documents;
the "3.0.3 not 3.1" rule applies to OpenAPI). One shared `common.schema.json`
keeps the enums in a single place so the two contracts cannot drift.

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | **Every `value` is a `string`**, enums/dates included; money and quantities are `DecimalString` (never JSON numbers) | Uniform envelope type → clean Go/Pydantic codegen; satisfies RFC-004 "decimal strings, never JSON floats". Per-field value vocab documented in each property's `description`. |
| 2 | **`status` is in the schema but never set by `ai-engine`** | Status is computed by `core-api` (checks + `τ_field`, RFC-004 §5) yet web needs it on the persisted shape. Optional in the wire contract; populated by core before persistence. |
| 3 | **`reasons` is writeable by both sides** | ai-engine may prefill `INFERRED_VALUE`, `AMBIGUOUS_NUMBER`, `AUDITOR_UNREACHABLE`, `MODEL_ESCALATED`; core appends check codes. Enum keeps codes machine-checkable, never prose (RFC-004 §3). |
| 4 | **All `fields` optional at the schema level** | "Extractor found nothing" is a valid response; missing required fields become `REQUIRED_FIELD_MISSING` in core's checks, not a schema failure — schema rejection is reserved for malformed output (RFC-004 §4 row 1). |
| 5 | **`search_description`/`embedding` sit outside `fields`** | They are derived artifacts for retrieval (canonical text + one 1024d vector), not evidence-backed extractions; no `exact_quote` exists for them. `embedding` currently optional — see open item O2. |
| 6 | **`classification` carries the label plus per-label probabilities** | Thresholds map probabilities → routing as versioned code (ADR-006); persisting raw probabilities keeps the routing auditable and re-decidable when thresholds change. |
| 7 | **`reference_dataset_version` is required** | RFC-004 §4 requires one versioned dataset consumed by both runtimes; the version must be pinned on every extraction for replay (rule 9). |
| 8 | **`private_tariffs.source` is a proposed extension** over ADR-006's exact field list | Marcelo decided ingestion is via the agent/carrier **website (Firecrawl)**, not sheets. `app.tariff_source_type` (`CSV_UPLOAD`,`RESEND_INBOUND`) has no web value — the enum needs a `FIRECRAWL_*` addition, and `snapshot_id` pins web extractions to a `public_snapshots` row for reproducibility. |
| 9 | **`charge_code` stays free text; `charge_category` mirrors `app.charge_category`** | Carrier codes are not ours to enumerate (data-model §6 note). |
| 10 | **`contract_version` (semver) on `ExtractionResult`** | Lets core reject/snapshot incompatible shapes and record the contract under which a quote was extracted. |

## 4. Proposed answers to RFC-004 open questions (for Marcelo to ratify)

| Q | Question | Proposed answer |
|---|---|---|
| 1 | Required vs optional fields | **Accept default.** Required: `incoterm`, `modal`, `origin_*`, `dest_*`, `equipment_or_unit`+`quantity` (FCL) or `gross_weight_kg`+`volume_m3` (AIR), `cargo_value_*`. `ncm`/`ready_date` optional. LCL cross-check stays deferred (out of v1.3). |
| 2 | `τ_field` / 99% target | **Accept default.** No hardcoded number in the contract; thresholds chosen from the eval set (ADR-008) and versioned via `model_info.thresholds_version`. |
| 3 | Eval set size / who labels | **Accept 50 emails** (~30 RFQ / 10 TARIFF_UPDATE / 10 OTHER), labelled by Marcelo with pilot help. Marcelo has real Litrans emails as seed; **committed fixtures must be anonymized or synthetic** (public repo). Skeleton lives under `apps/ai-engine/tests/eval/` on `feat/ai-engine`. |
| 5 | Degraded mode when both auditor routes unreachable | **Accept: all fields amber, nothing auto-approves.** `AUDITOR_UNREACHABLE` reason code exists in the enum for exactly this. |
| 6 | Vendor consent / local-model fallback | **Stays open** — needs pilot-company consent wording. Proposed default: consent clause in pilot terms + document the Ollama local path as a separately-approved option (ADR-006 currently limits it to offline CI). |
| 7 | ADR-006 injection wording | **Accept RFC-004 §7 suggested edit**: "drop to zero by construction" → "limits the blast radius to a biased decision, which the deterministic checks and human review then catch". |

(Q4 — reference-data licences — is Pedro's.)

## 5. Open items (recorded, not blocking)

- **O1 · `EquipmentOrUnit` enum** needs business review: container types use
  commercial codes (`20DV`…`40TK`); AIR/LCL units guessed (`PALLET`,`PIECE`,`ULD`,`OTHER`).
  Extend the enum in `common.schema.json` only.
- **O2 · Is `embedding` required?** RFC-004 Q5 covers auditor outage only. If the
  embedding model is unreachable, does extraction fail or ship unembedded
  (ranking degraded)? Flag for Marcelo/Pedro; schema leaves it optional.
- **O3 · `tariff_source_type` enum extension** (`FIRECRAWL_*`) for the web-ingestion
  decision — needs Pedro on the DB side and a matching value in the contract's
  `source.type`.
- **O4 · `warnings[].message`** is developer-facing free text — the contract keeps
  `code` machine-checkable; `message` must never reach customer output unvalidated.
- **O5 · `classification.model`** optional but recommended — Stage-2's pinned
  classifier id belongs in `model_info` too; kept on the object for locality.

## 6. Acceptance for Pedro

1. Enums match `app.*` types exactly (§2 note + data-model §6).
2. The `source` extension on `private_tariffs` gets a yes/no, and if yes the
   matching DB enum value.
3. Value-as-uniform-string envelope confirmed for codegen ergonomics.
4. After approval: wire `schemas/` into the codegen config (his task), and the
   contracts-drift check starts covering these files.
