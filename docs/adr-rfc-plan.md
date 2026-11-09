# ADR / RFC plan and status (as of 2026-11-09)

ADR = a decision we make and record. RFC = a proposal that needs input from others before it becomes a decision.

## MVP v1.3 scope

The planned import-and-export quote MVP covers FCL and AIR for costs payable by the requesting customer under the Incoterm. FCL uses the Hapag-Lloyd API only when that customer owes ocean freight; AIR uses valid tariffs when the customer owes air freight. Both use cost-based markup on payable base freight only; FCL has a minimum USD markup per container. LCL, Inland/Door-to-Door and AFRMM are outside this scope. RFC-001 and ADR-004 track the remaining pricing and pipeline design details.

## ADRs

| ID | Title | Status | Notes / blockers |
|---|---|---|---|
| ADR-001 | Modular monorepo: Go core + stateless Python ai-engine, contracts-first | **Approved** (open items remain) | OpenAPI 3.0.3 per ADR-002; the Go spike/fallback gate remains in ADR-002. The v1.3 Hapag API integration stays in a Python `ai-engine` connector; `core-api` validates, persists and prices. Renderer/template details (RFC-002) and service auth remain open. |
| ADR-002 | Go core stack, `InTenantTx` design, spike gate | **Drafted** (Proposed) | The allow-list of libraries that `AGENTS.md` refers to. The spike (Mission 3) decides Accepted versus the Python fallback. |
| ADR-003 | Postgres on Railway (dev) and Supabase (prod), Cloudflare R2 storage | **On-Hold** | Product spec v1.2 section 4.5 fixes the provider topology: Railway Postgres with pgvector for dev, Supabase `quotepilot-prod` in `sa-east-1` for prod, and R2 buckets `quotepilot-dev`/`quotepilot-prod`. Local role model was implemented in `infra/postgres/` (PR #2). Account, plan, connection limits, hosted provisioning and CI checks remain open; ADR-007 owns the runbook and deploy mechanism. |
| ADR-004 | Quote pipeline as staged River jobs; worker is the only caller of the pricing engine (MVP) | **Approved** (open items remain) | Retry counts and per-stage timeouts to tune in the spike; tariff-update path and River pooler compatibility remain open. |
| ADR-005 | Auth (magic link, JWT), tenant resolution, platform path with `SECURITY DEFINER` functions, waiter registry, 1-click approval token | **Drafted** (Proposed) | Role matrix to confirm with Marcelo. Resend retry behaviour for non-2xx to verify. |
| ADR-006 | AI agents architecture in `ai-engine` (decision model for routing and confidence, structured LLM extraction) | **Approved** (2026-11-07, owner Marcelo; model matrix updated 2026-11-09) | Jev classifies; `perplexity/pplx-decider-v1.2` audits confidence; `qwen/qwen3-embedding-8b` produces 1024d embeddings. RFC-004 builds on it and suggests one wording change. |
| ADR-007 | CI/CD and environments: registry, dev/staging, deploy mechanism, secrets, IPv4/IPv6 path per environment, hosted DB provisioning (roles, schemas, default privileges, search_path, extensions) | **To write** | Product spec v1.2 fixes Railway services and database in dev, Railway services and Supabase database in prod, Cloudflare Pages frontend and R2 storage. ADR-007 must reconcile this with `AGENTS.md`'s staging-on-`main` CI plan and define deployment and hosted provisioning; deploy jobs wait for it. |
| ADR-008 | Testing strategy: schema lint, isolation tests, money tests, contract tests, evaluations | **Drafted** (Proposed) | Numbered 008 because 006 was taken. Needs the golden quotes from the pilot companies. |

## RFCs

| ID | Title | Status | Needs input from |
|---|---|---|---|
| RFC-001 | Pricing engine: FCL/AIR freight-only cost-based markup, Incoterms, rounding and FX | **Drafted** | Product spec v1.3 covers import and export FCL/AIR quotes for the requesting customer's payable costs; Hapag offers are fetched only when that customer owes ocean freight. LCL, Inland/Door-to-Door and AFRMM are out. Marcelo and the pilot companies still need to validate markup values, Incoterms, export cost allocation and fee inputs, FX, AIR examples and golden quotes in both directions. |
| RFC-002 | Email pipeline: inbound, outbound, sender identity, rendering implementation, hostile content, tariff-update review | **Drafted** | `ai-engine` rendering location is fixed by product spec v1.2; Amanda and Marcelo still need to confirm the renderer/template quality. Pilot companies need to test forwarding and decide retention. |
| RFC-003 | Connectors and the Devin loop: sources, deterministic health score, degraded mode, repair guardrails, demo mirror | **Drafted** | The public-source candidate list remains draft; the Hapag API connector is in scope for FCL. Amanda still owns source terms, Firecrawl and Devin limits. |
| RFC-004 | Extraction contract and confidence model, building on ADR-006 | **Drafted** | Marcelo (required fields, thresholds, evaluation set, vendor consent) |

## Other documents owed

- **Data-model follow-up for product spec v1.3:** resolve the private review comments on sections 4 and 5 together with the schema implications of freight-only markup, FCL `min_markup_usd`, a pinned `carrier_offer_id`, and the remaining RFC impacts. Keep the private product spec and pilot data out of this public repo; Pedro approves schema and contract changes before implementation.
- **Architecture diagram:** `diagrams/quotepilot-architecture.excalidraw` (4 frames).

## Open decisions

| Decision | Current default | Who decides |
|---|---|---|
| Go spike and Python fallback gate (ADR-002) | ADR-001 approves a Go core; the one-day spike still decides whether to use the fallback | Pedro with team agreement |
| FCL/AIR markup values and validation (RFC-001) | v1.3 fixes cost-based markup on base freight only; FCL `min_markup_usd` applies per container, while local charges and AIR FSC/SSC/fixed fees pass through | Marcelo and pilot companies |
| Export Incoterm allocation, fee basis, tariff availability and FX (RFC-001) | Quote only the requesting customer's payable costs; the existing import buyer-side matrix does not define export rules | Marcelo and pilot companies |
| Hapag Stage 4A sequencing and offer validation (RFC-001) | Python connector in `ai-engine` under ADR-001; `core-api` validates and persists its untrusted output before deterministic pricing | Pedro and Marcelo |
| LCL classification outside the v1.3 pricing MVP (ADR-006/RFC-004) | Approved classifier still has an LCL hint; no LCL quote may be priced in the MVP | Marcelo and Pedro |
| PDF and email HTML renderer/template details (RFC-002) | Product spec v1.2 places rendering in `ai-engine`; Jinja2 and the PDF library still need the RFC-002 output-quality check | Amanda and Marcelo |
| Outbound sender identity (RFC-002) | Shared verified domain with the company as display name and `Reply-To` | Amanda and Marcelo |
| Embedding evaluation (RFC-004) | ADR-006 approves `qwen/qwen3-embedding-8b` at 1024 dimensions; retrieval quality still needs evaluation | Pedro and Marcelo |
| Consent to send tenant data to third-party APIs, including the decision-model vendor (RFC-004) | Unknown | Pilot companies |
| Wording of the prompt-injection claim in ADR-006 (RFC-004 section 7) | Suggest "limits the blast radius" | Marcelo |
| How failing page content reaches Devin, given a public repo (RFC-003) | Minimal excerpt on a branch, subject to terms of use | Pedro and Amanda |
| Railway/Supabase plans, connection limits and account ownership (ADR-003) | Production Supabase region is `sa-east-1`; provider topology is fixed, provisioning details remain open | Pedro with the team |

## Suggested order

1. Run the ADR-002 Go spike to validate the approved ADR-001 core choice and its documented Python fallback gate.
2. Review ADR-002, ADR-005 and ADR-008 with Marcelo and Amanda; resolve ADR-003's hold before hosted provisioning. ADR-004 is approved, with its open items tracked in the ADR.
3. Send the remaining open questions of RFC-001 to RFC-004 to their owners. Confirm FCL/AIR markup values, AIR examples, import/export Incoterm and FX rules, and synthetic golden quotes in both directions; RFC-002's forwarding tests still depend on pilot mailboxes.
4. Write ADR-007 during Mission 2.
5. Turn each answered RFC into an ADR or fold it into an existing one, then write the v1.3 data model follow-up.
