# RFC-001: Pricing engine: Incoterms, calculation order, rounding, margin scope and FX

- **Status:** Draft
- **Date:** 2026-11-08
- **Author:** Pedro (backend lead)
- **Needs input from:** Marcelo (product owner) and the pilot companies for the business rules marked **[business]**
- **Becomes:** an ADR once the open questions in section 9 are answered
- **Related:** ADR-004 (the worker is the only caller of the pricing package), ADR-008 (money tests), spec section 4.4, Notion comment 8

## 1. Why this RFC exists

The equation in spec section 4.4 is a good outline, but the engine cannot be written from it. Five things are undefined:

1. The Incoterm decides which cost groups belong in the quote. The equation ignores it.
2. For the import proposal, destination terminal fees may depend on a CIF-based value and the freight amount; export terminal fee incidence and calculation basis still need validation.
3. Origin charges and surcharges (GRI, BAF, peak season) appear as line categories but not in the equation.
4. Rounding the eligible FCL ocean or air freight markup and converting each line can produce cent differences; the line sell amounts must add up to the quote exactly.
5. The v1.3 spec fixes markup scope: FCL `OCEAN_FREIGHT` and base air freight only; local charges, FSC/SSC and fixed fees pass through. Exchange-rate selection remains open.

`internal/pricing` is a pure package (no I/O, decimal only). Everything below is what that package takes in and gives back.

## 2. Scope and assumptions

- **MVP covers imports into and exports from Brazil in FCL Hapag and AIR.** Quote only charges payable by the requesting customer (importer or exporter) under the Incoterm. The destination-terminal, CIF-band storage, THC and customs-dispatch examples below describe an import proposal; export charge allocation remains open for validation.
- Import taxes and duties (II, IPI, ICMS, PIS/COFINS) are proposed outside the quote. **[business] confirm that exclusion; export-specific customs and tax treatment remains open in question 16.**
- FCL Hapag and air are in the MVP. LCL, Inland, Door-to-Door and AFRMM are outside the MVP, even if an enum or historical tariff still names them.
- BRL output remains the current proposal. Confirm the export quote currency and FX treatment under open question 16. Foreign-currency amounts are converted line by line and every applied rate is recorded.

## 3. Inputs and outputs

**Input struct** (built by the worker inside the tenant transaction, then passed by value):

| Input | Source |
|---|---|
| Extracted cargo: Incoterm, modal, origin and destination LOCODE, equipment or units, gross weight, volume, cargo value and currency, NCM | Stage 3 (RFC-004) |
| Organization rules, resolved by precedence route, then modal, then `ALL` | `pricing_rules` |
| Valid private tariffs for the route | `private_tariffs` |
| Pinned public snapshots (terminal fees, carrier FX) and the PTAX reference rate | Layer C, by `public_snapshot_id` |
| Quote date, pricing instant `priced_at` and an injected clock | the worker |

**Output struct:** lines (category, description, source type, referenced tariff or snapshot id, original currency and amount, FX rate used, cost BRL, sell BRL, flags), totals, `warnings`, the list of `assumptions` applied, a status of `COMPLETE` or `INCOMPLETE` with reasons, and an `inputs_hash`.

**Determinism:** the same input produces the same output, byte for byte. No map iteration order, no time reads, no randomness. This is what makes quotes reproducible (architecture rule 9) and replayable in tests.

## 4. Which cost groups the quote includes, by Incoterm

The forwarder quotes only the charges payable by the requesting customer, whether importer or exporter, under the Incoterm. The following matrix is an **import-only proposal** for the buyer's side and has not been validated for export; the export matrix is an open design item with the pilot companies **[business]**:

| Incoterm (2020) | Origin charges | International freight | Terminal storage and THC | Customs dispatch | Where the freight for the CIF base comes from |
|---|---|---|---|---|---|
| EXW | yes | yes | yes | yes | private tariff |
| FCA | applicable origin terminal or loading fees; inland pre-carriage is outside the MVP | yes | yes | yes | private tariff |
| FOB (also FAS) | no (seller's) | yes | yes | yes | private tariff |
| CFR, CIF, CPT, CIP | no | **no** (seller pays; the declared freight is an input, not a charge) | yes | yes | declared in the RFQ or invoice |
| DAP, DPU | no | no | per RFQ | yes | declared |
| DDP | nothing to quote (seller clears) | | | | |

Notes:
- FAS, FOB, CFR and CIF are for sea and inland waterway only. The extractor flags them when paired with air (RFC-004).
- Under the CIF family the freight is paid by the seller but still belongs in the customs value, so it enters the calculation as a number from the documents and never as a charge line.
- Insurance for the CIF value is an input (declared, or zero). **[business] confirm whether to add an insurance line.**

## 5. Calculation order

1. **Validate and resolve.** Reject negative or invalid markup percentages. Resolve `route_target_margin` before the spec's `default_markup_rate`, and modal before `ALL`. Both rates mean markup on freight cost for the MVP. The proposed field rename is documented in the spec only; schema and contract changes require separate review. The data model enforces one rule per scope with `UNIQUE NULLS NOT DISTINCT` (comment 10).
2. **Chargeable quantity.**
   - FCL: number of equipment units.
   - LCL: W/M calculation is deferred beyond the MVP.
   - Air: `chargeable_kg = max(gross_kg, volume_m3 × 166.67)`. This factor is fixed for the MVP; any carrier-specific exception requires a later decision.
3. **Select tariffs.** Filter by direction, route, modal and equipment for charges payable by the requesting customer. Before this pure package runs, the proposed Stage 4A (a v1.3 amendment marked *proposed, not yet adopted* in ADR-004, pending Pedro's review of the offer contract and stage sequence) obtains a Hapag FCL offer through the `ai-engine` connector only when the customer owes ocean freight under the Incoterm; the worker passes that offer as pricing input. The package selects an AIR freight tariff only when the customer owes air freight. If freight is not payable by the customer, do not create a sell line for it. Direction and export tariff coverage still need validation. For each tariff natural key (carrier, route, modal, equipment, charge_category, charge_code), consider the rows whose own validity covers the quote date (`valid_from <= date <= valid_until`) and whose batch was activated at or before the pricing time (`activated_at <= priced_at`); take the row from the batch with the latest `activated_at`. If that row is a tombstone (`is_removed`), that natural key is dropped from the candidates (for example one carrier's rate); whether the result is then `INCOMPLETE` follows the offer rule in open question 12. See RFC-002 step 3 (tariff versioning). Filtering is deterministic on codes; embeddings only help rank service descriptions among already-valid rows (spec section 5.4 warning applies). If several carriers qualify, the default is: price the lowest net cost, list the others as alternatives in the audit view. **[business] one price or several options?** Missing tariff for a required group makes the result `INCOMPLETE`; the engine never invents a number.
4. **Freight and pass-through lines.** When the customer owes FCL Hapag ocean freight, apply cost-based markup only to `OCEAN_FREIGHT`, with floor `min_markup_usd × container_count`. When the customer owes air freight, calculate base net freight as `max(min_amount, chargeable_kg × net_amount)` and apply cost-based markup only to that base. Local charges, air FSC/SSC and fixed fees are separate pass-through lines sold at cost only when payable by the requesting customer under the Incoterm. Convert each line to BRL using the pinned FX rate (section 7).
5. **Import CIF base in BRL, when applicable.** For import fee bands only, use `(goods value + freight + insurance)`, with freight from the tariff when payable by the customer or declared in the documents when paid by the other party. Convert at the still-open customs rate (section 7). Do not assume this CIF-based fee basis applies to exports.
6. **Terminal lines by direction.** For the import proposal, look up the destination public fee band for the CIF base, weight and volume; first-period storage remains a business assumption. Which export origin/destination terminal charges are payable by the requesting customer, and their calculation basis and source, remain open **[business]**.
7. **Organization fixed fees.** Include fees from `fixed_fees_json` only when payable by the requesting customer under the Incoterm; convert if not in BRL. The export allocation is open in question 13.
8. **Cost total** is the sum of the rounded line costs (section 6), so what the analyst audits adds up.
9. **Markup and price** (sections 6 and 8): add the markup to the eligible FCL ocean or base air freight only, then add all other lines at cost.

## 6. Rounding and cent allocation

- Money is `decimal`. BRL amounts have two decimal places; FX rates have six (`NUMERIC(10,6)`).
- Each cost line is rounded **half up** to the cent when it is converted to BRL. Banker's rounding is rejected because it surprises people who check by hand. **[business] confirm.**
- Price is the sum of rounded sell lines. For FCL, the eligible ocean freight sell amount is `Net_Ocean + max(Net_Ocean × M_route, min_markup_usd × container_count)`; for air, it is `Net_Air × (1 + M_route)`. All other lines sell at their rounded cost. Convert foreign-currency amounts to BRL with pinned FX rates before the final sum.
- Allocate any rounding cent deterministically among eligible freight lines so that **the sum of line sell amounts equals the quote price**; pass-through lines remain equal to their rounded cost. Tie order and half-up behavior remain part of the golden-quote validation (ADR-008).
- `M_route` is markup **on freight cost**, not margin on the final selling price. The product spec names the route field `route_target_margin` and the default field `default_markup_rate`; the default-field rename is not yet a schema or contract change. Label the UI as markup.

## 7. Exchange rates

- A rate is looked up by `(source, currency, date)`. Carrier or coloader foreign-exchange rates ("dólar armador") come from the public connector; the PTAX reference comes from the Central Bank's official open-data API (see RFC-003).
- Freight and other carrier-charged amounts use the **carrier's** rate times `(1 + s_org)`, where `s_org` is the organization's protective spread. If the carrier has no published rate for the day, the engine falls back to PTAX plus `s_org` and flags the line `FX_FALLBACK` (amber).
- Which PTAX bulletin and side to use: proposal is the closing bulletin of the previous business day until today's is published, selling side for amounts the company pays out. **[business] confirm.**
- **Which rate converts the goods value for the CIF base is open.** Customs valuation in Brazil uses the customs exchange rate rather than the carrier's rate. **[business] confirm which rate the terminals and the pilot companies use for the fee bands.**
- A rate older than one business day is flagged `STALE_FX`. Every rate applied is stored in `fx_rates_applied_json` and per line in `fx_rate_used`.
- Recalculation always creates a new quote version with the then-current rates. Old versions are never changed.

## 8. Markup scope

The v1.3 product spec resolves the MVP scope: When the requesting customer owes ocean freight, FCL Hapag uses cost-based markup only on `OCEAN_FREIGHT`, with a floor of `min_markup_usd` per container; local costs pass through. When the customer owes air freight, Air uses `chargeable_kg = max(gross_kg, volume_m3 × 166.67)` and `Net_Air = max(min_amount, chargeable_kg × net_amount)`; cost-based markup applies only to that base air freight, while FSC/SSC and fixed fees pass through. LCL, Inland, Door-to-Door and AFRMM are outside the MVP.

The earlier alternatives of margin on the whole price or separate pass-through markup are superseded for the MVP. This RFC remains Draft because the Incoterm matrix, customs/FX/PTAX choices, rounding details and golden-quote evidence still need validation.

## 9. Open questions

| # | Question | Owner | Default if unanswered |
|---|---|---|---|
| 1 | MVP direction and modal scope | Pedro / product spec v1.3 | Resolved: imports and exports in FCL Hapag and AIR; charge only what the requesting customer owes |
| 2 | Quote excludes taxes and duties | Marcelo | Yes |
| 3 | Incoterm matrix in section 4 matches pilot practice | Pilot companies | As proposed |
| 4 | Markup scope for FCL Hapag and air | Pedro / product spec v1.3 | Resolved for MVP: eligible freight cost only; pass-through lines at cost |
| 5 | One recommended price, or several carrier options | Marcelo | One price plus alternatives in the audit view |
| 6 | Customs rate vs carrier rate for the CIF base | Pilot companies | Customs rate, flagged for confirmation |
| 7 | PTAX bulletin and side | Marcelo | Previous closing, selling side |
| 8 | Air volumetric factor; LCL W/M convention deferred | Pilot companies | Air: 166.67 kg per m³ for MVP; LCL out of scope |
| 9 | Storage period assumption | Pilot companies | First period only |
| 10 | Add an insurance line | Marcelo | No; insurance is an input only |
| 11 | Half-up rounding | Marcelo | Half up |
| 12 | What makes a tariff offer complete: must one carrier cover every required freight and origin group, or may groups from different parties combine (for example EXW origin charges from an origin agent plus ocean freight)? | Marcelo, pilot companies | Per carrier: an offer is usable only if that carrier covers every required freight and origin group; public terminal fees are carrier-independent; `INCOMPLETE` if no offer is usable. Never combine across carriers silently. |
| 13 | Export Incoterm matrix: which freight, origin/export customs, terminal, destination and fixed charges are payable by the requesting exporter or importer for each supported Incoterm? | To confirm with pilot companies | Open; current section 4 matrix applies to imports only |
| 14 | Export terminal and handling charges: which source, location and calculation basis apply instead of import CIF-band destination storage? | To confirm with pilot companies | Open; do not reuse import CIF formula automatically |
| 15 | Export tariff coverage and direction routing: can Hapag return FCL export offers for the pilot routes, and which AIR export tariffs are available; how are ambiguous or reverse routes handled? | To confirm with pilot companies | Open; no unverified reverse-route rate |
| 16 | Export quote currency, FX source/timing and any export-specific customs or tax treatment? | To confirm with pilot companies | Open; keep current BRL output only as an explicit proposal |

## 10. Impact

- **Data model v1.2:** `charge_category` and `charge_code` on tariffs; `min_markup_usd` on pricing rules for the FCL floor; `inputs_hash` and line `flags` on quotes; a rounding rule note in the schema comments; tariff versioning (`is_removed`, batch `activated_at`, quote `priced_at`) as defined in RFC-002 step 3 (tariff versioning). The superseded all-cost margin options do not require new `margin_scope` or `k` fields.
- **ADR-004:** unchanged. The worker remains the only caller.
- **ADR-008:** the money tests in that ADR implement sections 5 and 6.
- **Connectors (RFC-003):** terminal bands and carrier FX must be exposed in a shape the lookups above can use.

## 11. Acceptance for moving to an ADR

Before this Draft RFC can become an ADR, questions 3, 6, 7 and the new export questions 13–16 must be answered. The per-modal cost markup and rounding must then be confirmed against pilot practice, and a draft implementation must reproduce ten historical quotes to the cent using synthetic equivalents (ADR-008). The golden-quote set must include representative import and export cases for both FCL Hapag and AIR.
