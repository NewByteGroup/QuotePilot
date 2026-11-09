# RFC-001: Pricing engine: MVP FCL/AIR markup, Incoterms, rounding and FX

- **Status:** Draft
- **Date:** 2026-11-08
- **Author:** Pedro (backend lead)
- **Needs input from:** Marcelo (product owner) and the pilot companies for the business rules marked **[business]**
- **Becomes:** an ADR once the remaining Incoterm, FX and golden-quote questions are answered
- **Related:** ADR-004 (the worker is the only caller of the pricing package), ADR-008 (money tests), spec section 4.4, Notion comment 8

## 1. Why this RFC exists

Product spec v1.3 fixes the MVP modes and freight markup rules. The earlier global equation in spec section 4.4 is retired. The remaining design work concerns:

1. The Incoterm and import/export direction decide which costs the requesting customer pays. The earlier equation ignores both.
2. The order of calculation where an applicable import terminal fee depends on a CIF value that includes freight.
3. Which origin, destination and carrier surcharges are required by the chosen offer and Incoterm.
4. Rounding after applying markup to freight alone; line sell amounts must add up to the price exactly.
5. Which exchange rate applies to each foreign-currency amount and the USD-denominated FCL markup floor.

`internal/pricing` is a pure package (no I/O, decimal only). Everything below is what that package takes in and gives back.

## 2. Scope and assumptions

- **MVP scope:** FCL and AIR imports and exports, quoting only costs payable by the requesting customer under the Incoterm, whether that customer is the importer or exporter. LCL, Inland/Door-to-Door and AFRMM are out of scope. A Python connector in `ai-engine` fetches a Hapag-Lloyd FCL offer only when the customer owes ocean freight; `core-api` validates and persists its response and performs all money arithmetic.
- The earlier destination-terminal, CIF-band, THC and customs-dispatch assumptions describe an import buyer-side case. Their applicability to a given import and any export allocation or fee basis need validation; section 4 is not an export matrix.
- Taxes and duties (II, IPI, ICMS, PIS/COFINS) are **not** part of the quote. The spec equation has none. **[business] confirm that the forwarder's proposal excludes them.**
- The approved ADR-006 classifier still lists `LCL` as a possible hint. That does not authorize LCL pricing in the v1.3 MVP; extraction and routing behaviour for an LCL request needs reconciliation before implementation.
- All output is in BRL. Foreign-currency amounts are converted line by line and the rate is recorded.

## 3. Inputs and outputs

**Input struct** (built by the worker inside the tenant transaction, then passed by value):

| Input | Source |
|---|---|
| Extracted cargo: Incoterm, modal, origin and destination LOCODE, equipment or units, gross weight, volume, cargo value and currency, NCM | Stage 3 (RFC-004) |
| Organization rules, resolved by precedence route, then modal, then `ALL` | `pricing_rules` |
| Valid private tariffs for AIR freight when payable and other applicable charges | `private_tariffs` |
| Validated Hapag-Lloyd FCL offer, only when the customer pays ocean freight | Python connector in `ai-engine`; `core-api` validates and persists its output before pricing |
| Applicable pinned public snapshots (such as import terminal fees and carrier FX) and the PTAX reference rate | Layer C, by `public_snapshot_id` |
| Quote date, pricing instant `priced_at` and an injected clock | the worker |

**Output struct:** lines (category, description, source type, referenced private tariff, validated Hapag offer or public snapshot id, original currency and amount, FX rate used, cost BRL, sell BRL, flags), totals, `warnings`, the list of `assumptions` applied, a status of `COMPLETE` or `INCOMPLETE` with reasons, and an `inputs_hash`. A `carrier_offer_id` reference for FCL replay is a proposed schema/contract addition, subject to Pedro's approval.

**Determinism:** the same input produces the same output, byte for byte. No map iteration order, no time reads, no randomness. This is what makes quotes reproducible (architecture rule 9) and replayable in tests.

## 4. Which cost groups the quote includes, by Incoterm

**Import buyer-side proposal only.** The actual rule for imports and exports is to quote the requesting customer's own payable costs under the Incoterm, whether the customer is the importer or exporter. The table below predates that clarification and requires revalidation even for imports; it does not define an export matrix. Terminal-fee allocation in particular varies in practice **[business]**. It does not add Inland/Door-to-Door service to the MVP:

| Incoterm (2020) | Origin charges | International freight | Terminal storage and THC | Customs dispatch | Where the freight for the CIF base comes from |
|---|---|---|---|---|---|
| EXW | yes | yes | yes | yes | Hapag offer for FCL; private tariff for AIR |
| FCA | per RFQ: applicable origin terminal or loading fees; inland pre-carriage is outside the MVP | yes | yes | yes | Hapag offer for FCL; private tariff for AIR |
| FOB (also FAS) | no (seller's) | yes | yes | yes | Hapag offer for FCL |
| CFR, CIF, CPT, CIP | no | **no** (seller pays; the declared freight is an input, not a charge) | yes | yes | declared in the RFQ or invoice |
| DAP, DPU | no | no | per RFQ | yes | declared |
| DDP | nothing to quote (seller clears) | | | | |

Notes:
- Origin and local charges in this table mean applicable port, terminal or agency fees, not an Inland/Door-to-Door pickup or delivery service.
- FAS, FOB, CFR and CIF are for sea and inland waterway only. The extractor flags them when paired with air (RFC-004).
- In this import buyer-side example, freight paid by the seller under the CIF family may still enter the customs value from the documents; it is not a charge line payable by the buyer. The export seller-side treatment remains open.
- Insurance for the CIF value is an input (declared, or zero). **[business] confirm whether to add an insurance line.**

## 5. Calculation order

1. **Validate and resolve.** Determine direction, the requesting customer's role and payable cost groups under the Incoterm. Reject negative cost-based markup rates and negative `min_markup_usd`. Resolve rules by precedence route, modal, `ALL`. The data model enforces one rule per scope with `UNIQUE NULLS NOT DISTINCT` (comment 10); mapping the v1.3 markup settings to the schema still needs Pedro's approval.
2. **Chargeable quantity.** FCL uses the container count from the RFQ or validated offer when ocean freight is payable. AIR uses `max(gross_weight_kg, volume_m3 × 166.67)` kg when air freight is payable. The 166.67 factor is the v1.3 rule; it replaces the earlier rounded 167 factor. Validate weights, volumes, units and container counts against pilot examples before pricing.
3. **Select inputs.** Fetch a Hapag-Lloyd FCL offer through the Python connector only if the requesting customer owes ocean freight; `core-api` validates and persists it. When the customer owes air freight, AIR uses valid private tariff rows. Other payable charges use applicable valid rows. For each private-tariff natural key (carrier, route, modal, equipment, charge_category, charge_code), consider rows valid on the quote date whose batch was activated by `priced_at`; take the latest activated row. A tombstone removes that key (RFC-002 section 6, tariff versioning). Filtering is deterministic on codes; embeddings only rank already-valid rows. Missing any required payable freight or charge input makes the result `INCOMPLETE`; the engine never invents a number. How to present multiple valid offers remains open.
4. **Freight and pass-through lines.** If FCL ocean freight is payable, mark up only the `OCEAN_FREIGHT` base from the selected Hapag-Lloyd offer, using a cost-based rate and at least `min_markup_usd` per container. Payable local charges pass through at cost. If AIR freight is payable, `base_net = max(min_amount, chargeable_weight_kg × net_amount)`; mark up only that base air-freight cost at the cost-based rate. Payable FSC, SSC and fixed fees pass through at cost. Keep each applicable surcharge and fee as a separate audited line; convert using recorded rates (section 7). Neither mode applies a global margin to the total.
5. **Import CIF base, only if applicable.** For an import fee that uses CIF, the proposed base is `(goods value + freight + insurance)`. Freight comes from the validated Hapag offer for payable FCL ocean freight, the AIR private tariff for payable air freight, or a declared amount in the documents when another party pays it. Convert at the customs rate proposed in section 7. Whether export fees use any comparable base is open.
6. **Applicable terminal lines.** For an import terminal fee confirmed to use CIF, look up the public fee band using that base, weight and volume. Include only fees payable by the requesting customer. Storage covers the first period only unless the RFQ says otherwise; **[business] confirm** this import assumption. Export fee allocation, basis and tariff availability remain open.
7. **Organization fixed fees.** Include only fees payable by the requesting customer from `fixed_fees_json`, converted if not in BRL.
8. **Cost total** is the sum of the rounded line costs (section 6), so what the analyst audits adds up.
9. **Freight-only markup and final price** (sections 6 and 8).

## 6. Rounding and line reconciliation

- Money is `decimal`. BRL amounts have two decimal places; FX rates have six (`NUMERIC(10,6)`).
- Each cost line is rounded **half up** to the cent when it is converted to BRL. Banker's rounding is rejected because it surprises people who check by hand. **[business] confirm.**
- Round each freight and pass-through sell line to BRL cents after its recorded FX conversion, then sum those rounded sell lines to obtain the final price. **The sum of line `sell_amount_brl` equals `final_price_brl` exactly** (ADR-008). There is no global target price whose cents must be allocated across unrelated pass-through lines.
- The old global `P = C / (1 - M)` margin-on-price formula and its largest-remainder allocation are retired for the v1.3 MVP. The UI and audit trail must distinguish cost-based freight markup from the resulting margin-on-price metric.

## 7. Exchange rates

- A rate is looked up by `(source, currency, date)`. Carrier or coloader foreign-exchange rates ("dólar armador") come from the public connector; the PTAX reference comes from the Central Bank's official open-data API (see RFC-003).
- Freight and other carrier-charged amounts use the **carrier's** rate times `(1 + s_org)`, where `s_org` is the organization's protective spread. If the carrier has no published rate for the day, the engine falls back to PTAX plus `s_org` and flags the line `FX_FALLBACK` (amber).
- Which PTAX bulletin and side to use: proposal is the closing bulletin of the previous business day until today's is published, selling side for amounts the company pays out. **[business] confirm.**
- **Which rate converts goods value for an applicable import CIF base is open.** Customs valuation in Brazil uses the customs exchange rate rather than the carrier's rate. **[business] confirm which rate the terminals and the pilot companies use for import fee bands; export fee FX remains open.**
- A rate older than one business day is flagged `STALE_FX`. Every rate applied is stored in `fx_rates_applied_json` and per line in `fx_rate_used`.
- Recalculation always creates a new quote version with the then-current rates. Old versions are never changed.

## 8. Freight-only cost-based markup (v1.3 MVP)

- **FCL when the customer pays ocean freight:** for an `OCEAN_FREIGHT` base expressed in USD, `markup_usd = max(base_ocean_cost_usd × markup_rate, min_markup_usd × container_count)` and `sell_ocean_usd = base_ocean_cost_usd + markup_usd`. `min_markup_usd` is a per-container floor, not a surcharge on every line. Payable local charges pass through at cost. The conversion and floor comparison for a non-USD offer remain an FX question in section 9.
- **AIR when the customer pays air freight:** `chargeable_weight_kg = max(gross_weight_kg, volume_m3 × 166.67)` and `base_net = max(min_amount, chargeable_weight_kg × net_amount)` in the tariff currency. `sell_base_air = base_net × (1 + markup_rate)`. Only the payable base air-freight line receives the cost-based markup; payable FSC, SSC and fixed fees pass through at cost.
- The final BRL price is the sum of rounded sell lines (section 6). The resulting margin ratio `(final_price_brl - total_cost_brl) / final_price_brl` may be displayed for audit, but it is not the input used to calculate the sale price.

These v1.3 scope and arithmetic choices supersede this RFC's earlier proposal to apply margin to all costs. The exact rate and floor values, Incoterm allocation, FX conventions and synthetic golden quotes still need validation before the RFC becomes an ADR.

## 9. Open questions

The former question 1 is resolved: the MVP covers both imports and exports. The remaining question numbers are retained for continuity.

| # | Question | Owner | Default if unanswered |
|---|---|---|---|
| 2 | Quote excludes taxes and duties | Marcelo | Yes |
| 3 | Import buyer-side Incoterm proposal in section 4 matches the requesting customer's payable costs | Pilot companies | No final allocation until validated |
| 4 | FCL and AIR cost-based markup rates and the FCL `min_markup_usd` value | Marcelo, pilot companies | Freight-only markup is fixed by v1.3; values need confirmation |
| 5 | One recommended price, or several carrier options | Marcelo | One price plus alternatives in the audit view |
| 6 | Customs rate vs carrier rate for an applicable import CIF base | Pilot companies | Customs rate, flagged for confirmation |
| 7 | PTAX bulletin and side | Marcelo | Previous closing, selling side |
| 8 | Validate AIR chargeable-weight cases and rounding against pilot examples | Marcelo, pilot companies | `max(gross_weight_kg, volume_m3 × 166.67)` is fixed by v1.3 |
| 9 | Storage period assumption | Pilot companies | First period only |
| 10 | Add an insurance line | Marcelo | No; insurance is an input only |
| 11 | Half-up rounding | Marcelo | Half up |
| 12 | What makes a Hapag FCL offer or AIR tariff complete when origin and local charges come from another party? | Marcelo, pilot companies | `INCOMPLETE` if required inputs are missing; never combine offers or parties silently |
| 13 | How is a non-USD FCL offer converted before comparing its markup with `min_markup_usd`? | Pedro, Marcelo | Decide with the recorded FX rate before implementation |
| 14 | Which costs does the requesting exporter pay under each supported Incoterm? | Marcelo, pilot companies | No export matrix is assumed from the import proposal |
| 15 | Which export fees apply, how are they based, and are valid tariffs available? | Marcelo, pilot companies | No CIF or destination-terminal rule is assumed for exports |
| 16 | Which recorded FX rates convert export fees and any declared freight used only for valuation? | Pedro, Marcelo, pilot companies | Decide per applicable charge before implementation |

## 10. Impact

- **Data model and contracts (Pedro approval required):** use the proposed cost-based `pricing_rules.default_markup_rate` name and map FCL `min_markup_usd` to pricing rules; resolve migration from the old `default_target_margin` name before changing the schema or contracts. Add a pinned `carrier_offer_id` reference for validated Hapag inputs; retain `charge_category` and `charge_code` on tariffs, `inputs_hash` and line `flags` on quotes, and tariff versioning (`is_removed`, batch `activated_at`, quote `priced_at`) from RFC-002 section 6. Do not add `margin_scope` or `k` merely to preserve the retired global formula.
- **ADR-004:** the worker remains the only caller of the pure pricing package. When the customer owes FCL ocean freight, the `ai-engine` Hapag connector returns untrusted offer data that `core-api` validates and persists before stage 4 pricing; the exact Stage 4A sequencing still needs design review.
- **ADR-008:** money tests must cover FCL per-container floors, AIR minimum charges and the 166.67 factor, pass-through lines, and the exact line-sum invariant.
- **Connectors (RFC-003):** the Hapag API connector, terminal bands and FX inputs must expose validated, pin-able source data. RFC-003 records the v1.3 Hapag boundary; its public-source candidates remain draft.

## 11. Acceptance for moving to an ADR

The remaining Incoterm, export-fee and FX questions (especially 3, 6, 7 and 13–16) are answered, the markup values are confirmed, and ten historical quotes covering both import and export cases from the pilot companies are reproduced to the cent by a draft implementation against synthetic equivalents (ADR-008). The Hapag connector boundary remains ADR-001-compliant and its conditional Stage 4A sequencing is reviewed before implementation.
