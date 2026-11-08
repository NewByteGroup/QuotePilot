# ADR / RFC plan and status (as of 2026-11-08)

ADR = a decision we make and record. RFC = a proposal that needs input from others before it becomes a decision.

## ADRs

| ID | Title | Status | Notes / blockers |
|---|---|---|---|
| ADR-001 | Modular monorepo: Go core + stateless Python ai-engine, contracts-first | **Drafted** (Proposed) | Needs team buy-in (Marcelo prefers Python). Reversible via contract. Wording to amend: contracts are OpenAPI 3.0.3, not 3.1 (see ADR-002). |
| ADR-002 | Go core stack, `InTenantTx` design, spike gate | **Drafted** (Proposed) | The allow-list of libraries that `AGENTS.md` refers to. The spike (Mission 3) decides Accepted versus the Python fallback. |
| ADR-003 | Postgres on Supabase: roles, pooler mode, extensions, storage, local dev | **Drafted** (Proposed) | Open: region and plan, account owner, connection limits. Local role model implemented in `infra/postgres/` (PR #2); departures from the first sketch (sequence grants, extensions, PUBLIC revokes, stand-in roles, EXECUTE revoke, search_path) are recorded in the ADR's Revision notes. ADR-003 owns the role model and the acceptance checks; ADR-007 owns the hosted provisioning runbook (roles, schemas, default privileges, search_path, extensions). |
| ADR-004 | Quote pipeline as staged River jobs; worker is the only caller of the pricing engine (MVP) | **Drafted** (Proposed) | Retry counts and per-stage timeouts to tune in the spike. |
| ADR-005 | Auth (magic link, JWT), tenant resolution, platform path with `SECURITY DEFINER` functions, waiter registry, 1-click approval token | **Drafted** (Proposed) | Role matrix to confirm with Marcelo. Resend retry behaviour for non-2xx to verify. |
| ADR-006 | AI agents architecture in `ai-engine` (decision model for routing and confidence, structured LLM extraction) | **Approved** (2026-11-07, owner Marcelo) | Written by the team. RFC-004 builds on it and suggests one wording change. |
| ADR-007 | CI/CD and environments: registry, dev/staging, deploy mechanism, secrets, IPv4/IPv6 path per environment, hosted DB provisioning (roles, schemas, default privileges, search_path, extensions) | **To write** | Written during Mission 2. Deploy jobs wait for it. Hosted DB provisioning depends on ADR-003 section 6. |
| ADR-008 | Testing strategy: schema lint, isolation tests, money tests, contract tests, evaluations | **Drafted** (Proposed) | Numbered 008 because 006 was taken. Needs the golden quotes from the pilot companies. |

## RFCs

| ID | Title | Status | Needs input from |
|---|---|---|---|
| RFC-001 | Pricing engine: Incoterm matrix, calculation order, rounding and cent allocation, margin scope, FX | **Drafted** | Marcelo and the pilot companies: margin scope (the main question), Incoterm matrix, customs rate, PTAX bulletin, conventions |
| RFC-002 | Email pipeline: inbound, outbound, sender identity, rendering location, hostile content, tariff-update review | **Drafted** | Amanda (Resend plan limits, rendering), Marcelo and pilot companies (forwarding test on real mailboxes, retention) |
| RFC-003 | Connectors and the Devin loop: sources, deterministic health score, degraded mode, repair guardrails, demo mirror | **Drafted** | Pilot companies (which carriers and terminals), Amanda (terms of use, Firecrawl and Devin limits) |
| RFC-004 | Extraction contract and confidence model, building on ADR-006 | **Drafted** | Marcelo (required fields, thresholds, evaluation set, vendor consent) |

## Other documents owed

- **Data model v1.2 amendments:** resolve the 17 Notion comments on sections 4 and 5 (kept locally, not in git) together with the schema changes listed in the "Impact" sections of RFC-001 to RFC-004 and ADR-005 into one migration plan.
- **Architecture diagram:** `diagrams/quotepilot-architecture.excalidraw` (4 frames).

## Open decisions

| Decision | Current default | Who decides |
|---|---|---|
| Go vs Python core | Go behind a 1-day spike; Python fallback | Pedro with team agreement |
| Margin on pass-through terminal costs (RFC-001) | Margin on everything, as in the spec | Marcelo and the pilot companies |
| Where PDF and email HTML render (RFC-002) | `ai-engine` (Jinja2 + HTML-to-PDF); drop React Email | Amanda and Marcelo |
| Outbound sender identity (RFC-002) | Shared verified domain with the company as display name and `Reply-To` | Amanda and Marcelo |
| Embedding provider | OpenAI text-embedding-3-small at 1024 dims, after a 30-minute eval on real aliases | Pedro and Marcelo |
| Consent to send tenant data to third-party APIs, including the decision-model vendor (RFC-004) | Unknown | Pilot companies |
| Wording of the prompt-injection claim in ADR-006 (RFC-004 section 7) | Suggest "limits the blast radius" | Marcelo |
| How failing page content reaches Devin, given a public repo (RFC-003) | Minimal excerpt on a branch, subject to terms of use | Pedro and Amanda |
| Supabase region, plan and account owner (ADR-003) | Closest region, team-controlled account | Pedro with the team |

## Suggested order

1. Team agrees on ADR-001, or the spike flips it to the Python fallback. Run the spike (ADR-002) early in the online phase.
2. Review the drafted ADRs (002, 003, 004, 005, 008) with Marcelo and Amanda; mark each Accepted, or record what changed.
3. Send the open-question tables of RFC-001 to RFC-004 to their owners. RFC-001 question 4 (margin scope) and RFC-002 question 1 (forwarding test on the real mailboxes) have the longest lead time because they depend on the pilot companies.
4. Write ADR-007 during Mission 2.
5. Turn each answered RFC into an ADR or fold it into an existing one, then write data model v1.2.
