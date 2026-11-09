# ADR-001: Modular monorepo with a Go core and a stateless Python ai-engine

- **Status:** Approved (open items remain)
- **Date:** 2026-11-04
- **Deciders:** Pedro (backend lead). Reviewed and approved by Marcelo and Amanda.
- **Related:** ADR-002 (Go core stack), ADR-003 (Postgres on Railway dev / Supabase prod, R2 storage), ADR-004 (async jobs), RFC-003 (connectors and Devin loop)

## Context

QuotePilot is a multi-tenant B2B SaaS. Three facts shape the architecture:

1. **Isolation is the product risk.** Net rates, customers and margins are trade secrets. A leak between tenants is the failure we cannot afford. Isolation is enforced in PostgreSQL with Row-Level Security (product spec section 4.3, kept locally and not in git).
2. **Two kinds of code have very different trust levels.**
   - *Stateful, financial code:* tenancy, RLS transactions, the pricing engine, quote state machine, webhooks, storage. It must be deterministic and auditable.
   - *Untrusted-input code:* LLM agents that read customer email, and scrapers/parsers for public portals. The scraper code is rewritten by Devin (an LLM) when a portal changes. It is the code most likely to be wrong or manipulated.
3. **Constraints:** 3 people, online phase 04/11 to 30/11, final on 12/12. Load is tiny (4 pilot tenants); LLM calls dominate latency. Correctness, isolation and idempotency matter more than throughput.

## Decision

**One repository, two deployable runtimes, separated by a strict boundary.**

| Component | Language | Responsibility |
|---|---|---|
| `apps/core-api` | Go | HTTP API, auth and tenant resolution, **all** database and object-storage access, RLS transactions, pricing engine, quote state machine, Resend webhooks and sends, job queue and workers, connector registry and snapshots, Devin session tracking |
| `apps/ai-engine` | Python 3.12 | RFQ parsing agents, NCM/LOCODE normalization, Firecrawl connectors and parsers, document (PDF/email HTML) rendering, embeddings calls. Stateless HTTP service |
| `apps/web` | React + TypeScript + Vite | Pricing Workspace and Ops Console |

### Boundary rules (normative)

1. **Only `core-api` connects to PostgreSQL or object storage.** `ai-engine` has no database credentials and no storage credentials.
2. **`ai-engine` is stateless: JSON in, JSON out.** It keeps no tenant data between requests, does not log request payloads, and has no tenant-scoped cache.
3. **`ai-engine` does not receive `organization_id`.** It receives only the data needed for the task plus an opaque `correlation_id`. Tenant context needed for a task (for example candidate tariffs) is fetched by `core-api` inside the tenant transaction and passed in the request.
4. **Agent tools inside `ai-engine` may read only public reference data** (UN/LOCODE, NCM tables). Anything tenant-specific arrives in the request.
5. **`core-api` treats every `ai-engine` response as untrusted.** Responses are validated against the contract schema and sanity bounds (value ranges, deltas against the previous snapshot, expected row counts) before anything is persisted. Wrong-but-well-formed rates are the dangerous case, not crashes.
6. **`core-api` performs all money arithmetic** (decimal, no floats). No LLM or Python code adds or converts currency.
7. **Connectors are code, not configuration.** Each connector is an isolated Python module with fixture-based contract tests. The registry row (URL, schedule, thresholds) is the only declarative part.
8. **No hot-reloading of parser code in production.** A repair ships as: Devin PR, CI (pytest on fixtures), human review and merge, redeploy of `ai-engine` only. Because `ai-engine` is small and stateless, that redeploy is fast.
9. **Devin's write scope is limited** to `apps/ai-engine/connectors/**` and `apps/ai-engine/tests/**`, enforced by `CODEOWNERS`, branch protection and a path-filtered CI job.

### Contracts first

`packages/contracts/` is the single source of truth for the shapes exchanged between components (OpenAPI 3.0.3 and JSON Schema — 3.0.3 not 3.1, per ADR-002: `oapi-codegen` does not support 3.1):

- core-api HTTP API (consumed by `web`)
- core-api to ai-engine requests/responses (RFQ extraction, connector output, render requests)

Types are generated for Go, Pydantic and TypeScript. CI fails if generated code is out of date. Frontend and ai-engine work can start against mocks of the contract without waiting for `core-api`.

### Repository layout

```
quotepilot/
├── apps/
│   ├── core-api/            # Go: cmd/server, internal/{tenantdb,pricing,quotes,inbox,platform}, db/migrations
│   ├── ai-engine/           # Python: agents/, connectors/, render/, tests/ (+ tests/fixtures/<connector>/{old,new})
│   └── web/                 # React + TS + Vite + Tailwind/shadcn
├── packages/
│   └── contracts/           # OpenAPI + JSON Schema, codegen config
├── infra/                   # compose files, deploy config
├── docs/
│   ├── adr/                 # architecture decisions (what we chose and why)
│   ├── rfc/                 # proposals open for discussion before they become ADRs
│   ├── specs/               # feature/design specs (output of brainstorming)
│   ├── plans/               # implementation plans (output of writing-plans)
│   └── diagrams/            # Excalidraw sources
├── .claude/
│   ├── skills/              # skills written by this team, committed; third-party skills are not (see docs/skills.md)
│   └── agents/              # project subagents
├── AGENTS.md                # agent working rules (shared source of truth)
├── CLAUDE.md                # imports AGENTS.md, adds Claude Code specifics
├── .github/
│   ├── CODEOWNERS
│   └── workflows/           # path-filtered CI per app
└── docker-compose.yml       # Postgres 16 + pgvector, core-api, ai-engine, web
```

## Alternatives considered

1. **Python modular monolith (FastAPI) in one process.** Lowest ramp-up and best bus factor, but LLM/scraper code would run with the same database credentials as the pricing engine. Rejected as the default because the boundary is the strongest isolation argument we have. Remains the fallback (see Reversibility).
2. **Go only.** Rejected: AI SDKs, Firecrawl tooling and, above all, Devin's `pytest` repair loop are Python-native.
3. **Elixir core.** Strong job and supervision story, but contract-first code generation is weaker, and its main strengths (long-lived stateful concurrency) are not needed at our scale.
4. **Distributed services / microservices.** Rejected: operational cost with no benefit at 4 tenants and 3 engineers.

## Consequences

**Positive**
- Untrusted-input and LLM-written code cannot touch tenant data stores. The blast radius of a bad agent or a bad Devin patch is a wrong response that `core-api` validates, not a data leak.
- `ai-engine` runs `pytest` in Devin's sandbox with no database, which makes the self-healing loop realistic.
- One repo keeps contracts, ADRs and CI together.

**Negative / risks**
- Two runtimes mean two Dockerfiles, two CI jobs and a contract to maintain. Mitigation: contracts-first codegen and a CI drift check.
- `core-api` is a single-owner critical path (Pedro). Mitigation: contract mocks so Marcelo and Amanda are not blocked, small modules, ADRs for every non-obvious decision.
- Tool-calling agents cannot read tenant data mid-reasoning. Mitigation: boundary rule 3 (context is passed in); revisit by RFC only if a concrete need appears.

**Reversibility.** `web` and `ai-engine` depend on `core-api` only through the contract. If the Go spike in ADR-002 fails its gate, the core can be reimplemented in Python (FastAPI) behind the same contract and the same two-process boundary, without touching `web` or `ai-engine`.

## Open items

- Document rendering details. Product spec v1.2 fixes the location: `ai-engine` renders the PDF and email HTML, with templates owned by Marcelo and Amanda. RFC-002 still requests their confirmation of the renderer and template details.
- Embedding consent and evaluation questions (RFC-004). The approved ADR-006 model matrix selects `qwen/qwen3-embedding-8b` at 1024 dimensions, recorded with `embedding_model`.
- Service-to-service auth between `core-api` and `ai-engine` (private network plus a shared service token is the minimum).
