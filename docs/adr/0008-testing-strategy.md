# ADR-008: Testing strategy: isolation, money, schema lint, contracts and evaluations

- **Status:** Proposed
- **Date:** 2026-11-08
- **Deciders:** Pedro (backend lead). Review requested from Marcelo and Amanda.
- **Related:** ADR-001 (boundary rules), ADR-002 (stack), ADR-003 (roles), ADR-004 (pipeline), ADR-005 (platform path), ADR-006 (AI agents), ADR-007 (CI/CD, to write), RFC-001 (pricing), RFC-004 (extraction contract)
- **Numbering note:** `docs/adr-rfc-plan.md` first listed this decision as ADR-006. That number was taken by the approved AI agents ADR, so testing is ADR-008. ADR-007 stays reserved for CI/CD and environments.

## Context

The product's risk is a wrong or leaked number, not throughput. Three failure classes matter most:

1. **Tenant leakage.** Net rates, customers and margins are trade secrets. RLS is the barrier, and a mock database proves nothing about it.
2. **Wrong money.** Totals that do not add up, rounding drift, a float that slipped in, or a rate applied in the wrong currency.
3. **Untrusted input.** Emails, scraped pages and LLM output all feed the system.

The repository is **public**. Whatever the tests use must be safe to publish. The pilot companies' real tariffs, customers and emails never enter the repo.

`AGENTS.md` already fixes the commands (`make test`, `make test-ai`, `make test-web`, `make test-integration`, `make ci`), the CI job names, and the TDD scope. This ADR says what each layer must prove and where it runs.

## Decision

### 1. Layers

| # | Layer | What it proves | Runs in | Required |
|---|---|---|---|---|
| 1 | Unit | Pure logic: validators, state machine transitions, parsing helpers | `make test` (`go test -race -short`), CI `test` | Always |
| 2 | Money | The pricing package (section 2) | `make test`, CI `test` | Always for `internal/pricing` |
| 3 | Role and schema checks | Role model of ADR-003, RLS present on every tenant table, platform-table allow-list (section 3) | `make db-check`, `make db-check-negative`, CI `integration-test` | Any change touching migrations or roles |
| 4 | Isolation suite | Tenant data cannot cross, through every access path (section 4) | `make test-integration`, CI `integration-test` | Any change touching DB, RLS, tenancy, pricing or the quote state machine |
| 5 | Migration tests | History applies from scratch, is linear, and never drops a tenant policy | CI `migrations` | Always |
| 6 | Contract tests | Shapes agree across Go, Python and TypeScript; hostile payloads are rejected (section 5) | CI `contracts-drift`, `test` | Always |
| 7 | Connector tests | Parsers still read old and new fixtures; sanity checks fire | `make test-ai`, CI `test` | Any connector change (Devin PRs included) |
| 8 | Evaluations | Agent quality: classification, extraction, evidence (section 6) | `make eval` (live, manual), replay mode in CI | Before tuning a prompt or changing a model |
| 9 | Privacy canary | No email body, rate or margin reaches logs or job args (section 7) | CI `integration-test` | Always |
| 10 | Frontend | Component behaviour (vitest); one demo-path smoke test | `make test-web`, CI `test` | Behaviour changes |
| 11 | Security scans | Known vulnerabilities, secrets, image findings | CI `security` | Always |

Every PR still records the exact commands and output in its description (`AGENTS.md`, workflow step 5).

### 2. Money tests

For `internal/pricing`, which is pure and decimal-only (ADR-004):

- **Invariant: the sum of line `sell_amount_brl` equals `final_price_brl`, exactly.** Checked by property-based tests over generated inputs (Go's built-in fuzzing or `testing/quick`), plus table tests for the cases RFC-001 will define (rounding allocation, minimums, per-unit charges, expired tariffs).
- Totals never go negative; a higher margin never lowers the price; converting a line and summing in a different order gives the same total.
- **Golden vectors:** at least ten historical quotes from the pilot companies, supplied by Marcelo, with the expected totals. Stored as synthetic values with the same structure, not the real figures, because the repo is public. The real set stays on the team's private storage and is run locally.
- **No floats.** A test parses the package source with `go/parser` and fails if `float32` or `float64` appears in `internal/pricing`. This catches a mistake that no behavioural test would.
- Recalculation creates a new version and never mutates the old one; replaying a quote with the recorded snapshot ids and FX rates reproduces the same numbers.

### 3. Schema lint and role checks

Run against the real compose database as part of `integration-test`:

- Every table in `app` with an `organization_id` column has RLS enabled **and** forced, has at least one policy, and the policy text uses the `NULLIF(current_setting('app.current_organization_id', true), '')::uuid` form. A table that fails breaks the build (Notion comment 17a).
- The set of tables **without** RLS in `app` equals an explicit allow-list (the platform tables of ADR-005). A new unprotected table fails until someone edits the list on purpose.
- `qp_app` holds no privilege on platform tables, no `UPDATE` or `DELETE` on audit tables, and no write on public-snapshot tables.
- The `anon`, `authenticated` and `service_role` grants query from ADR-003 section 2 returns no rows.
- The existing `infra/postgres/checks/roles.sql` and its negative tests stay the first line: each assertion is proven to fail when its property is broken.

### 4. Isolation suite

A dedicated Go test package, run with the real `qp_app` role against Postgres, never against a mock:

1. **Pooled reuse:** use a connection for org A, return it, borrow it for org B, read: nothing of A is visible.
2. **Identical vectors:** rows with the same embedding in A and B; a similarity query with **no** application `WHERE` returns zero rows of the other org.
3. **No context:** a query with no tenant set returns zero rows or a clean error, and never an `''::uuid` failure.
4. **Writes:** inserting a row with another org's id is rejected by `WITH CHECK`.
5. **Platform functions:** each `SECURITY DEFINER` function of ADR-005 works for `qp_app`, returns only its documented columns, and `qp_app` cannot read the underlying table directly.
6. **Waiter registry:** a tenant transaction cannot register a waiter for another organization.
7. **Storage keys:** the presign helper refuses a key outside the caller's `tenants/{organization_id}/` prefix, including `..` and encoded variants.
8. **Jobs:** River job args contain only identifiers (a test scans enqueued args for forbidden fields, ADR-004), and a worker with a wrong or missing envelope does nothing.
9. **Membership:** a user removed from an organization is refused on the next request.

`make test-isolation` runs this package and prints a plain-language table of what was proved. It is also the demo asset: a live proof that one tenant cannot see another.

### 5. Contract tests

- `contracts-drift` regenerates Go, Pydantic and TypeScript from `packages/contracts` and `sqlc` output and fails on any diff.
- A shared directory of **example payloads** (valid and hostile) is validated by both the Go and the Python test suites, so the two sides cannot drift in what they accept.
- Core-side validation of `ai-engine` responses (ADR-001 rule 5; ADR-006): tests feed malformed, out-of-range, oversized and well-formed-but-wrong responses (unknown LOCODE, weight of zero, `valid_from` after `valid_until`, a quote string that is not in the source text) and assert nothing is persisted.
- Webhook tests use recorded Resend payloads with valid and invalid Svix signatures, stale timestamps and duplicate deliveries.

### 6. Evaluations for the AI agents

Unit tests do not tell you whether a prompt is good. Per the TDD policy in `AGENTS.md`, the **evaluation set is written before a prompt is tuned**.

- A golden set of RFQs and tariff emails with expected classification and extracted fields. The in-repo set is **synthetic or fully anonymised**; real pilot emails stay private.
- Metrics: classification confusion matrix, per-field accuracy, exact-match rate on `origin_locode` and `dest_locode`, and the rate at which each extracted `exact_quote` is a literal substring of the source text.
- CI runs the set in **replay mode** from recorded model outputs, with no network, so it is fast and free. `make eval` runs against the live models, is manual, and has a spend cap. CI never calls a live LLM.
- Calibration check for the confidence model: the share of fields marked full confidence that are actually correct must meet the threshold set in RFC-004. A model or vendor change reruns the set before merge.

### 7. Privacy canary

Insert a unique canary string into an email body and into an attachment, run the full pipeline against fakes, then scan captured logs, River job args and `ai-engine` access logs. The canary must appear nowhere except the intended tenant rows. This is the test behind "never log email bodies, attachments, rates or margins" and "`ai-engine` does not log request payloads".

### 8. Test data and doubles

- **No real customer, tariff or email data in the repository.** Fixtures are synthetic or scrubbed, including the connector fixtures under `apps/ai-engine/tests/fixtures/<connector>/{old,new}`. Public pages are fine to record; their licence is checked per source (RFC-003).
- External services (Resend, Firecrawl, Devin, LLM and embedding providers, object storage) are replaced by **named fake classes**, not inline stubs.
- Time and randomness are injected, so tests do not depend on the clock.
- A flaky test is quarantined with a linked issue within a day. A skipped test names its issue in the skip reason.

### 9. Coverage and speed

No coverage percentage gate. Coverage is published as a CI artifact (`AGENTS.md`) and reviewed on risky paths. What is mandatory is the set of suites in the table above for the kind of change. `make test` stays fast (`-short` skips the database); the integration job is allowed to be slower.

### 10. Demo-path smoke test

One end-to-end test drives the compose stack through the happy path: an inbound-email fixture arrives, the pipeline produces a quote, an analyst approves it, and the send is captured by a fake mailer. It runs on demand and nightly first, and becomes a required check only if it proves stable.

## Alternatives considered

1. **Mock the database in tenant tests.** Faster, and it proves nothing about RLS. Rejected: `AGENTS.md` already forbids it for isolation tests.
2. **A coverage percentage gate.** Encourages tests that touch lines instead of proving properties. Rejected in favour of mandatory suites per change type.
3. **Live LLM calls in CI.** Non-deterministic, slow and billable. Rejected; replay in CI, live by hand.
4. **`testcontainers-go` per test.** Isolated, but it skips the role and schema model the suite exists to verify (ADR-002).
5. **Real pilot data as fixtures.** Most realistic, and a leak in a public repo. Rejected.

## Consequences

**Positive**
- The isolation proof is automated, repeatable and presentable.
- The properties that matter most (no float, lines sum to price, RLS on every table) are checked mechanically, not by review.
- Public-repo safety is a stated rule, not a habit.

**Negative / risks**
- Synthetic golden vectors can miss real-world oddities. Mitigation: the real set runs locally and its failures become new synthetic cases.
- The `integration-test` job grows with each suite. Mitigation: keep the database tests focused, parallelise per package.
- Replay-mode evals go stale when prompts change. Mitigation: re-record on purpose, in the PR that changes the prompt.

## Open items

- The ten golden quotes (Marcelo and the pilot companies), and a place to keep the private set.
- Size of the evaluation set and the confidence threshold (RFC-004).
- Whether the smoke test becomes a required check (decide after it has run for a week).
- Property-test tooling in Go (`testing/quick` or fuzzing) once the first money tests exist.
