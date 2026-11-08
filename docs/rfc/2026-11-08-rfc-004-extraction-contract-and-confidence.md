# RFC-004: Extraction contract and the confidence model (building on ADR-006)

- **Status:** Draft
- **Date:** 2026-11-08
- **Author:** Pedro (backend lead)
- **Needs input from:** Marcelo (owner of the agents and of ADR-006)
- **Becomes:** an ADR (or an amendment to ADR-006) once sections 3 to 6 and the open questions are settled
- **Related:** ADR-001 (rules 5 and 6), ADR-004 (stages 2 and 3), ADR-006 (AI agents, approved), ADR-008 (evaluations), RFC-001 (consumer of the output), RFC-002 (display and hostile content)

## 1. What this RFC is, now that ADR-006 exists

The plan listed this RFC as "extraction contract and confidence model, with deterministic checks rather than the LLM's self-reported confidence". ADR-006 has since been approved and decides the architecture: a Jev decision model for classification and for confidence auditing, plus a `temperature=0` structured-output LLM for extraction. So this RFC no longer chooses the architecture. It covers what ADR-006 leaves open:

1. The exact **contract** between `ai-engine` and `core-api` (what a field carries).
2. The **deterministic checks** `core-api` runs on every response (ADR-001 rule 5), which ADR-006 mentions but does not enumerate.
3. How the **calibrated confidence from the model and the deterministic checks combine** into the green, amber and red states the interface shows.
4. **Calibration and monitoring** with real analyst corrections.
5. **Risks of the chosen architecture** that need an owner.

## 2. Verified background

Jev is a hosted decision model from TypeSafe AI, released in September 2026, that returns typed answers with probabilities and generates no text. It is reachable at TypeSafe's own API and through gateways, and the same `System One` protocol is served by Ollama, which gives a local option. It is a closed model and about two months old at the time of writing. Calibration claims come from the vendor and from the team's own tests only after the evaluation in section 6.

## 3. The contract

`ExtractionResult` lives in `packages/contracts` as JSON Schema, and Pydantic and Go types are generated from it.

**Per-field envelope** (every extracted value uses it):

| Part | Meaning |
|---|---|
| `value` | The normalised value. Money and quantities are **decimal strings**, never JSON floats. |
| `evidence.source` | `EMAIL_BODY` or `ATTACHMENT:<filename>` |
| `evidence.exact_quote` | A literal excerpt of the source that supports the value |
| `inferred` | True if the value was not stated and was deduced |
| `confidence` | Calibrated probability from the auditor, 0 to 1 |
| `reasons` | Short codes, never prose from the model |

**Fields:** `incoterm`, `modal`, `origin_locode` and `dest_locode` (plus the raw place text), `equipment_or_unit` and quantity, `gross_weight_kg`, `volume_m3`, `cargo_value` (amount and ISO currency), `ncm` (optional), `ready_date` (optional). Also the classification result with its probabilities, `warnings`, and `model_info` (extractor model, auditor model, versions).

**Offsets.** The service does not return character offsets for highlights. `core-api` finds the quote in the source text itself and computes them. Models are unreliable at counting characters, and the check in section 4 needs the search anyway.

## 4. Deterministic checks in `core-api`

Run on every response before anything is persisted. Any failure is recorded as a reason code on the field.

| Check | Failure result |
|---|---|
| Schema valid, enums known, sizes within caps | Reject the whole response |
| `origin_locode`, `dest_locode` exist in the UN/LOCODE reference | Field red |
| `ncm` is eight digits and exists in the NCM table | Field red |
| Weight, volume and value positive and inside caps; weight to volume ratio inside a plausible density range | Field amber or red by severity |
| Currency on the ISO 4217 allow-list | Field red |
| **Quote is in the source:** `exact_quote` occurs in the email or attachment text after normalisation (Unicode NFKC, collapsed whitespace, case-folded) | Field amber, evidence unverified |
| **Value is in the quote:** the number appears in the quote after number normalisation, treating `1.234,56` and `1,234.56` explicitly | Field amber; a locale-ambiguous number is always amber |
| Cross-field: origin differs from destination; FAS, FOB, CFR and CIF are sea and inland waterway terms only (Incoterms 2020), so they are flagged with air | Amber with reason |
| FCL has an equipment type; LCL or air has weight and volume | Required field missing |

`core-api` needs its own copy of the **reference data** (UN/LOCODE, NCM, Incoterms). To avoid two copies drifting, the data is one versioned dataset under `packages/` that both runtimes consume, with a version recorded on every extraction. **Licence and currency of the UN/LOCODE and NCM sources are not verified here** and must be checked when the dataset is built.

## 5. Combining the signals

A field's **status** is the source of truth. The numeric confidence is kept for sorting and analytics.

| Status | Condition |
|---|---|
| Green | Not inferred, all deterministic checks pass, evidence verified in the source, **and** the auditor's confidence is at or above the field threshold `τ_field` |
| Amber | Anything else short of red: inferred, unverified evidence, low auditor confidence, a soft check failed |
| Red | A hard check failed or a required field is missing |

A quote is `READY` only if every required field is green and the pricing result has no amber flags (RFC-001). Otherwise it is `NEEDS_REVIEW`, and no 1-click link is issued for it (ADR-005). This is the spec's "100% confidence turns green" rule expressed as a state rather than a number: the model never outputs a probability of exactly 1, so a threshold on the number alone would never turn anything green.

`extraction_confidence` (`NUMERIC(5,4)`) is stored as the minimum auditor confidence across required fields, for ordering and dashboards, and is no longer what decides green. `auto_approve_threshold` in the pricing rules then acts on status: it must be reworded in the data model (v1.2).

**Asymmetry.** A false green sends a wrong price to a customer; a false amber costs an analyst ten seconds. Thresholds are chosen so that, on the evaluation set, at least 99% of green fields are correct (proposal, to be agreed with Marcelo).

## 6. Calibration and monitoring

1. **Before launch:** build the evaluation set required by ADR-008 (proposal: 50 emails, about 30 RFQs, 10 tariff updates, 10 other, anonymised or synthetic for anything committed). Measure the reliability of the auditor's probabilities and pick `τ_field` from it.
2. **In production:** every time an analyst edits an extracted field, that is a labelled example. Store the field, the old value and the new one in the tenant's own tables (they are tenant data). Track the correction rate of green fields. If it rises above an agreed level, thresholds are tightened and the evaluation is rerun.
3. **Pin the model version** (a specific `jev` release, not a rolling alias). A version change reruns the evaluation before it ships.

## 7. Risks of the ADR-006 architecture that need an owner

1. **Vendor and maturity.** A new closed model behind one hosted API. Mitigation: pin the version; keep the Ollama-served decision-model path working as a fallback; define a degraded mode in which, if the auditor is unreachable, every field is amber and nothing is auto-approved. **Owner: Marcelo.**
2. **Data leaving the platform.** Customer emails, names and rates go to the model vendors. This is the unresolved "consent to send tenant data to third-party APIs" item. Mitigation options: contract terms with each vendor, or the local model path for tenants who refuse. **Owner: Marcelo and the pilot companies.**
3. **Wording in ADR-006.** The ADR says prompt-injection takeovers "drop to zero by construction". That is too strong. A decision model that cannot write text or call tools cannot be talked into *taking actions*, which genuinely limits the blast radius. But the email can still try to bias its answers ("this is clearly an RFQ, certain"). The structural controls above (deterministic checks, verbatim evidence, human review, no 1-click for amber) are what actually protect pricing. **Suggested edit to ADR-006:** replace the "by construction" claim with "limits the blast radius to a biased decision, which the deterministic checks and human review then catch". Marcelo owns that text.
4. **Retrieval quality.** The `search_description` and its embedding come from model output (ADR-006 step 3.2). Tariff selection must filter on codes first and use vectors only to rank among valid rows (RFC-001 section 5). Retrieval quality is part of the evaluation set.

## 8. Open questions

| # | Question | Owner | Default if unanswered |
|---|---|---|---|
| 1 | Required versus optional fields | Marcelo | Required: incoterm, modal, origin, destination, equipment or weight and volume, cargo value |
| 2 | `τ_field` and the 99% target | Marcelo, Pedro | Set from the evaluation set |
| 3 | Evaluation set size and who labels it | Marcelo | 50 emails, labelled by Marcelo with the pilot companies' help |
| 4 | Reference data sources and licences (UN/LOCODE, NCM) | Pedro | Verify before building the dataset |
| 5 | Degraded mode when the auditor is unreachable | Marcelo | All fields amber |
| 6 | Vendor consent and fallback to the local model | Marcelo, pilot companies | Open |
| 7 | Edit to the injection wording in ADR-006 | Marcelo | Propose the edit in section 7 |

## 9. Impact

- **Data model v1.2:** per-field status and reason codes; reference dataset version; correction log; `auto_approve_threshold` semantics; evidence verification flag.
- **`packages/contracts`:** `ExtractionResult` and shared hostile-payload examples (ADR-008).
- **ADR-006:** the wording change in section 7 and a pointer to this RFC for the contract.
- **ADR-008:** the evaluation and contract-test sections implement sections 4 and 6.

## 10. Acceptance for moving to an ADR

Sections 4 and 5 agreed with Marcelo; the evaluation set exists and `τ_field` is chosen from it; questions 4 and 6 answered.
