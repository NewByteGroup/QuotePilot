# Tech spec — staged quote pipeline (River jobs, `internal/jobs`, `internal/rfq`)

- **Status:** Draft for Pedro's review. Encodes the five decisions ratified on
  2026-11-11 (split extract/price, defer waiters, audited send via schema,
  auto-to-draft with human approval, raw retention per v1.3 model), his
  2026-11-12 calls (delivery table, TARIFF_UPDATE parking, approval roles,
  header retention, confidence) and his 2026-11-16 calls (4A/4B split, rate
  provenance, export parking). Items marked ☐ need his call before the PR
  they gate.
- **Date:** 2026-11-11
- **Author:** Devin (for Pedro)
- **Governs:** `apps/core-api/internal/jobs` (worker registry and per-stage
  workers), the `rfq_requests`/`quotes` state machines, the core→ai-engine
  call seams, R2 storage writes, and the Resend Receiving/Send clients.
- **Sources:** ADR-004 (staged River jobs — the governing document, including
  its 4A/4B amendment, decided by Pedro on 2026-11-16 — see S3a); ADR-001
  rules 2, 6, 8;
  ADR-002 (`InTenantTx`, library allow-list); ADR-003 (R2 `quotepilot-dev`);
  ADR-006 + RFC-004 (classifier + auditor contract); RFC-001 (deterministic
  pricing, pinned snapshots/FX); RFC-002 (email pipeline); migration 0002
  (the shipped v1.3 schema this spec binds to).

## 1. Purpose and hard rules

Turn a verified `inbound_messages` receipt into a rendered, human-approved,
customer-sent quote as a chain of small River jobs — each checkpointed, each
retryable without re-running earlier paid or non-deterministic work.

Rules that must never bend:

- **Job args carry IDs only** (`organization_id`, `correlation_id`, entity
  ids). `river_job` is not RLS-scoped; no email bodies, rates, or customer
  data in args (AGENTS.md rule 7, ADR-004).
- **Every tenant read/write opens `InTenantTx(organization_id)`** as the
  worker's first act. The exception is the Layer A outbox worker (no org).
- **`ai-engine` never receives `organization_id`** and holds no DB/storage
  credentials. Core fetches tenant context and passes content in the request.
- **Every ai-engine response is untrusted**: contract-schema validation plus
  sanity bounds in core before anything persists (rule 6).
- **Pricing is pure** (`internal/pricing`, decimal only, no I/O). The worker
  is its only caller in the MVP (ADR-004).
- **A stage with its checkpoint is a no-op.** Retries and replays never
  repeat completed work; the LLM extraction and the carrier-rate fetch are
  each paid once per RFQ.
- **Failures are loud.** A dead-lettered job flips the RFQ to `FAILED` with
  `failure_reason`; nothing waits silently. `FAILED` means a technical
  failure after retries; a business gap parks as `NEEDS_REVIEW` with a
  `review_reason` (S3b).

## 2. Entity map — what exists today

| Table | Role in pipeline |
|---|---|
| `inbound_messages` | Receipt written by the verified webhook (org resolved from `to:` alias, `resend_message_id`, subject, `received_at`). UNIQUE(org, resend_message_id) is the ingest idempotency key. **Not** the pipeline entity. |
| `rfq_requests` | Pipeline entity + checkpoints: `kind` (RFQ/TARIFF_UPDATE/OTHER), `status` machine, `raw_text_body`, `raw_html_body`, `thread_headers_json`, `extracted_payload_json`, `extraction_confidence`, `failure_reason`. |
| `rfq_attachments` | One row per attachment; `storage_key` → R2 under `tenants/{org}/rfqs/{rfq_id}/`; `parsed_markdown` feeds extraction. |
| `carrier_offers` | Validated carrier offer pinned before pricing (reproducibility, rule 9). |
| `quotes` | `version`, `quote_number` (off `org_quote_counters`), `priced_at`, `inputs_hash`, `carrier_offer_id`, `fx_rates_applied_json`, `overall_confidence`, `pricing_result_json`, `pdf_storage_key`, `approved_*`, `sent_outbound_message_id`. Status DRAFT→SUPERSEDED / APPROVED_SENT. |
| `private_tariffs`, `pricing_rules`, `public_snapshots` | Pricing inputs loaded inside the tenant tx. |
| `connectors`, `healing_sessions` | The repair topology a future waiter registry hangs off — deferred, see §7. |
| `tenant_audit_logs` | Approve/send audit trail. |

**Send audit is already in schema** (`quotes.sent_outbound_message_id` +
`tenant_audit_logs`). Pedro ratified deferring `outbound_deliveries` — it
becomes justified only when the product shows delivery state (delivered,
bounce, complaint). Two conditions he attached:

- The provider message id must always live in an **indexed** column on the
  quote version, never only inside audit JSON — so a later
  `outbound_deliveries` backfills with `INSERT ... SELECT`. TODAY
  `sent_outbound_message_id` is a plain `text` column with no index; adding
  `CREATE INDEX` is a schema change and rides the next approved migration.
- A bounce/delivery webhook arriving before the table exists becomes a
  `tenant_audit_logs` event and nothing more.

## 3. Job envelope, queues, idempotency

```go
// per-stage arg structs, each implementing river.JobArgs (Kind()).
type IngestArgs  struct { OrganizationID, CorrelationID, InboundMessageID uuid.UUID }
type ExtractArgs struct { OrganizationID, CorrelationID, RfqID uuid.UUID }
type RateArgs    struct { OrganizationID, CorrelationID, RfqID uuid.UUID } // stage 4A
type PriceArgs   struct { OrganizationID, CorrelationID, RfqID uuid.UUID } // stage 4B
type RenderArgs  struct { OrganizationID, CorrelationID, RfqID, QuoteID uuid.UUID }
type SendArgs    struct { OrganizationID, CorrelationID, QuoteID uuid.UUID }
```

- Each stage inserts the next with `InsertTx` inside the same tenant tx that
  writes its checkpoint — row and job commit or roll back together.
- Jobs are unique on `(kind, entity id)` via River's unique option — a double
  enqueue collapses.
- Queues (config, not code — ADR-004): `ingest`, `extract` (≈4 workers,
  LLM-bound), `price`, `render`, `send`. The existing `auth_mail_send`
  worker stays on `default`.

**Retry/timeout per stage** (decided — Pedro, 2026-11-16; ADR-004 open
item closed). General rule: **classify the error first** — transient
retries, deterministic cancels (`river.JobCancel`), so a pure-function
failure never burns five attempts.

| Stage | Attempts | Per-attempt timeout | Retries on | Window / notes |
|---|---|---|---|---|
| S1 `rfq_ingest` | 5 | — | transient | **Backoff must end ≈20–25 min total** — Resend `download_url`s expire ~1 h and a late attempt cannot re-fetch |
| S3a offer fetch | 3 | ~15 s | transient | `FETCH_FAILED` releases pricing anyway, so waiting longer only delays the quote |
| S2 `rfq_extract` | 3 | 120 s | transient only (timeout, 5xx, 429) | Contract-violating output: at most one retry, then `NEEDS_REVIEW` — each attempt costs money |
| S3b `rfq_price` | 5 | — | transient DB errors only | `INCOMPLETE` is not an error and never retries; deterministic errors discard via `JobCancel` |
| S4 `rfq_render` | 3 | — | transient | |
| S5 `rfq_send` | 5 | — | transient | Resend idempotency key makes re-sends safe; total window <1 h; exhausted ⇒ `FAILED`, no automatic resend |

## 4. Stages

### S1 `rfq_ingest` — receipt → RFQ row + raw content (queue `ingest`)

`InTenantTx`: read the `inbound_messages` receipt (id → args); fetch the full
message from Resend's Receiving API (`from`, RFC `message_id`, headers,
text/HTML bodies, attachment metadata + `download_url`s — note the urls
expire in ~1h, which is why fetch lives in a job, not the webhook); insert
`rfq_requests` (status `RECEIVED`, `resend_message_id` from the receipt,
`customer_email` ← `from`, bodies, `thread_headers_json`); download
attachments → R2 `tenants/{org}/rfqs/{rfq_id}/` + `rfq_attachments` rows;
set status `PARSING`, enqueue S2.

- UNIQUE(org, resend_message_id) on `rfq_requests` collapses replays: an
  existing RFQ for the receipt = no-op.
- ☐ Resend Receiving fetch is the one piece the webhook cannot do — a
  permanently-failed fetch (4xx) still creates the RFQ shell with
  `failure_reason` so nothing is invisible.

### S2 `rfq_extract` — classify + extract + confidence gate (queue `extract`)

`InTenantTx`: first, produce `parsed_markdown` for every attachment that
lacks it — attachment→text conversion is ai-engine's lane (same untrusted
rules): the worker calls its parser per attachment and writes
`parsed_markdown` on the row in the same tenant tx. A per-attachment parse
failure leaves the column NULL, is recorded in `tenant_audit_logs` and
degrades into the confidence gate — an attachment-only RFQ that yields no
usable text parks `NEEDS_REVIEW` (P2 review code) rather than paying an LLM
call on nothing.

Then, with bodies + `parsed_markdown` in hand: call ai-engine classify
(Jev, ADR-006) → `OTHER` ⇒ `IGNORED`, done; `TARIFF_UPDATE` ⇒ ☐ park as
`NEEDS_REVIEW` until the tariff-import path exists (RFC-002 open item);
`RFQ` ⇒ structured extraction call + auditor confidence (pplx-decider,
RFC-004) → validate against the extraction contract + sanity bounds → write
`extracted_payload_json`, `extraction_confidence` → confidence ≥ threshold
⇒ enqueue S3a (`PRICING`); below ⇒ `NEEDS_REVIEW` (human queue, fields
pre-filled — the gate is the triage). Invalid/uncontracted ai-engine output
⇒ `FAILED` with reason, never persisted.

- `TARIFF_UPDATE` parks as `NEEDS_REVIEW` (RFC-002: a tariff never enters
  pricing without a reviewed batch) — with Pedro's three requirements,
  because classification is untrusted model output and a real RFQ
  misclassified as TARIFF_UPDATE would otherwise park invisibly:
  1. a visible "needs review" list carrying the classification reason;
  2. a "reclassify as RFQ" action that re-enqueues the job by ID like any
     other pipeline step;
  3. an invariant test: nothing parked `NEEDS_REVIEW` via the tariff path
     reaches pricing.
- Threshold: a named constant with a comment pointing at RFC-004's open
  question; the code is threshold-agnostic. Pedro's two guarantees:
  (a) **structural** — no code path sends a quote without human approval
      regardless of the number, and that invariant gets a test, so a wrong
      threshold costs reviewer attention, never a wrong quote on the street;
  (b) **data from day one** — record each field's raw confidence and the
      reviewer outcome (accepted/corrected/rejected) so the evaluation set
      that answers RFC-004 comes from real decisions, not only synthetics.
      Green means "less to check", never "correct".

### S3a `rfq_fetch_rates` — carrier offer, conditional (queue `price`)

**Decided (Pedro, 2026-11-16): the carrier offer fetch is its own job with
its own checkpoint, separate from pricing (4B).** A pricing retry reuses the
persisted outcome and never calls the carrier again.

Only when the validated extraction says FCL and the requesting customer owes
ocean freight under the Incoterm does this job call the carrier connector —
with tenant context fetched by core, no `organization_id` sent. AIR and
freight-not-owed never reach the call; they checkpoint as `NOT_APPLICABLE` —
a row, not an absence, so retries and Reprocess can tell "fetch completed"
apart from "fetch never ran".

- **Terminal outcomes — every one releases pricing (enqueues S3b):**

  | Outcome | Meaning | What S3b receives |
  |---|---|---|
  | `OFFER_FOUND` | Contract-valid, sanity-bounded offer persisted | the pinned offer |
  | `NO_OFFER` | Carrier answered, nothing usable for the request | no offer |
  | `FETCH_FAILED` | Retries exhausted (transport, 5xx, invalid payload) | no offer |
  | `NOT_APPLICABLE` | No carrier call was owed (AIR, or the Incoterm puts ocean freight on the other party) | no offer |

  A carrier outage therefore never strands an RFQ: pricing runs on tariffs
  and the outcome travels into the quote's provenance (S3b). The outcome is
  the stage's checkpoint, so a replay of S3a is a no-op.
- **The persisted offer is a tenant row under RLS** — `app.carrier_offers`
  (ENABLE + FORCE RLS, shipped in migration 0002), written inside
  `InTenantTx` only after core validates the untrusted payload (rule 6).
- **Idempotency: unique key `(rfq_id, carrier, request_hash)`.**
  `request_hash` is a digest of the normalized request (route, equipment,
  container count, quote date) — no tenant content, and it never enters job
  args. A repeated fetch for the same request collapses onto the same row;
  a re-extraction that changes the request yields a new hash and a new
  offer.
- **P3 migration scope (Pedro, 2026-11-16) — extend `app.carrier_offers`,
  no new table.** One row per fetch attempt outcome:
  - `outcome` with the controlled values `OFFER_FOUND`, `NO_OFFER`,
    `FETCH_FAILED`, `NOT_APPLICABLE` — this row is the S3a checkpoint;
  - `request_hash`, and `UNIQUE (rfq_id, carrier, request_hash)`;
  - the offer-content columns become nullable (`offer_payload_json` is
    `NOT NULL` today; `offer_reference` and `expires_at` already are
    nullable), with a `CHECK` that an `OFFER_FOUND` row always carries
    offer content and `NO_OFFER`/`FETCH_FAILED`/`NOT_APPLICABLE` rows
    never do;
  - `source` with the values `LIVE` and `MIRROR` — which connector
    produced the row (see the mirror guards below);
  - ENABLE + FORCE RLS and the tenant policy stay exactly as shipped, and
    the table joins the ADR-008 isolation suite (pooled reuse A then B, no
    tenant context ⇒ 0 rows) in the same PR.
- **The connector sits behind an interface** owned by core (the worker
  depends on the interface, not on the Hapag client), so the demo can bind
  the mirror portal and tests bind a named fake. The real implementation
  stays the `ai-engine` Hapag connector (ADR-001); which one is bound is
  configuration. Two guards on the mirror (RFC-003: never used for real
  tenants' quotes):
  - **startup assertion, environment-level only (Pedro, 2026-11-16)** —
    `APP_ENV` is required at startup with the values `local`, `test`,
    `demo` or `production`; empty or any other value refuses to boot.
    `MIRROR_PORTAL_ENABLED=true` is refused when `APP_ENV=production`, and
    so is the dev mailer (`AUTH_DEV_MAILER=true`), which today is gated
    only by the Resend credentials being absent. There is no
    per-organization flag: the demo runs in a separate environment with
    its own database, so a real tenant never shares a process with the
    mirror. P3 boot tests: `APP_ENV` unset fails; `production` + mirror
    fails; `production` + dev mailer fails. P3 also adds `APP_ENV` to
    `.env.example` (name only).
  - **UI label** — every row written through the mirror has
    `source = MIRROR`. The UI labels that offer, and any quote priced from
    it, as **simulated** by reading the column — never by looking at the
    environment, so the label survives a database copy or export.
- **Connector order (pilot meeting, 2026-11-16).** **CMA CGM is the first
  real connector** — its API was already approved and Pedro tests it. The
  Hapag-Lloyd API was **not** granted (internal security restrictions; a
  retry via the Centauro/NewByte registration is possible), so S3a ships
  CMA-first and the interface keeps "Hapag" as one binding among others.
  The **Playwright fallback** validates the MVP while the paid aggregator
  (Essential Pack, ~10k calls) is negotiated down to ~100–500 calls — the
  portal credential doubles as the Playwright session. The interface
  already supports all three bindings (CMA API, Playwright, mirror).
- **Import tariff provenance (pilot meeting, 2026-11-16).** For imports,
  the Brazilian forwarder negotiates with the **overseas correspondent**
  (the selling agent), not directly with the carrier — the correspondent
  contract is what enables bookings. Tariff `source` rows record that
  counterpart, not just the carrier the service runs on.

### S3b `rfq_price` — deterministic pricing (queue `price`)

`InTenantTx`: load extraction + the S3a outcome and validated
`carrier_offers` row (when `OFFER_FOUND`) + `pricing_rules` +
`private_tariffs` + pinned `public_snapshots` + FX in one tx →
`internal/pricing` → `org_quote_counters` for `quote_number` → `quotes`
row (`DRAFT`, `version`, `priced_at`, `inputs_hash`, `fx_rates_applied_json`,
`overall_confidence`, `pricing_result_json`) + `quote_line_items` → enqueue
S4.

- **Rate provenance (Pedro, 2026-11-16).** Every quote shows where its
  freight rate came from and, when it fell back, why:
  - source — the freight line's `source_type` (`CARRIER_OFFER` vs
    `PRIVATE_TARIFF`) plus `carrier_offer_id`, both already in schema;
  - fallback reason, machine-readable, in `quotes.pricing_result_json`:
    **missing** (S3a `NO_OFFER` or `FETCH_FAILED`), **expired**, or
    **incomplete** (the offer does not cover every required carrier
    group). The worker composes it from the S3a outcome and the engine
    output — the engine already flags the replacement line `OFFER_EXPIRED`
    and emits warnings for expired/incomplete/unpriceable offers, but has
    no signal for an offer that was never supplied; that half comes from
    the S3a checkpoint.
  - **Invariant test (P3): a fallback is never silent.** For each of the
    three reasons, a quote whose customer owes FCL freight and whose
    freight line is tariff-sourced must carry a non-empty fallback reason;
    the test fails on a tariff-sourced line with none.
- **Export parks with a reason (Pedro, 2026-11-16).** `internal/pricing`
  keeps returning `INCOMPLETE{EXPORT_MATRIX_OPEN}` for `EXPORT` — its
  behavior is unchanged. The worker maps that to RFQ `NEEDS_REVIEW` with
  `review_reason = EXPORT_MATRIX_OPEN`, creates no quote, and keeps
  `extracted_payload_json` and the attachments intact so the analyst works
  from the extraction and nothing is re-extracted once export is
  supported. It is not `FAILED`: nothing broke; the export matrix is
  pending (see below), not the scope.
  - **Export is in the MVP — decided by Pedro with the pilot stakeholder
    on 2026-11-16** (supersedes the previous "pending Marcelo, default
    import-only" note). The parking above is temporary until the export
    matrix lands, not the end state: RFC-001 questions 13–16 (export
    Incoterm matrix, terminal/handling source and basis, tariff coverage
    and routes, quote currency and FX treatment) stay open with the pilot
    companies, and `internal/pricing` needs a real export path
    implemented once they are answered — a new engine deliverable, not
    only a review flag.
- **Missing or stale tariff parks the same way (Pedro, 2026-11-16).** The
  engine returns `INCOMPLETE`; the worker sets RFQ `NEEDS_REVIEW` with a
  distinct code — `MISSING_TARIFF` (no row for a required group) or
  `STALE_TARIFF` (rows exist but none is valid on the quote date) — creates
  no quote and keeps the extraction. The engine's own reason codes
  (`MISSING_TARIFF_GROUP:<group>`, …) are kept alongside for the audit
  view. The waiter registry that would re-run it automatically stays
  deferred (§7); until then the analyst fixes the tariff and uses
  Reprocess.
- **`review_reason` column (Pedro, 2026-11-16).** A new column on
  `rfq_requests` only — a parked RFQ has no quote row — added in **P2's**
  migration, because P2 is the first stage that parks (TARIFF_UPDATE, low
  confidence). It says why a human must look; `failure_reason` is **not**
  reused, because it means something broke.
  - Plain `text`, no `CHECK`: adding a code is a code change, not a
    migration.
  - The codes live as Go constants in one list. P3 adds
    `EXPORT_MATRIX_OPEN`, `MISSING_TARIFF` and `STALE_TARIFF`; P2 names
    its own codes for TARIFF_UPDATE and low confidence.
  - A real-Postgres test fails if any stored `review_reason` is not in
    that list, and writes go through the constants only — never a string
    literal at the call site.
- **`FAILED` is reserved for technical failures after retries** (transport,
  provider 5xx, storage, a dead-lettered job). A business gap — unsupported
  scope, missing data, low confidence — is always `NEEDS_REVIEW` with a
  `review_reason`.
- Recalculation is a new `version`, never an overwrite (rule 9).
- **Quote payload and validity rules (pilot meeting, 2026-11-16; business
  rules recorded in RFC-001 §9):**
  - **Original currency only.** Lines are presented in the currency the
    carrier charged (USD/EUR) — no BRL conversion, no PTAX shown. Totals
    are **per-currency groups**, so `pricing_result_json` groups the
    payload as international freight / local fees / service taxes; the
    single-BRL-total shape is superseded. Consequence flagged for the
    pricing engine update, not this spec alone.
  - **Service taxes only.** The taxes group carries ISS/CBS/IOF on the
    agency service; freight is untaxed repasse (RFC-001 §2).
  - **Negative margin is a hard block** — a proposal priced below cost is
    never sent. The check fires at approval *and* at send (defense in
    depth: the approving analyst sees it, and S5 refuses even if data
    changed between approval and send).
  - **Validity/SLA.** `expires_at` comes from the carrier; a spot rate is
    bound to its vessel. A quote whose validity ends in under ~30 minutes
    is not viable — it parks `NEEDS_REVIEW` (new review code, named in
    P3) rather than being sent. ~5 business days is the preferred window.
  - **`free_time`** (container free time) is a payload field on the
    commercial proposal.

### Reprocess — HTTP, synchronous (not a job)

`POST /rfqs/{id}/reprocess` under `Authenticate` + `ResolveTenant` +
`RequireRole(OWNER, ADMIN, PRICING_ANALYST)`: for an RFQ parked in
`NEEDS_REVIEW` (or `FAILED`), the tenant tx clears
`review_reason`/`failure_reason`, sets the status back to the stage being
re-run and enqueues that stage's job by ID with `InsertTx` — the same shape
as "reclassify as RFQ" (S2).

- **Resumes from the earliest missing checkpoint**, so it never repeats the
  paid extraction or a carrier fetch that already has an outcome row;
  re-extraction stays a separate, explicit request (ADR-004).
- **Minimum interval per RFQ (Pedro, 2026-11-16).**
  `REPROCESS_MIN_INTERVAL`, default 60s. A second reprocess of the same RFQ
  inside the interval returns 429 with a `Retry-After` header and enqueues
  nothing — a double click or a retry loop cannot fan out jobs or carrier
  calls. The last reprocess time is read from the audit log, so it needs
  no new column.
- **Audit log entry with the actor.** Every accepted reprocess writes a
  `tenant_audit_logs` row in the same tx: who (user id), which RFQ, the
  `review_reason`/`failure_reason` it cleared and the stage it resumed.

### S4 `rfq_render` — draft quote document (queue `render`)

`InTenantTx`: load quote + lines + org branding → ai-engine render
(Jinja2 + WeasyPrint) → PDF + email HTML → R2 `tenants/{org}/quotes/` →
`quotes.pdf_storage_key` → RFQ status `READY`. The pipeline **stops here
for a human** — the draft costs nothing to discard; a wrong price in a
customer's inbox is the expensive error.

### Approval — HTTP, synchronous (not a job)

`POST /quotes/{id}/approve` under `Authenticate` + `ResolveTenant` +
`RequireRole(OWNER, ADMIN, PRICING_ANALYST)` — approving a quote is the
analyst's central act (ADR-005's proposed matrix already grants it; no ADR
edit needed). Tariff-batch activation and margin-policy changes stay
OWNER/ADMIN-only: approval affects one quote, activation reprices every
future quote of the tenant. The same user **may** prepare and approve —
small tenants would deadlock otherwise — and `tenant_audit_logs` records
who did each step.

Tenant tx writes `approved_at`, `approved_by_user_id`, `approved_via` +
enqueues S5 with `InsertTx`. Re-approve of an already-approved version is a
no-op; approving supersedes nothing (versions only change on
recalculation).

### S5 `quote_send` — deliver + audit (queue `send`)

`InTenantTx`: load the approved quote (unapproved ⇒ refuse) → Resend send
with idempotency key `quote_id:version`, thread headers from the RFQ,
`outbound_from_name`/`outbound_reply_to` branding → `sent_outbound_message_id`
→ quote `APPROVED_SENT`, RFQ `APPROVED_SENT` → `tenant_audit_logs` entry.
Resend retry after a successful send collapses on the idempotency key; the
row is already APPROVED_SENT so a worker retry no-ops.

## 5. Status machine (exists in schema — pipeline drives it)

`RECEIVED` → `PARSING` → `PRICING` → `READY` → `APPROVED_SENT`, with
`NEEDS_REVIEW` (low confidence, tariff-update parking, and the
`review_reason` codes `EXPORT_MATRIX_OPEN`, `MISSING_TARIFF`,
`STALE_TARIFF`; future human edits), `FAILED` (technical failure after
retries, + `failure_reason`), `IGNORED` as reachable side/terminal states.
Reprocess moves a parked or failed RFQ back to the stage being re-run. Quote status
moves `DRAFT` → `APPROVED_SENT` (`SUPERSEDED` on recalculation). Every
transition is a worker/endpoint write inside `InTenantTx`; a test asserts
the allowed-transition set (ADR-004 consequence).

## 6. Raw retention

v1.3 already answers this: bodies persist in `rfq_requests.raw_*_body`,
attachments in R2 via `rfq_attachments.storage_key` — nothing is dropped.
Pedro ratified **skipping** the byte-exact `.eml` copy (bodies+attachments
already allow re-extraction; one more copy only grows the LGPD surface) —
with one reservation: headers must persist. `Message-ID`/`In-Reply-To`/
`References` are already carried (`rfc_message_id`, `thread_headers_json`);
SPF/DKIM results (the `Authentication-Results` header, RFC-002's hostile-
mail defense and "the client says they never received it" disputes) must
also be captured — into `thread_headers_json` if the shape fits, else a
dedicated column Pedro approves at the next migration.

## 7. Deferred (explicitly out of this spec's PRs)

- **Waiter registry** (`register_waiter`/`platform_take_waiters` + connector
  repair re-enqueue): needs a complete wake-up topology — register, persist,
  take, repair jobs, timeout/human escalation. Missing or stale tariffs
  park in `NEEDS_REVIEW` with a `review_reason` until then (S3b). The schema's `connectors`/`healing_sessions` half exists; the
  wake-up half does not.
- `outbound_deliveries` table (§2), SSE/`NOTIFY` fan-out to the UI (ADR-004
  decided the mechanism; the relay is its own PR), unmatched-mail quarantine
  table + counters, `inbound_email_alias` CHECKs, auto-send without approval.

## 8. Implementation slicing (one behavior per PR)

| PR | Contents | Depends on |
|---|---|---|
| P1 | Job envelope + S1 `rfq_ingest` (Resend Receiving client, R2 put, receipt→rfq promote) | R2 credentials config ☐ |
| P2 | S2 `rfq_extract` (ai-engine classify/extract seam, contract validation, confidence gate, `review_reason` parking) | extraction contract in `packages/contracts` ☐ Pedro approval; migration ☐ Pedro approval: `rfq_requests.review_reason` (plain text) |
| P3 | S3a `rfq_fetch_rates` (connector interface, three outcomes) + S3b `rfq_price` (provenance, `review_reason` parking for export and missing/stale tariff, never-silent-fallback test) + `POST /rfqs/{id}/reprocess` — one PR per behavior, the migration first | `internal/pricing` is implemented (import-only); migration ☐ Pedro approval: `carrier_offers` outcome/`request_hash`/unique key/`source`/nullable content + CHECK; offer contract in `packages/contracts` ☐ |
| P4 | S4 `rfq_render` → `READY` | render endpoint contract ☐ |
| P5 | `POST /quotes/{id}/approve` + S5 `quote_send` → `APPROVED_SENT` | — |
| P6+ | Deferred items above | — |

Each PR is its own branch off `develop`, ≤~500 lines non-generated,
real-Postgres integration tests for tenant-scoped writes, and the arg-scan
test (job args contain no forbidden fields — ADR-004 consequence).

## 9. Open items for Pedro

Decided, recorded in the sections above: `outbound_deliveries` deferred
(§2); TARIFF_UPDATE parking (S2); approval roles (Approval); no `.eml` copy,
headers kept (§6); conservative confidence constant with the
no-send-without-approval guarantee (S2); 4A/4B split, the `carrier_offers`
migration scope and mirror guards (S3a); rate provenance, export and
missing/stale-tariff parking, `review_reason` (P2 migration, Go
constants), `FAILED` reserved for technical failures (S3b); Reprocess
(roles, 60s interval, audit); `APP_ENV` required at startup (S3a);
retry/timeout table (§3). `internal/pricing` is implemented.

- ✓ **Index on `quotes.sent_outbound_message_id` (P5, approved
  2026-11-16)**: `UNIQUE` **partial** index `WHERE
  sent_outbound_message_id IS NOT NULL`, **not** prefixed by
  `organization_id` — provider ids are globally unique and uniqueness
  guards against double-send. Scope warning recorded: this does **not**
  enable MVP bounce handling — a bounce webhook arrives tenantless and
  `platform_*` functions cannot read RLS tables, so bounces will need a
  platform-side IDs-only registry (like the waiter registry), not a
  lookup on this column. `outbound_deliveries` stays deferred.
- ✓ **SPF/DKIM retention** — lives in `thread_headers_json` (implemented
  in P1); no dedicated column.
- ✓ **R2 bucket** — `quotepilot-dev` exists on the shared Cloudflare
  account; only the bucket-scoped S3 keypair is still needed in `.env`
  for the live attachment path (dev uses `QP_STORAGE_DIR` meanwhile).

- ✓ **Export scope** — decided 2026-11-16: export is in the MVP (S3b).
- ✓ **Pilot meeting 2026-11-16** — original-currency quotes, service-only
  taxes, payload groups and `free_time` (S3b + RFC-001 §9); negative
  margin hard block (S3b + S5); validity/SLA rules (S3b); CMA-first
  connector + Playwright fallback and correspondent tariff provenance
  (S3a); user+password login (ADR-005 amendment).

- ✓ **>95% confidence semantics** — confirmed 2026-11-16: high confidence
  means auto-*process* (extract, fetch, price without pausing), never
  auto-*send*. Human approval stays mandatory for every quote.

Still open:

- ☐ **Export matrix mechanics** — RFC-001 questions 13–15 with the pilot
  companies (16 resolved); when answered, `internal/pricing` gains a real
  export path and `EXPORT_MATRIX_OPEN` stops being emitted (S3b).
- ☐ Carrier offer wire contract in `packages/contracts` (P3).
