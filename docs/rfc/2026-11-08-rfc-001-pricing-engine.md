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
2. The order of calculation: terminal fees depend on the CIF value, and the CIF value depends on the freight.
3. Origin charges and surcharges (GRI, BAF, peak season) appear as line categories but not in the equation.
4. Rounding: dividing each line by `(1 - M)` produces cent differences, and the lines must add up to the price exactly.
5. Whether the margin applies to pass-through terminal costs, and which exchange rate applies to which amount.

`internal/pricing` is a pure package (no I/O, decimal only). Everything below is what that package takes in and gives back.

## 2. Scope and assumptions

- **MVP is import into Brazil.** The spec's destination terminals, storage by CIF band, THC and customs dispatch all describe imports. Export quotes are out of scope. **[business] confirm.**
- Taxes and duties (II, IPI, ICMS, PIS/COFINS) are **not** part of the quote. The spec equation has none. **[business] confirm that the forwarder's proposal excludes them.**
- Sea (FCL, LCL) and air are in scope, matching the `modal` enum.
- All output is in BRL. Foreign-currency amounts are converted line by line and the rate is recorded.

## 3. Inputs and outputs

**Input struct** (built by the worker inside the tenant transaction, then passed by value):

| Input | Source |
|---|---|
| Extracted cargo: Incoterm, modal, origin and destination LOCODE, equipment or units, gross weight, volume, cargo value and currency, NCM | Stage 3 (RFC-004) |
| Organization rules, resolved by precedence route, then modal, then `ALL` | `pricing_rules` |
| Valid private tariffs for the route | `private_tariffs` |
| Pinned public snapshots (terminal fees, carrier FX) and the PTAX reference rate | Layer C, by `public_snapshot_id` |
| Quote date and an injected clock | the worker |

**Output struct:** lines (category, description, source type, referenced tariff or snapshot id, original currency and amount, FX rate used, cost BRL, sell BRL, flags), totals, `warnings`, the list of `assumptions` applied, a status of `COMPLETE` or `INCOMPLETE` with reasons, and an `inputs_hash`.

**Determinism:** the same input produces the same output, byte for byte. No map iteration order, no time reads, no randomness. This is what makes quotes reproducible (architecture rule 9) and replayable in tests.

## 4. Which cost groups the quote includes, by Incoterm

The forwarder quotes the **buyer's side**. Proposal, to be validated with the pilot companies (terminal fee allocation in particular varies in practice) **[business]**:

| Incoterm (2020) | Origin charges | International freight | Terminal storage and THC | Customs dispatch | Where the freight for the CIF base comes from |
|---|---|---|---|---|---|
| EXW | yes | yes | yes | yes | private tariff |
| FCA | per RFQ: yes only if pre-carriage or loading is requested | yes | yes | yes | private tariff |
| FOB (also FAS) | no (seller's) | yes | yes | yes | private tariff |
| CFR, CIF, CPT, CIP | no | **no** (seller pays; the declared freight is an input, not a charge) | yes | yes | declared in the RFQ or invoice |
| DAP, DPU | no | no | per RFQ | yes | declared |
| DDP | nothing to quote (seller clears) | | | | |

Notes:
- FAS, FOB, CFR and CIF are for sea and inland waterway only. The extractor flags them when paired with air (RFC-004).
- Under the CIF family the freight is paid by the seller but still belongs in the customs value, so it enters the calculation as a number from the documents and never as a charge line.
- Insurance for the CIF value is an input (declared, or zero). **[business] confirm whether to add an insurance line.**

## 5. Calculation order

1. **Validate and resolve.** Reject margins outside `[0, 1)`. Resolve rules by precedence route, modal, `ALL`. The data model enforces one rule per scope with `UNIQUE NULLS NOT DISTINCT` (comment 10).
2. **Chargeable quantity.**
   - FCL: number of equipment units.
   - LCL: chargeable W/M is the greater of weight in tonnes and volume in cubic metres (1 tonne = 1 m³ convention).
   - Air: chargeable weight is the greater of gross kg and volumetric kg, with volumetric kg = volume m³ × 167 (the usual 6000 cm³ per kg convention).
   **[business] confirm the conventions with the pilot companies.**
3. **Select tariffs.** Filter by route, modal, equipment and validity on the quote date (`valid_from <= date <= valid_until`). Filtering is deterministic on codes; embeddings only help rank service descriptions among already-valid rows (spec section 5.4 warning applies). If several carriers qualify, the default is: price the lowest net cost, list the others as alternatives in the audit view. **[business] one price or several options?** Missing tariff for a required group makes the result `INCOMPLETE`; the engine never invents a number.
4. **Freight and origin lines.** `quantity × net_amount`, never below `min_amount`, converted to BRL (section 7). Surcharges (BAF, GRI, peak season) are their own lines, identified by `charge_category` and `charge_code` on the tariff row (comment 11).
5. **CIF base in BRL.** `(goods value + freight + insurance)` where freight is the tariff freight (EXW, FCA, FOB) or the declared freight (CIF family). Converted at the customs rate (section 7).
6. **Terminal lines.** Look up the public fee band for the CIF base, weight and volume. Storage covers the first period only unless the RFQ says otherwise. **[business] confirm the storage period assumption.**
7. **Organization fixed fees.** From `fixed_fees_json`, converted if not in BRL.
8. **Cost total** is the sum of the rounded line costs (section 6), so what the analyst audits adds up.
9. **Margin and price** (section 6 and 8).

## 6. Rounding and cent allocation

- Money is `decimal`. BRL amounts have two decimal places; FX rates have six (`NUMERIC(10,6)`).
- Each cost line is rounded **half up** to the cent when it is converted to BRL. Banker's rounding is rejected because it surprises people who check by hand. **[business] confirm.**
- Price: `P = round_half_up( C / (1 - M) )` where `C` is the sum of rounded cost lines.
- Each line's unrounded sell amount is `cost_i / (1 - M)`. The cents of `P` are shared among the lines by the **largest remainder method** (floor each line to the cent, then give the leftover cents to the lines with the largest fractional parts, ties broken by line order). The result is exact: **the sum of line sell amounts equals `P`**. This is a mandatory invariant test (ADR-008).
- `M` is margin **on price**: `margin = (P - C) / P`. This is not markup. The interface labels it clearly, because markup and margin are easy to confuse.

## 7. Exchange rates

- A rate is looked up by `(source, currency, date)`. Carrier or coloader foreign-exchange rates ("dólar armador") come from the public connector; the PTAX reference comes from the Central Bank's official open-data API (see RFC-003).
- Freight and other carrier-charged amounts use the **carrier's** rate times `(1 + s_org)`, where `s_org` is the organization's protective spread. If the carrier has no published rate for the day, the engine falls back to PTAX plus `s_org` and flags the line `FX_FALLBACK` (amber).
- Which PTAX bulletin and side to use: proposal is the closing bulletin of the previous business day until today's is published, selling side for amounts the company pays out. **[business] confirm.**
- **Which rate converts the goods value for the CIF base is open.** Customs valuation in Brazil uses the customs exchange rate rather than the carrier's rate. **[business] confirm which rate the terminals and the pilot companies use for the fee bands.**
- A rate older than one business day is flagged `STALE_FX`. Every rate applied is stored in `fx_rates_applied_json` and per line in `fx_rate_used`.
- Recalculation always creates a new quote version with the then-current rates. Old versions are never changed.

## 8. Margin scope

The spec equation applies `M` to the entire cost, terminal pass-through fees included. That may overprice items the company merely passes on.

| Option | Formula | Effect |
|---|---|---|
| **A. Margin on everything** (spec as written) | `P = C / (1 - M)` | Simple. Terminal fees are marked up. |
| B. Margin on the forwarder's own services only | `P = C_pass + C_services / (1 - M)` | Pass-through lines are sold at cost. |
| C. Separate pass-through markup | `P = C_pass × (1 + k) + C_services / (1 - M)` | Most flexible, one more parameter to explain. |

**Default until the business answers: A.** If B or C is chosen, the rule becomes `pricing_rules.margin_scope` (and `k`), a schema change for data model v1.2. **[business] This is the main question for Marcelo and the pilot companies.**

## 9. Open questions

| # | Question | Owner | Default if unanswered |
|---|---|---|---|
| 1 | MVP is import-only | Marcelo | Yes |
| 2 | Quote excludes taxes and duties | Marcelo | Yes |
| 3 | Incoterm matrix in section 4 matches pilot practice | Pilot companies | As proposed |
| 4 | Margin scope: A, B or C | Marcelo, pilot companies | A |
| 5 | One recommended price, or several carrier options | Marcelo | One price plus alternatives in the audit view |
| 6 | Customs rate vs carrier rate for the CIF base | Pilot companies | Customs rate, flagged for confirmation |
| 7 | PTAX bulletin and side | Marcelo | Previous closing, selling side |
| 8 | W/M and air volumetric conventions | Pilot companies | 1 t = 1 m³; 167 kg per m³ |
| 9 | Storage period assumption | Pilot companies | First period only |
| 10 | Add an insurance line | Marcelo | No; insurance is an input only |
| 11 | Half-up rounding | Marcelo | Half up |

## 10. Impact

- **Data model v1.2:** `charge_category` and `charge_code` on tariffs; `margin_scope` (and `k`) on pricing rules; `inputs_hash` and line `flags` on quotes; a rounding rule note in the schema comments.
- **ADR-004:** unchanged. The worker remains the only caller.
- **ADR-008:** the money tests in that ADR implement sections 5 and 6.
- **Connectors (RFC-003):** terminal bands and carrier FX must be exposed in a shape the lookups above can use.

## 11. Acceptance for moving to an ADR

Questions 3, 4, 6 and 7 answered, and ten historical quotes from the pilot companies reproduced to the cent by a draft implementation against synthetic equivalents (ADR-008).
