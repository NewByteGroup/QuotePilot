# Tech spec: `ExtractionResult` and `PrivateTariff` wire contracts (JSON Schema)

- **Status:** Decisions ratified by Pedro on 2026-11-16 (source object shape, decimal-string value + tombstone `oneOf`, optional+nullable embedding — see §3); remaining sections still draft for his review.
- **Date:** 2026-11-10 (decisions updated 2026-11-16)
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
  private_tariffs.schema.json     # one append-only tariff row: priced OR
                                  # tombstone via oneOf, plus the `source`
                                  # provenance object (decided 2026-11-16)
```

JSON Schema draft 2020-12 (these are JSON Schema files, not OpenAPI documents;
the "3.0.3 not 3.1" rule applies to OpenAPI). One shared `common.schema.json`
keeps the enums in a single place so the two contracts cannot drift.

## 3. Decisions

| # | Decision | Rationale |
|---|---|---|
| 1 | **Every envelope `value` is a `string`**, enums/dates included; money and quantities are `DecimalString` (never JSON numbers). On the tariff line item the price (`net_amount`) is a REQUIRED decimal string with REQUIRED `currency` — see decision 11 | Uniform envelope type → clean Go/Pydantic codegen; satisfies RFC-004 "decimal strings, never JSON floats" and Pedro's 2026-11-16 call that a JSON number parses as float on first touch (ADR-008). Per-field value vocab documented in each property's `description`. |
| 2 | **`status` is in the schema but never set by `ai-engine`** | Status is computed by `core-api` (checks + `τ_field`, RFC-004 §5) yet web needs it on the persisted shape. Optional in the wire contract; populated by core before persistence. |
| 3 | **`reasons` is writeable by both sides** | ai-engine may prefill `INFERRED_VALUE`, `AMBIGUOUS_NUMBER`, `AUDITOR_UNREACHABLE`, `MODEL_ESCALATED`; core appends check codes. Enum keeps codes machine-checkable, never prose (RFC-004 §3). |
| 4 | **All `fields` optional at the schema level** | "Extractor found nothing" is a valid response; missing required fields become `REQUIRED_FIELD_MISSING` in core's checks, not a schema failure — schema rejection is reserved for malformed output (RFC-004 §4 row 1). |
| 5 | **`search_description`/`embedding` sit outside `fields`**; `embedding` is OPTIONAL and NULLABLE — **DECIDED by Pedro, 2026-11-16** | They are derived artifacts for retrieval (canonical text + one 1024d vector), not evidence-backed extractions; no `exact_quote` exists for them. Per RFC-001 embeddings only rank service descriptions among already-valid lines — they never decide validity, and a batch must not fail because the embedding model was down. Shared `Embedding` def is `["array","null"]` with `minItems = maxItems = 1024` (ADR-006), so the dimension is validated whenever a vector is present; a batch without embedding stays importable and ranking falls back to deterministic order. Resolves open item O2. |
| 6 | **`classification` carries the label plus per-label probabilities** | Thresholds map probabilities → routing as versioned code (ADR-006); persisting raw probabilities keeps the routing auditable and re-decidable when thresholds change. |
| 7 | **`reference_dataset_version` is required** | RFC-004 §4 requires one versioned dataset consumed by both runtimes; the version must be pinned on every extraction for replay (rule 9). |
| 8 | **`private_tariffs.source` is a required provenance OBJECT** — **DECIDED by Pedro, 2026-11-16** | A tariff's origin must answer "where from, when, with what content". Shape: `schema_version` (integer, evolution without breaking), `kind` (enum mirroring `app.tariff_source_type` — `CSV_UPLOAD`, `RESEND_INBOUND` — plus `FIRECRAWL_PUBLIC` for portal-scraped tariffs), `ref` (portal URL / file name / resend message id), `fetched_at` (date-time), `content_hash` (string). The hash + `fetched_at` are what the Devin repair loop and the audit screen use to say "this line came from this file at this version". Object kept deliberately small; the earlier draft's `snapshot_id` is dropped in favour of `content_hash` + `fetched_at`. Replaces the draft's proposed `{type, ref, snapshot_id}` extension. |
| 9 | **`charge_code` stays free text; `charge_category` mirrors `app.charge_category`** | Carrier codes are not ours to enumerate (data-model §6 note). |
| 10 | **`contract_version` (semver) on `ExtractionResult`** | Lets core reject/snapshot incompatible shapes and record the contract under which a quote was extracted. |
| 11 | **A tariff row is `{value + currency}` OR `{tombstone}` — `oneOf`, never optional** — **DECIDED by Pedro, 2026-11-16** | The price is REQUIRED and a decimal STRING, never a JSON number (a JSON number parses as float on first touch; ADR-008 bans float money). `currency` is a separate REQUIRED field on priced rows. The exception is a tombstone line (`is_removed`), which has no value: the schema is a `oneOf` of a priced row (`is_removed: false`, requires `currency`+`net_amount`+`valid_from`+`valid_until`+`search_description`) and a tombstone (`is_removed: true`, all price/description/embedding fields forbidden) so a missing value can never pass as "optional". `is_removed` itself is required on the wire so every row declares its shape explicitly. Naming: Pedro's "value" is the tariff's price field `net_amount`, keeping the wire aligned with `app.private_tariffs` and ADR-006's field list. |
| 12 | **`embedding` OPTIONAL and NULLABLE on both contracts** — **DECIDED by Pedro, 2026-11-16** | Same decision as row 5, stated for the tariff row as well: 1024 dims validated when present; absent or `null` keeps the row importable, ranking falls back to deterministic order. Tombstones forbid `embedding`/`embedding_model` outright. |

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
- **O2 · Is `embedding` required?** — **DECIDED by Pedro, 2026-11-16.** Optional
  and nullable on both contracts: embeddings only rank service descriptions
  among already-valid lines, they never decide validity, and a batch must not
  fail because the embedding model was down. A batch without embedding stays
  importable; ranking falls back to deterministic order. The shared `Embedding`
  def validates exactly 1024 items whenever a vector is present.
- **O3 · `tariff_source_type` enum extension** (`FIRECRAWL_*`) — contract side
  **DECIDED by Pedro, 2026-11-16**: `source.kind` carries `FIRECRAWL_PUBLIC`.
  DB side still open by design: the `FIRECRAWL_*` kind means
  `app.tariff_source_type` needs a new enum value via a later migration —
  flagged for Pedro's approval at that migration; not implemented here.
- **O4 · `warnings[].message`** is developer-facing free text — the contract keeps
  `code` machine-checkable; `message` must never reach customer output unvalidated.
- **O5 · `classification.model`** optional but recommended — Stage-2's pinned
  classifier id belongs in `model_info` too; kept on the object for locality.

## 6. Acceptance for Pedro

1. Enums match `app.*` types exactly (§2 note + data-model §6).
2. ~~The `source` extension on `private_tariffs` gets a yes/no~~ — answered
   YES (2026-11-16) with the decided object shape (decision 8). The matching
   DB enum value (`FIRECRAWL_*` on `app.tariff_source_type`) is the pending
   migration flagged in O3.
3. ~~Value-as-uniform-string envelope confirmed~~ — confirmed 2026-11-16:
   uniform string envelopes stay, and on tariff rows the price is a required
   decimal string under the priced/tombstone `oneOf` (decision 11).
4. After approval: wire `schemas/` into the codegen config (his task), and the
   contracts-drift check starts covering these files.
