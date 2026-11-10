# Plan — deterministic pricing engine (`internal/pricing`)

- **Spec:** `docs/specs/2026-11-10-pricing-engine-v1.3.md` (read first —
  every rule below resolves to it)
- **Branch:** `feat/core-pricing-engine` off `develop`
- **Scope:** `apps/core-api/internal/pricing/` + its tests only. No DB,
  no worker wiring, no migrations — the package is pure (rule 5).

## Constraints that apply while coding

- Files < 500 lines, functions 4–20 lines (split helpers).
- `shopspring/decimal` everywhere; **no `float64` in any signature or
  arithmetic** (assert via tests + review).
- No I/O imports in package code (`os`, `net`, `database/sql`, pgx).
  `time` is allowed only as `time.Time` values — the clock arrives as a
  parameter. Sort everything before it reaches `Output` — never emit map
  order.
- Errors: `error` return for invalid configuration/input (negative
  markup, unknown incoterm, missing currencies); `Output.Status =
  INCOMPLETE` + `Reasons` for data gaps.
- TDD: write each behavior's test first, watch it fail, implement.

## File layout

```
internal/pricing/
  types.go        — enums (Direction/Modal/Incoterm/Category/SourceType/
                    Status/Reason/Assumption), Money, FXRate
  input.go        — Input, TariffCandidate, FeeBand, CarrierOffer,
                    FixedFee + validate() (step 1 rejects)
  output.go       — Output, Line, Alternative, FXApplied, canonicalJSON()
  incoterm.go     — import matrix table + requiredGroups(direction,
                    incoterm, modal, terminalRequested)
  tariffs.go      — version resolution: natural-key grouping, validity
                    window vs QuoteDate, batch activated_at <= PricedAt,
                    latest-wins, tombstone drop
  freight.go      — chargeable kg, air base net, FCL ocean selection,
                    offer candidacy/completeness
  groups.go       — required-group line building (origin/customs/
                    surcharge/terminal bands/fixed fees)
  fx.go           — rate lookup by (source,ccy,date), carrier ×(1+s_org),
                    PTAX fallback + FX_FALLBACK, customs rate, STALE_FX
  round.go        — half-up cent rounding, aggregate-sell distribution +
                    cent allocation, totals
  price.go        — Price(): the 9-step orchestration, status/reasons,
                    assumptions, inputs_hash
  hash.go         — canonical serialization + sha256
  *_test.go       — one file per source file, plus golden_test.go
```

## Task order (executor-dispatchable)

1. **types/input/output skeleton + validate()** — enums, structs,
   validation rejects; tests: reject negatives, unknown enum, missing
   currency.
2. **incoterm matrix + requiredGroups** — table-driven; tests: every row
   of the §3 matrix incl. FCA note, DAP/DPU flag, DDP empty, sea-on-air
   and LCL/EXPORT incompletes.
3. **tariff version resolution** — grouping + latest-activated + window +
   tombstone; tests: newer batch wins, tombstone drops key, unactivated
   batch ignored, out-of-window ignored, deterministic tie-break.
4. **freight selection + offer candidacy** — chargeable kg, air base,
   FCL ocean from offer-or-tariff, completeness (Q12), OFFER_EXPIRED;
   tests per spec §4.4/§5.
5. **group lines (origin/customs/surcharge/terminal/fixed fees)** —
   payable matrix, fee-band match on CIF base, first-period assumption;
   tests: band pick, missing band, payable gating.
6. **fx resolution** — carrier ×(1+s), PTAX fallback flag, customs rate
   requirement, staleness flag; tests per §6.
7. **markup + rounding + cent allocation** — FCL formula + floor flag,
   AIR base markup, pass-through sell=cost, aggregate distribution;
   tests: floor binds, cent residual deterministic, Σ sells = price.
8. **Price() orchestration + hash + determinism** — steps in §4 order,
   reasons/assumptions sorted, canonical hash stable; tests: golden set
   from spec §10 + run-twice-identical + purity (no forbidden imports).

## Verification (acceptance)

```
cd apps/core-api
go test -count=1 -race ./internal/pricing/...
go vet ./internal/pricing/... && gofmt -l ./internal/pricing
go build ./...            # nothing else may break
```

Money-test bar: every numeric expectation is a decimal literal compared
with `.Equal` / `Cmp == 0`, never float tolerance. Determinism test
marshals `Output` twice and compares bytes.

## Review gates

Local `reviewer` pass on the full diff before the PR: purity, money
handling, matrix correctness vs RFC-001 §4/§6/§8, reason/flag coverage,
test quality (golden cases, deterministic ordering). Then PR → Devin
Review → fix rounds → Pedro merge decision.
