# ADR / RFC plan and status (as of 2026-11-07)

ADR = a decision we make and record. RFC = a proposal that needs input from others before it becomes a decision.

## ADRs

| ID | Title | Status | Notes / blockers |
|---|---|---|---|
| ADR-001 | Modular monorepo: Go core + stateless Python ai-engine, contracts-first | **Drafted** (Proposed) | Needs team buy-in (Marcelo prefers Python). PT justification message ready. Reversible via contract. |
| ADR-002 | Go core stack: pgx v5, sqlc, `InTenantTx`, migrations tool, HTTP router, oapi-codegen, testcontainers, decimal lib, spike gate | **To write** | Spike criteria are in `CLAUDE.md`. Can be written now. |
| ADR-003 | Postgres on Supabase: roles, pooler mode, extensions, storage, local dev | **Drafted** (Proposed) | Supabase used as plain Postgres + S3 only: own roles (no BYPASSRLS), own schema, Data API off, session-mode pooler or direct. Open: region and plan, account owner, connection limits. Provisioning checklist is in the ADR. Local role model implemented in `infra/postgres/` (PR #2); departures from the first sketch are in the ADR's Revision notes. ADR-003 owns the role model and the acceptance checks; ADR-007 owns the hosted provisioning runbook (roles, schemas, default privileges, search_path, extensions). |
| ADR-004 | Quote pipeline as staged River jobs; worker is the only caller of the pricing engine (MVP) | **Drafted** (Proposed) | Retry counts and per-stage timeouts to tune in the spike. |
| ADR-005 | Auth (magic link + JWT), tenant resolution path, 1-click approval token | **To write** | Includes the privileged path for pre-tenant lookups (alias to org, memberships). |
| ADR-006 | Testing strategy: schema lint, isolation tests, money tests, contract tests | **To write** (new) | Isolation proof is a core demo asset. |
| ADR-007 | CI/CD and environments: registry, dev/staging, deploy mechanism, secrets, IPv4/IPv6 path per environment, hosted DB provisioning (roles, schemas, default privileges, search_path, extensions) | **To write** (new) | Written during Mission 2. Deploy jobs wait for it. Hosted DB provisioning depends on ADR-003 section 6. |

## RFCs

| ID | Title | Status | Needs input from |
|---|---|---|---|
| RFC-001 | Pricing engine: Incoterm handling, calculation order (CIF), surcharges and origin charges, rounding and penny allocation, margin on pass-through costs, FX per carrier | **To write** | Marcelo and the pilot companies (business rules) |
| RFC-002 | Email pipeline: inbound (metadata-only webhook, subdomain, classification of RFQ vs tariff update vs noise), outbound (per-tenant verified domains, threading), rendering location, prompt-injection defenses, attachment limits | **To write** (partly unblocked) | Amanda (Resend), Marcelo (templates) |
| RFC-003 | Connectors and the Devin loop: source list, confidence and sanity checks, degraded mode with amber quotes, CODEOWNERS scope, demo mirror portal | **To write** | Pilot companies (which terminals), Amanda |
| RFC-004 | Extraction contract and confidence model (deterministic checks, not LLM self-reported confidence) | **To write** (new) | Marcelo (agents owner) |

## Other documents owed

- **Data model v1.2 amendments:** resolve the 17 Notion comments on sections 4 and 5 (kept locally, not in git) into one migration plan.
- **Architecture diagram:** `diagrams/quotepilot-architecture.excalidraw` (4 frames).

## Open decisions

| Decision | Current default | Who decides |
|---|---|---|
| Go vs Python core | Go behind a 1-day spike; Python fallback | Pedro with team agreement |
| Where PDF and email HTML render | `ai-engine` (Jinja2 + HTML-to-PDF); drop React Email | Amanda and Marcelo |
| Embedding provider | OpenAI text-embedding-3-small at 1024 dims, after a 30-minute eval on real aliases | Pedro and Marcelo |
| Margin on pass-through terminal costs | Unknown | Marcelo and the pilot companies |
| Consent to send tenant data to third-party APIs | Unknown | Pilot companies |

## Suggested order

1. Team agrees on ADR-001 (or flips to the Python fallback).
2. Spike (1 day) in a connected repo; write ADR-002 and ADR-003 while it runs.
3. ADR-005 and ADR-006, then RFC-001 and RFC-002 as inputs arrive.
4. RFC-003 and RFC-004 once the pilot source list and Marcelo's extraction schema are known.
