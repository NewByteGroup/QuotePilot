# ADR-004: Quote pipeline as staged async jobs on River (Postgres-native queue)

- **Status:** Approved (the open items below are Pedro's to close)
- **Date:** 2026-11-04
- **Deciders:** Pedro (backend lead). Reviewed and approved by Marcelo and Amanda.
- **Related:** ADR-001 (monorepo, Go/Python boundary), ADR-002 (Go core stack), ADR-003 (Postgres hosting), RFC-001 (pricing engine), RFC-002 (email pipeline)

## Context

Turning an incoming email into a priced quote involves slow, failure-prone I/O: fetching the email body and attachments from Resend, calling LLM-backed extraction in `ai-engine`, rendering a PDF, and sending the proposal. These steps take seconds to tens of seconds and fail partially.

Constraints that matter:

- The Resend `email.received` webhook carries metadata only and is retried if the endpoint is slow, so the handler must return fast and be idempotent.
- LLM extraction is non-deterministic and costs money. A later failure (for example PDF rendering) must not re-run it.
- Every job must run under tenant context (RLS), so the queue must carry the tenant envelope.
- Load is small (4 tenants). Operational simplicity beats throughput.
- MVP v1.3 covers FCL and AIR for imports and exports; LCL, Inland/Door-to-Door and AFRMM are outside this pipeline's MVP scope. A quote includes only costs payable by the requesting customer under the agreed Incoterm.

## Decision

**Use River, a Postgres-native job queue for Go, running in the `core-api` binary. Model the quote pipeline as staged jobs with checkpointed state, not one large job.**

### Why River

- Jobs live in the same Postgres. A job is enqueued in the **same transaction** that writes the data (`InsertTx`), so "row written but job lost" and "job queued for a row that rolled back" cannot happen.
- No Redis, no broker, one fewer thing to host.
- Built-in retries with backoff, per-queue concurrency limits, unique jobs, and a dead state for exhausted jobs.

### Job envelope and tenant context

- Every job carries `{ organization_id, correlation_id, payload }`.
- **Job arguments contain identifiers only** (for example `rfq_id`), never email bodies, rates or customer data. River's job table is not tenant-scoped by RLS, so it must not hold tenant content.
- The worker's first action is to open `InTenantTx(organization_id, ...)`. Application code never queries tenant tables outside it.
- The application database role has access to River's tables. River migrations run with the migration role, not the app role.

### Pipeline stages

**4A/4B split — accepted (Pedro, 2026-11-16).** Approved stage 4 is split into 4A (carrier offer fetch, its own job and checkpoint) and 4B (deterministic pricing). 4A ends in one of three persisted outcomes — `OFFER_FOUND`, `NO_OFFER`, or `FETCH_FAILED` after retries — and every one of them enqueues 4B, so a carrier outage never strands an RFQ and a pricing retry never calls the carrier again. The outcome is a tenant row under RLS in `app.carrier_offers`, idempotent on `(rfq_id, carrier, request_hash)`; the connector sits behind a core-owned interface. Details, the migration scope and the provenance rules are in `docs/specs/2026-11-11-staged-quote-pipeline.md` (S3a, S3b). The schema change and the offer wire contract in `packages/contracts` still need Pedro's approval at their PRs.

| Stage | Queue | Runs | Writes (checkpoint) |
|---|---|---|---|
| 1. Ingest | `ingest` | Fetch body and attachments from Resend, store attachments under `tenants/{org}/rfqs/{rfq}/` | `RECEIVED` |
| 2. Classify | `extract` | Call `ai-engine`: RFQ, tariff update, or other | `kind`; non-RFQ becomes `IGNORED` or a tariff-import path |
| 3. Extract | `extract` | Call `ai-engine`; validate response against the contract; compute confidence | `extracted_payload_json`, `PARSING` done |
| 4. Price (original baseline, replaced by 4A + 4B) | `price` | Load rules, tariffs, pinned public snapshots in one tenant tx; call the pricing package; persist quote and lines | `PRICING` done, quote `version = n`, `NEEDS_REVIEW` or `READY` |
| 4A. FetchCarrierRates (accepted 2026-11-16) | `price` | For FCL imports or exports, call the Hapag API connector in `ai-engine` only when the requesting customer owes ocean freight under the agreed Incoterm. `core-api` fetches tenant context in `InTenantTx` and passes it without `organization_id`; it validates the offer's schema and sanity bounds before tenant-scoped persistence. AIR makes no Hapag call. | Outcome row: `OFFER_FOUND` (validated offer), `NO_OFFER` or `FETCH_FAILED`; not applicable when no carrier call is owed. Every outcome releases 4B |
| 4B. Price (accepted 2026-11-16) | `price` | For imports and exports, load rules, tariffs, a validated carrier offer only when applicable, pinned public snapshots and FX in one tenant tx; call the deterministic pricing package; include only customer-payable costs. AIR base freight is included only when the requesting customer owes it under the agreed Incoterm. | `PRICING` done, quote `version = n`, `NEEDS_REVIEW` or `READY` |
| 5. Render | `render` | Call `ai-engine` to render PDF; store under `tenants/{org}/quotes/` | `pdf_storage_key` |
| 6. Approve | HTTP (sync) | Validate token or session, change state, enqueue the send job in the same tx | `APPROVED` |
| 7. Send | `send` | Resend send with idempotency key `quote_id:version` and thread headers | `APPROVED_SENT`, `sent_outbound_message_id` |

Each stage reads its input from the previous checkpoint, writes its own, and then enqueues the next stage. A stage that already has its checkpoint is a no-op, so a retry or replay never repeats earlier work. In particular, stage 3 (the LLM call) is never repeated unless a user explicitly requests re-extraction. Stage 4A is likewise not refetched on an ordinary retry. Its River arguments contain IDs only; `ai-engine` receives neither `organization_id` nor database or object-storage credentials.

### Pricing engine placement

- `internal/pricing` is a **pure package**: it takes a struct of inputs (rules, tariffs, snapshots, FX, extracted cargo data) and returns lines and totals. A validated carrier offer is an additional input when applicable. It performs **no I/O** and uses decimal arithmetic only.
- **In the MVP the worker (stage 4B) is the only caller.** Interactive recalculation after an analyst edits a line is also done by enqueuing a reprice job, with the UI updated through server-sent events. One execution path means one place to test and audit.
- A direct synchronous call from an HTTP handler can be added later without changing the package, if reprice latency is a problem in the demo.
- Recalculation never overwrites: it creates a new quote `version` and records the exact `public_snapshot_id`s and FX rates used. The quote also pins the exact validated carrier offer when applicable.

### Idempotency

- Webhook ingest: `UNIQUE (organization_id, resend_message_id)`. A duplicate delivery inserts nothing and enqueues nothing.
- Jobs are keyed by `(stage, entity_id)` using River's unique-job option, so double enqueues collapse.
- Outbound send uses a Resend idempotency key derived from `quote_id` and `version`.

### Failure handling

- Per-stage retries with exponential backoff and a timeout appropriate to the stage.
- After the final attempt the job is dead-lettered, the RFQ moves to `FAILED` with a reason, and it appears in the tenant's inbox. Failures must be visible, never silent.
- Cross-tenant work (for example reprocessing quotes that waited on a connector repair) fans out into **one job per waiting RFQ**, each with its own tenant envelope. There is no loop that spans tenants inside a job.

### Concurrency and backpressure

- `extract` is limited by the LLM provider's rate limit and by `ai-engine` capacity (start small, for example 4).
- `price` is cheap and CPU-bound; `render` is moderate; `send` is low.
- Queue limits are configuration, not code.

### Status updates to the UI

Stage transitions write the RFQ status and notify through Postgres `NOTIFY`. `core-api` relays tenant-scoped events to the browser over SSE. The channel payload carries only IDs; the client refetches through the normal tenant-checked API.

## Alternatives considered

1. **Do the work inside the HTTP request or webhook handler.** Rejected: webhook timeouts and retries, no isolation of partial failure, LLM latency blocks the request.
2. **One job per RFQ doing every step.** Rejected: a late failure re-runs the non-deterministic, paid LLM call.
3. **Redis plus a task library (asynq or similar).** Rejected: extra infrastructure and no transactional enqueue, which brings back dual-write bugs.
4. **Hand-rolled `FOR UPDATE SKIP LOCKED` queue.** Viable, but we would rebuild retries, uniqueness, scheduling and observability that River already provides.
5. **Workflow engine (Temporal).** Rejected as over-engineering for a 4-week MVP with 4 tenants.

## Consequences

**Positive**
- No lost or phantom jobs, thanks to transactional enqueue.
- Failures stay local to a stage and are visible, retries are cheap, and LLM cost is not repeated.
- Quotes are reproducible: pinned public snapshots and FX rates, with versioned recalculation; carrier offers are pinned too when applicable.
- One fewer service to run.

**Negative / risks**
- The queue shares the primary database. Mitigation: load is tiny; revisit only if job volume grows.
- River tables sit outside tenant RLS. Mitigation: IDs-only job args, enforced in code review and by a test that scans job args for forbidden fields.
- More states to keep consistent. Mitigation: a single explicit state machine for `rfq_requests.status` with tests for every allowed transition.

## Status mapping

`RECEIVED` (ingested) → `PARSING` (classify, extract) → `PRICING` (price) → `NEEDS_REVIEW` or `READY` (after render) → `APPROVED_SENT` (after send). `PRICING` conditionally fetches Hapag rates before deterministic pricing only when the requesting customer owes FCL ocean freight; AIR bypasses that fetch. Terminal and side states: `FAILED`, and `IGNORED` (new, for non-RFQ mail; see the data-model comments).

## Open items

- Exact retry counts, backoff and timeouts per stage (tune during the spike).
- 4A/4B sequence: accepted 2026-11-16 (see Pipeline stages). Still open under it: the exact Hapag offer wire contract in `packages/contracts` and the `app.carrier_offers` migration (outcome, `request_hash`, unique key), both needing Pedro's approval at their PRs.
- ~~AIR fallback for a missing or expired tariff.~~ **Resolved 2026-11-16:** there is no fallback price. The engine returns `INCOMPLETE` and the RFQ parks in `NEEDS_REVIEW` with `review_reason` `MISSING_TARIFF` or `STALE_TARIFF`; the analyst fixes the tariff and reprocesses. See `docs/specs/2026-11-11-staged-quote-pipeline.md`, S3b and Reprocess.
- ~~Whether tariff-update emails reuse this pipeline or get their own import path.~~ **Resolved 2026-11-12:** a `TARIFF_UPDATE` email never enters pricing; it parks in `NEEDS_REVIEW` with its classification reason and a "reclassify as RFQ" action, until the reviewed tariff-import path of RFC-002 exists. See the same spec, S2.
- Pooler compatibility of River's listen/notify with the chosen Postgres hosting (ADR-003: use a direct or session-mode connection for the worker).
