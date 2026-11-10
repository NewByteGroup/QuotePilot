@AGENTS.md

# QuotePilot: Claude Code context

Shared rules (architecture invariants, git flow, workflow, style, tests, docs layout) are in
`AGENTS.md`, imported above. This file adds what is specific to Claude Code and the current
state of the project.

## Architecture in one paragraph

Modular monorepo, two runtimes. `apps/core-api` (Go) is the only component that touches
Postgres and storage: API, auth, tenant resolution, RLS transactions, pricing engine, quote
state machine, Resend webhooks and sends, River job queue. `apps/ai-engine` (Python,
stateless) hosts LLM agents, Firecrawl connectors, document rendering and embeddings calls.
`apps/web` is React + TypeScript. `packages/contracts` (OpenAPI + JSON Schema) is the source
of truth between them. Postgres 16 + pgvector (Railway in dev, Supabase in prod) with RLS
enforced; object storage on Cloudflare R2. Diagram:
`docs/diagrams/quotepilot-architecture.excalidraw`.

## Where things are

- `docs/adr-rfc-plan.md`: status of every ADR and RFC (done, to write, owner, blockers).
- `docs/adr/`: ADR-001 (monorepo, Go core + Python ai-engine), ADR-002 (Go core stack and spike gate), ADR-003 (Postgres: Railway dev, Supabase prod, R2 storage), ADR-004 (River staged jobs), ADR-005 (auth and tenant resolution), ADR-007 (CI/CD, environments, hosted DB provisioning) and ADR-008 (testing strategy) drafted; ADR-006 (AI agents in `ai-engine`) approved.
- Marcelo's product spec (Portuguese) and the 17 Notion review comments on sections 4 and 5 are kept locally and are not in git (the repo is public). Sections 4 and 5 and the pricing equation (4.4) are Pedro's.

## Decisions so far

- ADR-001 (Approved): Go core + stateless Python ai-engine. The Python/FastAPI fallback behind the same contract remains, gated on ADR-002's spike.
- ADR-004: staged River jobs; the worker is the only caller of the pricing engine in the MVP.
- ADR-003 (On-Hold; provider topology fixed by product spec v1.2): Railway Postgres for dev, Supabase `quotepilot-prod` in `sa-east-1` for prod, Cloudflare R2 (`quotepilot-dev`/`quotepilot-prod` buckets) for object storage. The role model still applies: `qp_migrator` owner and `qp_app` NOBYPASSRLS roles, tables in schema `app`, RLS with stand-in roles locally, Data API and pg_graphql disabled on Supabase, session-mode pooler or direct connection (River needs LISTEN/NOTIFY). Plans, connection limits and account owner are still open.
- Embeddings: `VECTOR(1024)` plus an `embedding_model` column, no ANN index in the MVP; provider fixed at `qwen/qwen3-embedding-8b` (1024 dims) by ADR-006's approved model matrix — a retrieval-quality eval is still owed.
- Document rendering (PDF and email HTML): `apps/ai-engine/render/` with Jinja2 + WeasyPrint; HTML/CSS templates owned by Marcelo and Amanda (resolved via the ADR-001 items; the output-quality check is owed in RFC-002).

## Missions, in order

Each mission is its own branch and PR (or several small ones), following `AGENTS.md`. Use
`brainstorming` first and show the plan before changing anything.

**Mission 0: skills.** Install and verify the skills named in `AGENTS.md` (see the Skills
routing table). Third-party skills are installed per developer and are **not committed**:
the Go skills sit in `.claude/skills/` but are git-ignored (`.claude/skills/golang-*/`), because
only Pedro writes Go and their upstream license is unchecked. Only skills this team writes itself
may be committed under `.claude/skills/`. `docs/skills.md` records what is installed, from where,
and what is still missing. Show every install command before running it, and read each
third-party `SKILL.md` before enabling it. Nothing from Pedro's employer is committed.

**Mission 1: structure the repo.** First create `develop` from `main` and push it (the only direct
push allowed; do not touch repository settings), then branch `chore/repo-structure` off `develop`.
No business logic. Create the
layout from ADR-001 (`apps/core-api`, `apps/ai-engine`, `apps/web`, `packages/contracts`,
`infra`, `docs/*`), a root `Makefile` with the targets named in `AGENTS.md` (`test`, `test-ai`,
`test-web`, `test-integration`, `db-up`, `ci`; each app may start as a stub that exits 0),
`docker-compose.yml` (Postgres 16 with pgvector), `.env.example`, and a short README per app
saying what belongs there. Show the tree and wait for approval before committing.

**Mission 2: git flow and CI/CD.** Branch `ci/pipeline`.
1. Install the PR template: copy `docs/templates/pull_request_template.md` to
   `.github/pull_request_template.md` (remote tools could not write into `.github/`). Keep it in
   sync with `AGENTS.md`.
2. `.github/CODEOWNERS` for the paths in the Protected branches section (ask Pedro for the
   GitHub handles of Pedro, Marcelo and Amanda).
3. Workflows implementing every check in the CI/CD section of `AGENTS.md`, with exactly those job
   names, path filters, SHA-pinned actions, minimal `permissions:`, concurrency and caching.
   Checks for apps that are still stubs must pass trivially, not be skipped, so that branch
   protection can require them from day one.
4. Local parity: `make ci`, plus a pre-commit config (`gofmt`, `ruff`, `gitleaks`).
5. Write `docs/adr/0007-ci-cd-and-environments.md` as Proposed: registry, environments
   (dev, staging), deploy mechanism, secrets. Do not create deploy jobs until it is accepted.
6. Give Pedro the exact `gh api` commands or the click-path to apply branch protection on `main`
   and `develop` (required checks, merge commits only, no force push, CODEOWNERS review). Do not
   run them without his go-ahead.

**Mission 3: Go spike (1 day, pass/fail).**
1. `docker compose up` brings up Postgres 16 with pgvector.
2. Migration creates a non-owner app role without BYPASSRLS and two tenant tables with ENABLE + FORCE RLS.
3. `InTenantTx` plus one `sqlc` query works.
4. Isolation tests pass (pooled reuse A then B; identical vectors without WHERE; no tenant context).
5. Webhook with valid signature does an idempotent insert on `(organization_id, resend_message_id)`.
6. A River job enqueued in the same transaction runs with tenant context.

If it does not pass quickly, switch the core to Python/FastAPI behind the same contract
(ADR-001, Reversibility).

**Then:** ADR-002, ADR-005, ADR-007, ADR-008 and RFC-001..004 are drafted (ADR-001 and ADR-004 approved; ADR-003 on hold pending its open items). The next steps are answering the RFC open questions (owners are in `docs/adr-rfc-plan.md`).

## Working with Pedro

- Pedro's daily drivers are Go and Elixir/Erlang; he also writes Python. Explain Python/React idioms when they matter, not Go ones.
- Use plan mode for architectural tasks. Ask before changing the schema or any rule in `AGENTS.md`.
- Do not create cloud resources (Supabase project, deployments) without explicit confirmation.
- Repo: `github.com/NewByteGroup/QuotePilot`. `main` exists; `develop` is created at the start of Mission 1.
