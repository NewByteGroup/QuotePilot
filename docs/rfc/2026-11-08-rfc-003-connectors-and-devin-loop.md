# RFC-003: Public connectors, health scoring, degraded mode and the Devin repair loop

- **Status:** Draft
- **Date:** 2026-11-08
- **Author:** Pedro (backend lead)
- **Needs input from:** the pilot companies (which carriers and terminals they use), Amanda (Firecrawl, Devin operations, demo mirror portal), Marcelo (what the Ops Console shows)
- **Becomes:** an ADR once sections 2, 6 and 9 are answered
- **Related:** ADR-001 (rules 5, 7, 8, 9), ADR-004 (River), ADR-005 (waiter registry), ADR-008 (connector tests), RFC-001 (how pricing consumes snapshots)

## 1. Scope

Layer C of the data model: connectors that turn public sources into immutable snapshots; how a snapshot is scored; what quotes do when a source is unhealthy; and how a broken parser is repaired by Devin without opening a path into tenant data.

## 2. Sources for the MVP

The team's guardrail is three or four connectors done well. Candidates:

| Connector | Source | Method | Notes |
|---|---|---|---|
| `bcb_ptax` | Central Bank open-data API (`olinda.bcb.gov.br`, service PTAX, OData, JSON) | Plain HTTP call, **no scraping** | Official and public, so Firecrawl is not needed. Publishes several bulletins a day. Endpoint and parameters to be re-checked when the connector is written. |
| `carrier_fx_daily` | Carrier or coloader pages that publish a daily rate | Firecrawl | **Open:** which carriers the pilot companies use, and whether those rates are public at all. If a carrier does not publish one, fall back to PTAX plus the organization's spread (RFC-001) or let the company enter its own rate. |
| Terminal fee tables | Santos Brasil, BTP, Portonave, TCP are named in the spec | Firecrawl, HTML or PDF | Pick the two or three the pilot companies actually use. Formats differ per terminal. |
| `demo_mirror_terminal` | A controlled replica of a terminal page | Firecrawl | For the live break-and-repair demo (section 8). Never used for real tenants' quotes. |

**Before any scraping:** check each site's terms of use and robots rules, and the licence of the content. Public does not automatically mean free to republish, and the repository is public (section 7). **Owner: Amanda, reviewed by Pedro.**

## 3. Connector contract

A connector is a Python module in `apps/ai-engine/connectors/<slug>/` with two parts: fetch (get the raw content) and parse (turn it into rates). Its output is a `ConnectorOutput` defined in `packages/contracts`:

- `connector_slug`, `parser_version`, `source_url`, `fetched_at`, `source_content_hash`
- `raw_content`: the fetched bytes, base64, size-capped by a configured limit; its SHA-256 must equal `source_content_hash`, which `core-api` verifies before storing it under the platform prefix. If the content exceeds the cap the connector fails its hard checks rather than truncating.
- `rates`: typed rows (kind, keys such as terminal and fee band, amount as a decimal string, currency, unit, validity if known)
- `diagnostics`: rows found, columns seen, structure signature, notes

Flow (ADR-004 style, all in the existing runtimes):

1. A River periodic job in `core-api` triggers `connector.run` on the connector's schedule.
2. `core-api` calls `ai-engine` at `/connectors/<slug>/run`. `ai-engine` fetches (with Firecrawl's key, which only it holds) and parses. It has no database or storage access (rule 1).
3. `core-api` receives the output, validates it (section 4), and only then stores the raw content under a platform storage prefix and writes the immutable `public_snapshots` row through the platform path (ADR-005).
4. Each quote line stores the `public_snapshot_id` it used (rule 9).

## 4. Deterministic health checks and `S_conf`

The spec's score `S_conf` must not come from an LLM's opinion of itself. It is computed in `core-api` from checks that can be tested:

| Check | Type |
|---|---|
| Output matches the schema | hard |
| Required fields and currencies are present and on the allow-list | hard |
| Row count inside the connector's expected range | hard |
| Amounts inside configured bounds, bands monotonic where they must be | hard |
| Structure signature (table headers or column set) equals the previous snapshot's | soft, weighted |
| Change against the previous snapshot per key inside a configured percentage | soft, weighted (a large jump is `SUSPECT`, not auto-accepted) |
| Date on the page matches the expected freshness | soft, weighted |
| Parser version and fixture suite pass | hard in CI, informational at runtime |

`S_conf` is the weighted share of checks passed, from 0 to 1. **A failed hard check forces `DEGRADED` regardless of the score.** A connector entering `DEGRADED`, whether through a failed hard check or `S_conf` below 0.95 (the spec's threshold), triggers healing. Weights and thresholds are configuration, calibrated on recorded fixtures, and reviewed when a connector is added.

Status machine: `HEALTHY` to `DEGRADED` (low score or hard fail), then `HEALING` while a repair is open, then `HEALTHY` after a repaired parser passes. A connector that exhausts its repair budget moves to `FAILED` and waits for a human. `FAILED` is an addition to the spec's three statuses (data model v1.2).

## 5. Degraded mode for quotes

Each source kind has a maximum acceptable age (proposal, **[business]** to confirm): carrier or PTAX rates, one business day; terminal fee tables, 30 days.

| Situation | What the quote does |
|---|---|
| Source healthy | Normal |
| Unhealthy, last good snapshot within max age | Use it; line flagged amber `STALE_SOURCE` with the snapshot age shown |
| Unhealthy and no snapshot within max age | Line unresolved; quote is `INCOMPLETE` and goes to `NEEDS_REVIEW`; the analyst can enter a `MANUAL_OVERRIDE` |

Whenever a quote is held back by a connector, pricing registers a waiter inside its tenant transaction (ADR-005 section 6). After the connector recovers, a platform job takes the waiters and enqueues one reprocess job per RFQ, each with its own tenant envelope. Reprocessing creates a new quote version.

## 6. The Devin repair loop

### 6.1 Trigger and inputs

A connector enters `DEGRADED`; a healing session is opened (`healing_sessions` row, platform scope). Devin needs the broken parser and evidence of what changed:

- **Proposed:** `core-api` commits a minimal excerpt of the failing page (the table region, not the whole page) to a branch `heal/<connector>-<timestamp>` as `tests/fixtures/<connector>/new`, next to the existing `old` fixture, then starts the Devin session on that branch.
- **Why a branch and not the prompt:** Devin cannot read our storage, and a fixture is exactly what the connector tests need.
- **Consequence to accept:** the repository is public, so the excerpt becomes public. It is public-source content, not tenant data, but it may carry copyright. Options: keep the excerpt minimal, use a private mirror of the repo for healing, or paste the excerpt into the session instead. **[decision] Pedro and Amanda.** Whether the Devin API accepts attachments of the size needed is **not verified**; check before choosing.

### 6.2 Guardrails (ADR-001 rules 8 and 9)

1. Devin may change only `apps/ai-engine/connectors/**` and `apps/ai-engine/tests/**`. Enforced three ways: CODEOWNERS, branch protection, and the `devin-scope` CI job that fails on any other path.
2. Its repository token can open pull requests only. It cannot push to `main` or `develop`, change settings, or read secrets, and its environment holds none.
3. Page content is **untrusted input to a model that can write code**. The prompt delimits it as data, and the controls that matter are structural, not textual: scope check, no new dependencies (CI), no network access in connector tests, and mandatory human review.
4. A pull request needs: the parser updated, the new fixture, the `pytest` suite passing in CI, and a human approval from Pedro or Amanda. **Nothing auto-merges.**
5. The repaired parser's output goes through the same checks as any output (section 4). A fix that passes tests but returns implausible numbers is still rejected.
6. Budget: at most N repair sessions per connector per day, with backoff, then `FAILED`. **N is a configuration value; proposal 3.**

### 6.3 After the merge

Redeploy `ai-engine` only (rule 8). Re-run the connector. If the checks pass, the connector returns to `HEALTHY` and the waiters are released (section 5). The Ops Console shows the session link, the parser diff, the `pytest` result and the number of quotes reprocessed.

## 7. Where public-source content may live

The repository is public. Fixtures hold recorded page excerpts, so each source's terms must permit that, or the fixture is replaced by a synthetic page with the same structure (ADR-008). The raw snapshots in object storage are private to the platform prefix.

## 8. Demo plan

- A static mirror of one terminal page, controlled by Amanda, with two layout versions. The `demo_mirror_terminal` connector is excluded from real tenants' pricing.
- Live: flip the mirror to the new layout, run the connector, show `DEGRADED`, open a real Devin session.
- A repair takes longer than a pitch slot, so a **recently finished session with a real pull request and a clean diff** is shown alongside it (the spec's own mitigation). Both links are kept ready before the final on 12/12.

## 9. Open questions

| # | Question | Owner | Default if unanswered |
|---|---|---|---|
| 1 | Which carriers and terminals do the four pilot companies actually use | Marcelo, pilot companies | Start with PTAX plus two terminals |
| 2 | Do those carriers publish daily rates publicly | Pilot companies | If not, PTAX plus spread, or tenant-entered rate |
| 3 | Terms of use and robots rules for each source | Amanda | No source goes live before this is checked |
| 4 | How failing content reaches Devin (branch, private mirror, or prompt) | Pedro, Amanda | Minimal excerpt on a branch, subject to question 3 |
| 5 | Max age per source kind | Pilot companies | 1 business day for rates, 30 days for terminal tables |
| 6 | Repair budget N | Pedro | 3 per connector per day |
| 7 | Firecrawl plan limits and schedule frequency | Amanda | Daily for rates, daily to weekly for terminals; verify credits |
| 8 | Devin API access and limits for the team | Amanda | Verify before the demo |

## 10. Impact

- **Data model v1.2:** `FAILED` connector status; `S_conf` details and check results stored per snapshot; waiters table (ADR-005); `parser_version` on snapshots; `healing_sessions.failing_snapshot_payload` replaced by a hash plus a reference (comment 15).
- **CI:** `devin-scope` and the connector fixture tests are required checks (already in `AGENTS.md`).
- **RFC-001:** consumes snapshots through the lookups defined there.

## 11. Acceptance for moving to an ADR

Questions 1 to 4 answered; `bcb_ptax` implemented end to end as the first connector (no scraping risk); one scraped connector passing its fixtures and the health checks.
