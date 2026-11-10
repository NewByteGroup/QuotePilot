# Tech spec — deterministic pricing engine (`internal/pricing`)

- **Status:** Draft for implementation (bounded by RFC-001 defaults; open
  business questions are encoded as flags/assumptions, not invented)
- **Date:** 2026-11-10
- **Author:** Devin (for Pedro)
- **Governs:** `apps/core-api/internal/pricing` — the only component that
  does money math (architecture rule 5)
- **Sources:** RFC-001 (`docs/rfc/2026-11-08-rfc-001-pricing-engine.md`),
  spec §4.4 equation (superseded parts noted), data-model spec
  `docs/specs/2026-11-10-data-model-v1.3.md` (schema column names),
  ADR-008 (money tests), ADR-004 (worker is the only caller)

## 1. Purpose and hard rules

The engine turns one extracted RFQ plus resolved org parameters, tariff
candidates, pinned snapshots and FX rates into the exact quote lines a
customer sees — deterministically. Rules that must never bend:

- **Pure package**: no I/O, no `time.Now`, no randomness, no maps whose
  iteration order can reach the output. Same input → same output, byte
  for byte. The clock is injected.
- **Decimal only** (`shopspring/decimal`). A float anywhere is a bug.
  Money lines: 2 decimal places; FX rates: 6 (`NUMERIC(10,6)`);
  percentages: 4 (`NUMERIC(5,4)`); tariff net amounts: 4 (`NUMERIC(14,4)`).
- **Never invents a number.** Any missing input a required line needs
  makes the result `INCOMPLETE` with a machine-readable reason — never a
  guess, never a fallback average.
- **Markup on cost only** (RFC-001 §8): markup applies to the eligible
  freight charge, nothing else. The spec §4.4 margin-on-price formula
  `P = C/(1−M)` is **superseded**; the schema keeps the column names
  `default_target_margin`/`route_target_margin` but both are markup
  rates on freight cost (RFC-001 §6 note; rename deferred, not a schema
  change here).
- Everything an analyst must audit is a **line**: each line carries its
  source id, original currency+amount, the FX rate applied, rounded cost
  BRL and sell BRL. Pass-through lines always have `sell = cost`.

## 2. API shape

```go
func Price(in Input, clock func() time.Time) (Output, error)
```

`error` is for **programmer/config errors** (negative markup, negative
cargo, unknown Incoterm/modal, nil clock) — the caller should 500/log.
`INCOMPLETE` inside `Output` is for **data gaps** — the caller should
route the RFQ to `NEEDS_REVIEW`.

### Input

```go
type Input struct {
    Direction   Direction // IMPORT | EXPORT
    Incoterm    Incoterm  // EXW FCA FAS FOB CFR CIF CPT CIP DAP DPU DDP
    Modal       Modal     // FCL | AIR (LCL arrives but is out of MVP)
    OriginLOCODE, DestLOCODE string
    EquipmentOrUnit        string // '40HC' etc; required for FCL/AIR, matched to tariff/offers
    ContainerCount         int    // FCL only
    GrossKg, VolumeM3      decimal.Decimal
    CargoValue    decimal.Decimal // goods value, feeds the import CIF base
    CargoCurrency string          // ISO-4217
    DeclaredFreight   *Money      // seller-paid freight for CIF-family base
    DeclaredInsurance *Money      // input only; no insurance line (Q10)
    TerminalRequested bool        // DAP/DPU: extraction says customer asked

    // Org parameters — already resolved route>modal>ALL by the worker.
    MarkupRate     decimal.Decimal  // resolved markup on freight cost
    MinMarkupUSD   *decimal.Decimal // FCL per-container floor, NULL ok
    FXSafetySpread decimal.Decimal  // s_org
    FixedFees      []FixedFee       // fixed_fees_json decoded upstream

    // Candidates for this route/modal/equipment — the worker applies only
    // the dumb filters (org, modal, locodes, equipment, valid window vs
    // QuoteDate). Version/tombstone resolution is engine logic below.
    Tariffs   []TariffCandidate
    Snapshots []FeeBand   // terminal/storage bands from pinned snapshots
    CarrierOffer *CarrierOffer // validated Hapag payload, FCL only

    FXRates    []FXRate  // keyed by (Source, Currency, Date)
    QuoteDate  time.Time // validity window + FX date basis
    PricedAt   time.Time // batch activation ceiling + inputs_hash
}
```

`Money{Amount decimal, Currency string}`; `TariffCandidate` mirrors
`private_tariffs` columns plus `BatchActivatedAt` (from the join) so the
engine needs no DB; `FeeBand{SnapshotID, Location, CIFMin, CIFMax,
WeightMinKg, VolumeMinM3, PeriodDays, Amount, Currency}`;
`FXRate{Source, Currency, Date, Rate}`;
`CarrierOffer{ID, Carrier, Charges []OfferCharge{Category, Code, Amount,
Currency, EquipmentOrUnit}, ExpiresAt}`.

### Output

```go
type Output struct {
    Status        Status      // COMPLETE | INCOMPLETE
    Reasons       []Reason    // machine codes, e.g. MISSING_TARIFF_GROUP
    Lines         []Line      // sorted: category asc, then charge_code
    Alternatives  []Line      // complete-but-not-cheapest carrier options
    TotalCostBRL  decimal.Decimal // Σ rounded line costs
    FinalPriceBRL decimal.Decimal // Σ rounded line sells — authoritative
    Warnings      []string
    Assumptions   []Assumption // every open-question default applied
    FXApplied     []FXApplied  // per (source, currency, date): rate+spread
    InputsHash    string       // sha256 hex of canonical input (§8)
}
type Line struct {
    Category   Category   // INTL_FREIGHT ORIGIN_CHARGES TERMINAL_STORAGE_THC
                          // CUSTOMS_DISPATCH SURCHARGE
    ChargeCode string     // 'OCEAN_FREIGHT', 'BAF', 'HBL_FEE' ...
    Description string
    SourceType SourceType // PRIVATE_TARIFF FIRECRAWL_PUBLIC ORG_FIXED_RULE
                          // CARRIER_OFFER
    PrivateTariffID, PublicSnapshotID, CarrierOfferID *uuid.UUID // pin
    Currency        string
    OriginalAmount  decimal.Decimal // source-currency amount pre-FX
    FXRateUsed      decimal.Decimal // rate incl. spread actually applied
    CostBRL, SellBRL decimal.Decimal // rounded half-up at conversion
    Flags           []string        // FX_FALLBACK STALE_FX MIN_MARKUP_APPLIED
}
```

## 3. Incoterm → required cost groups (import proposal, RFC-001 §4)

`Direction=EXPORT` → `INCOMPLETE{EXPORT_MATRIX_OPEN}` immediately
(Q13–16: export allocation is explicitly unvalidated — the engine must
not reuse the import matrix silently). `Direction=IMPORT`:

| Incoterm | ORIGIN_CHARGES | INTL_FREIGHT | TERMINAL_STORAGE_THC | CUSTOMS_DISPATCH |
|---|---|---|---|---|
| EXW | required | required | required | required |
| FCA | required* | required | required | required |
| FOB, FAS | — | required | required | required |
| CFR, CIF, CPT, CIP | — | — (declared input) | required | required |
| DAP, DPU | — | — | only if `TerminalRequested` | required |
| DDP | nothing — `COMPLETE`, zero lines, assumption `DDP_NOTHING_PAYABLE` | | | |

*FCA origin group = applicable origin terminal/loading fees; inland
pre-carriage stays out of MVP (any `PRE_CARRIAGE` code or `INLAND` as a
whole code token — `INLAND`, `INLAND_*`, `*_INLAND*`; `MAINLAND_*` does
not count — tariff row, offer charge or fixed fee — is skipped with a
warning; a fixed fee scoped to a group the matrix does not open was
never a candidate and is ignored silently).

Sea-only Incoterms (`FAS FOB CFR CIF`) with `Modal=AIR` → `INCOMPLETE
{SEA_INCOTERM_ON_AIR}` (the extractor also flags it upstream; the engine
double-checks because inputs are untrusted).

`Modal=LCL` (or anything not FCL/AIR) → `INCOMPLETE{MODAL_OUT_OF_MVP}`.

## 4. Calculation order (deterministic, single pass)

1. **Validate**: reject on programmer/config error — negative markup,
   negative cargo/weight/volume, `ContainerCount<1` for FCL,
   `ContainerCount>0` for AIR, missing currencies, empty
   `EquipmentOrUnit` for FCL/AIR, unsupported Incoterm.
2. **Chargeable quantity**: FCL → `ContainerCount`; AIR →
   `chargeable_kg = max(GrossKg, VolumeM3 × 166.67)` (fixed MVP factor).
3. **Version resolution (tariffs)**: group candidates by natural key
   `(carrier_or_agent, origin_locode, dest_locode, modal,
   equipment_or_unit, charge_category, charge_code)`; keep rows whose
   `valid_from <= QuoteDate <= valid_until` and
   `batch_activated_at <= PricedAt`; take the row from the batch with the
   **latest** `batch_activated_at` (tie → later `created_at`, then id);
   if that winner `is_removed`, the natural key is dropped. Deterministic
   ordering everywhere — never map iteration. The worker's candidate
   filters are untrusted (rule 6): before a row joins any pool the
   engine re-verifies modal, origin LOCODE, destination LOCODE and
   equipment/unit against the shipment (normalized). This spec defines
   **no wildcard convention** for tariff rows — an empty field on either
   side is not applicable (fail-closed, not even empty-vs-empty), unlike
   offer charges where an empty `equipment_or_unit` is documented as
   generic.
4. **Carrier-offer candidacy (FCL)**: if `CarrierOffer` present, check
   `ExpiresAt` — a zero or past `ExpiresAt` means expired (`OFFER_EXPIRED`
   flag on the replacement tariff lines; fail-closed because the offer
   payload is untrusted `ai-engine` output). Completeness follows the
   Q12 **strict default**: the offer is usable only if it covers **every
   required carrier group — freight always, and origin charges when the
   Incoterm matrix requires origin** (EXW/FCA); public terminal fees are
   carrier-independent and never required from the offer. An offer
   failing completeness is not usable — the engine falls back to the
   tariff path (warning + `OFFER_STRICT_COVERAGE` assumption; a
   `NO_USABLE_OFFER` reason fires only when nothing prices a group the
   offer could have covered). Offers never combine with another
   carrier's rows silently.
5. **Line building per required group** — cheapest **net cost** wins;
   non-winning complete options go to `Alternatives` (one price plus
   audit alternatives, Q5 default). A freight alternative is a
   **whole-option** price: its `SellBRL` is the freight aggregate the
   option would sell at if that losing row replaced the winner of its
   own charge code **inside its own source pool** — a tariff row swaps
   within the tariff pool's per-code winners, an offer charge within
   the offer's winners — never a hybrid of offer winners plus tariff
   rows (Q12 forbids silent cross-carrier combinations). A code the
   pool's winners do not cover adds its contribution to that pool's
   aggregate. `CostBRL` stays the losing row's own net; an alternative
   whose aggregate binds the floor carries `MIN_MARKUP_APPLIED` and the
   floor's FX flags like the winners. Pass-through groups keep
   standalone alternatives (sell = cost per line).
   - `INTL_FREIGHT`:
     - FCL owed → prefer `CarrierOffer` `OCEAN_FREIGHT` charge if usable,
       else tariff rows `charge_category='OCEAN_FREIGHT'`; none →
       `INCOMPLETE{MISSING_TARIFF_GROUP:INTL_FREIGHT}`.
     - FCL not owed (CIF family) → no line; `DeclaredFreight` feeds the
       CIF base only (§5 step 5 RFC-001: declared freight is an input,
       never a charge).
     - AIR owed → tariff rows `AIR_FREIGHT`; base
       `net = max(min_amount, chargeable_kg × net_amount)`; none →
       `INCOMPLETE{MISSING_TARIFF_GROUP:AIR_FREIGHT}`.
   - `ORIGIN_CHARGES`/`CUSTOMS_DISPATCH`/`SURCHARGE`: every required
     group collects its resolved tariff rows (`ORIGIN_CHARGE`,
     `CUSTOMS`, `SURCHARGE` categories) plus `FixedFees` whose scope
     matches the group; payable-only under the matrix.
   - `TERMINAL_STORAGE_THC` (import): pick `FeeBand` where
     `Location == DestLOCODE` (normalized) and
     `CIFMin <= CIFbase < CIFMax` with weight/volume bounds when set;
     first period only (Q9 assumption `STORAGE_FIRST_PERIOD_ONLY`); no
     matching band → `INCOMPLETE{MISSING_TERMINAL_BAND}`.
6. **CIF base (import only)**: `(CargoValue + freight + insurance)`;
   freight = payable freight net when owed, else `DeclaredFreight`;
   insurance = `DeclaredInsurance` or 0. Converted with the customs rate
   (§6); a zero-valued component is a known zero and resolves no rate.
   Absent cargo value but a CIF band is required →
   `INCOMPLETE{MISSING_CARGO_VALUE}`. When no freight is payable — the
   CIF family (CFR/CIF/CPT/CIP) and DAP/DPU with `TerminalRequested` —
   the seller-paid `DeclaredFreight` is a required input for the base:
   nil → `INCOMPLETE{MISSING_DECLARED_FREIGHT}` and band selection is
   skipped (a zero-valued `Money` is a known zero and stays valid).
7. **FX conversion**: every non-BRL line converts with its resolved rate
   (§6); `OriginalAmount` keeps the source amount.
8. **Rounding**: each `CostBRL` rounds half-up to the cent at conversion;
   sell per §7; `TotalCostBRL = Σ CostBRL`, `FinalPriceBRL = Σ SellBRL`.
9. **Status**: `INCOMPLETE` if any required group produced nothing or
   any hard-missing input; reasons are sorted, deduplicated; warnings
   and assumptions likewise.

## 5. Markup scope (RFC-001 §8, non-negotiable)

- FCL: sell for the ocean freight line =
  `Net_Ocean + max(Net_Ocean × MarkupRate, MinMarkupUSD × ContainerCount)`
  — the `max` comparison happens **in BRL** (the USD floor converts at
  the same carrier rate): algebraically identical for USD freight and the
  only coherent reading for non-USD lines. `MinMarkupUSD` NULL → floor
  is 0; a zero floor likewise — it can never bind, so it resolves no
  USD rate. When the floor binds, flag `MIN_MARKUP_APPLIED`.
  Within the tariff path the freight category competes **per charge
  code** like every other group — cheapest resolved row per code,
  distinct codes summed; an offer's freight charges are all additive.
- `SURCHARGE` lines are **emit-if-present**: a freight-owing route with
  no surcharge rows is not a data gap — the pool follows the freight
  payer's incoterm but is never `MISSING_TARIFF_GROUP`.
- AIR: freight sell = `Net_Air × (1 + MarkupRate)` on the base net
  (step 2/5). FSC/SSC/security surcharges and fixed fees are separate
  pass-through lines.
- **Every other line sells at its rounded cost** — `SellBRL = CostBRL`.

## 6. FX resolution (RFC-001 §7)

Lookup key `(Source, Currency, Date)`:

- Carrier-charged lines (freight, carrier surcharges): rate =
  `carrier_rate(currency, QuoteDate) × (1 + FXSafetySpread)`. If no
  carrier-published rate → `PTAX(prev_business_day, sell side) ×
  (1 + FXSafetySpread)` and flag the line `FX_FALLBACK`.
- Non-carrier lines (tariff/fixed/public fees): `PTAX(prev_business_day,
  sell)` — no spread (spread protects carrier-rate drift only).
- CIF goods value: `Source='CUSTOMS'` rate for `QuoteDate`; absent →
  `INCOMPLETE{MISSING_CUSTOMS_FX}` (Q6 default is "customs rate",
  recorded as assumption `CUSTOMS_RATE_UNCONFIRMED` when used).
- `Date` older than one business day vs `QuoteDate` → flag `STALE_FX`
  on the line; a stale customs rate has no line to carry the flag, so it
  surfaces as warning `CUSTOMS_FX_STALE`. Zero dates on FX rates or
  tariff validity windows are validation errors, not silent rows.
- `Currency='BRL'` → rate 1, no lookup.
- Every applied rate appends `{Source, Currency, Date, Rate, Spread}` to
  `FXApplied` (deduped by key) — maps to `fx_rates_applied_json`.

## 7. Rounding and cent allocation

- Half-up to the cent for every `CostBRL`/`SellBRL` — banker's rounding
  rejected (Q11 default, hand-checkable).
- `FinalPriceBRL = Σ SellBRL` exactly — the quote price is the sum of
  what the lines say, so audit always adds up.
- When a sell amount is computed on an **aggregate** (e.g. FCL markup
  over `Net_Ocean` spanning several container lines of the same charge
  code): compute the aggregate sell, then distribute proportionally to
  the contributing lines, round each half-up, and assign residual cents
  **one at a time in largest-remainder order** (each cent to the line
  with the largest remaining fractional part; ties → lowest line index).
  Pass-through lines are never adjusted.

## 8. `inputs_hash` — canonical input fingerprint

`sha256` hex over the canonical serialization of the whole `Input`:
JSON object, keys sorted, decimals rendered at the fixed scale of their
column (money 2, rates 6, percentages 4, tariff amounts 4), dates
RFC-3339 UTC, UUIDs lowercase, slices in input order. Implemented once
as `hashInput` and unit-tested for stability — this hash is what makes
"same quote twice" detectable and pins `quotes.inputs_hash`.

## 9. Flags, reasons, assumptions vocabulary

- **Line flags**: `FX_FALLBACK`, `STALE_FX`, `MIN_MARKUP_APPLIED`,
  `OFFER_EXPIRED` (on the tariff line that replaced an expired offer),
  `DECLARED_BASIS` (CIF base built from declared freight).
- **Incomplete reasons**: `MISSING_TARIFF_GROUP:<category>`,
  `MISSING_TERMINAL_BAND`, `MISSING_CARGO_VALUE`, `MISSING_CUSTOMS_FX`,
  `MISSING_DECLARED_FREIGHT`, `MISSING_FX:<currency>`, `NO_USABLE_OFFER`,
  `EXPORT_MATRIX_OPEN`, `SEA_INCOTERM_ON_AIR`, `MODAL_OUT_OF_MVP`.
- **Assumptions** (always recorded, never silent): import-only Incoterm
  matrix, `STORAGE_FIRST_PERIOD_ONLY`, `CUSTOMS_RATE_UNCONFIRMED`,
  `PTAX_PREV_BUSINESS_DAY_SELL`, `NO_INSURANCE_LINE`, `AIR_FACTOR_166_67`,
  `TAXES_EXCLUDED`.

## 10. Determinism & testing contract

- TDD per AGENTS.md — money tests are mandatory (ADR-008): write tests
  first. Every function gets a test; every bug gets a regression.
- Package must not import `database/sql`, `net/http`, pgx, or `time.Now`
  (clock injected); a guard test parses imports to assert purity.
- Golden cases (minimum): EXW FCL with offer+tariff alternative, FOB FCL
  tariff-only, CFR (freight excluded, declared freight feeds CIF), AIR
  with min_amount binding and volumetric weight winning, AIR with
  FX_FALLBACK, min-markup floor binding (flag), DDP empty quote, export
  RFQ → `EXPORT_MATRIX_OPEN`, tombstoned latest batch dropping a
  natural key, cent-allocation across split container lines.
- Byte-for-byte determinism: `Price` run twice on the same input must
  produce identical `Output` (compare marshaled canonical JSON).

## 11. Explicit non-goals (this spec does not solve)

- The worker/orchestration that builds `Input` (stage pipeline — later
  mission; the engine takes its input by value).
- Export Incoterm matrix, customs-rate source selection, insurance lines,
  storage period beyond the first, multi-carrier quote presentation —
  all `[business]`-flagged in RFC-001 §9; the engine records the
  corresponding `Assumptions`/`Reasons` instead of guessing.
- LCL W/M, inland pre-carriage, DDP charging.
- Embedding/similarity ranking — only codes filter candidates (spec §5.4
  warning); embeddings may rank descriptions upstream but never change
  which rows are valid.
