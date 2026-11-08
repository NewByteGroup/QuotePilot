# Agent Instructions

Shared working rules for every coding agent in this repo (Claude Code, Copilot, Devin).
`CLAUDE.md` imports this file; keep the rules here, not there.

## Project purpose

QuotePilot is a multi-tenant B2B SaaS that turns messy RFQ emails from freight forwarders
into audited, multi-currency quotes in minutes (Inbox-to-Quote). Hack Brasil 2026, team
NewByte. Online phase 04/11 to 30/11, in-person final 12/12 (Google Brasil). Pilot: 4 real
foreign-trade companies.

Team: Pedro (backend, tenancy, pricing engine, infra), Marcelo (PO, AI agents, UI/UX),
Amanda (frontend, Resend, Firecrawl, Devin ops). Pedro owns `apps/core-api`; he decides,
agents implement and review.

## Architecture rules that must never be broken

Source of truth: `docs/adr/0001-modular-monorepo-go-core-python-ai-engine.md`. If a task
seems to need breaking one of these, stop and ask Pedro. Do not work around it.

1. Only `core-api` connects to Postgres or object storage. `ai-engine` has no DB or storage credentials.
2. `ai-engine` never receives `organization_id`. Tenant context is fetched by core inside the tenant transaction and passed in the request.
3. Every tenant query goes through `InTenantTx`, which runs `SELECT set_config('app.current_organization_id', $1, true)`. Never string-format a tenant id into SQL.
4. RLS policies use `NULLIF(current_setting('app.current_organization_id', true), '')::uuid`. Every table with `organization_id` has ENABLE and FORCE RLS. The app DB role is not the table owner and has no BYPASSRLS.
5. All money math is decimal in `internal/pricing` (no floats, no LLM arithmetic). The package is pure: no I/O.
6. Treat every `ai-engine` response as untrusted: validate schema and sanity bounds in core before persisting.
7. River job args contain IDs only, never tenant content.
8. Connectors are code with fixture tests. No hot-reloading of LLM-written parsers. Devin may only change `apps/ai-engine/connectors/**` and its tests.
9. Quotes are reproducible: record pinned `public_snapshot_id`s and FX rates; recalculation creates a new quote version, never overwrites.

Ask Pedro before changing the schema, a contract in `packages/contracts`, or any rule above.

## Git branching and PR convention

`develop` is the shared integration branch; `main` is the release branch.

1. **Branch off `develop`** for every task: `git switch -c <type>/<slug> origin/develop`.
   One branch per task.
2. **Merge the feature branch into `develop`** via its PR (base = `develop`).
3. **Promote `develop` into `main`** with a `develop` → `main` PR, after a milestone and before
   the final. Pedro triggers it; it merges as soon as CI is green.
4. **Hotfix** (production-only breakage): `hotfix/<slug>` off `main`, PR into `main`, then merge
   `main` back into `develop` with a real merge commit.

### Branch names

`<type>/<slug>`: lowercase, kebab-case, at most 50 characters. An issue number may follow the
type: `feat/42-inbound-webhook`.

| Type | Use |
|---|---|
| `feat` | new behavior |
| `fix` | bug fix |
| `refactor` | no behavior change |
| `test` | tests only |
| `docs` | ADRs, RFCs, specs, plans, README |
| `chore` | tooling, deps, repo structure |
| `ci` | workflows, hooks, branch protection docs |
| `spike` | time-boxed experiment with a pass/fail gate (never merged as-is) |
| `hotfix` | urgent fix off `main` |

Start the slug with the app when the change is scoped to one: `feat/core-inbound-webhook`,
`fix/ai-engine-btp-parser`, `chore/web-eslint`.

### Commits

Conventional Commits: `type(scope): summary` in imperative mood, one concern per commit.
Scopes: `core-api`, `ai-engine`, `web`, `contracts`, `infra`, `docs`, `ci`.
Example: `feat(core-api): verify Svix signature on inbound webhook`.

### Merging

**Always use a real merge commit. Never squash-and-merge and never cherry-pick.** Every
integration step (feature → `develop`, and `develop` → `main`) is an ordinary merge that
preserves the original commits. Squashing or cherry-picking creates new, independently
authored commits, so the same change has no shared history between branches, and git then
reports an add/add conflict on every file touched by more than one such change at the next
`develop`↔`main` merge, even when the content is identical. One real commit per change,
merged and not copied, avoids this. Disable squash and rebase merging in the repo settings.

### Small PRs

**One unit of behavior per PR**: one endpoint, one repository, one migration plus its tests.
When a spec or plan lists several independent endpoints, handlers or repositories, split them
into one PR each, stacked in sequence against `develop` if later ones depend on shared code
introduced by an earlier one. Judge this during `writing-plans`/`brainstorming`, before coding
starts, not after everything is implemented. A PR that needs more than about 400 changed lines
of non-generated code is a signal to split it.

### PR template

Every PR uses `.github/pull_request_template.md` (source: `docs/templates/pull_request_template.md`). Fill every section; do not delete sections
that do not apply, write "n/a" and a reason. `gh pr create` reads the template only without
`--body`, so pass the filled template as the body (`--body-file`).

### Protected branches

`main` and `develop`: PR required, the required checks from the CI/CD section green, no force
push, merge commits only, CODEOWNERS review for `apps/core-api/**`, `packages/contracts/**`,
`apps/core-api/db/migrations/**` and `apps/ai-engine/connectors/**`. Pedro applies these
settings in GitHub; agents document them and verify them with `gh api`, and never change
repository settings without explicit approval.

Never force-push a shared branch. Never commit secrets; `.env.example` holds names only.

## End-to-end development workflow

Every implementation task follows this workflow. Do not silently skip a step; the
exceptions are explicit.

1. **Read context first.** `CLAUDE.md`, `docs/adr-rfc-plan.md`, the ADRs and RFCs that touch
   the task, and any existing spec in `docs/specs/`. Delegate wide searches to `explore`.
2. **Create and approve a tech spec.** Always invoke the `brainstorming` skill, including for
   simple tasks. The skill's classification decides the artifact:
   - Bounded tasks: a short in-chat design. Once Pedro approves it, it is the tech spec.
   - Architectural tasks: a reviewed and approved `docs/specs/YYYY-MM-DD-<slug>.md`.
3. **Create an implementation plan.** For architectural tasks, invoke `writing-plans` and write
   `docs/plans/YYYY-MM-DD-<slug>.md` before coding. Bounded tasks may go straight from the
   approved design to code.
4. **Develop on a feature branch** off `develop` (naming above), test-first
   (`test-driven-development`). Implementation is delegated to `executor` (see Subagent
   delegation). Use `using-git-worktrees` when two tasks run in parallel.
5. **Verify before claiming done** (`verification-before-completion`): the orchestrator itself
   runs the test command, the lint, and the isolation suite when the change touches DB,
   tenancy or money. Record the exact commands and output in the PR description.
6. **Review locally.** `code-reviewer` reviews the full diff against the architecture rules
   before the PR is opened. Fix its findings first.
7. **Open a PR targeting `develop`** using the PR template (`gh pr create --base develop
   --body-file <filled template>`). Request GitHub Copilot review when it is enabled on the
   repo. A local subagent review is not a Copilot review; do not describe it as one.
8. **Process review and CI.** Fix comments that make technical sense and answer with reasoning
   when they do not (`receiving-code-review`). Wait for CI with
   `gh pr checks <pr-number> --watch`. If CI fails or review issues remain, stop and ask
   whether to start troubleshooting. Do not troubleshoot automatically.
9. **Wait for explicit merge authorization from Pedro**, then
   `gh pr merge <pr-number> --merge`.
10. **Post-merge.** Wait for the push-to-`develop` pipeline to turn green. Deployment to the dev
    environment is defined by the CI/CD mission and its ADR; until that exists, stop here and
    report. Do not invent a deploy step.
11. **Promote to `main`.** Open `develop` → `main` when Pedro asks, wait for CI, merge with a
    real merge commit.
12. **Troubleshoot only with permission.** On any CI, PR or runtime failure, stop and ask. Once
    approved, invoke `systematic-debugging`, find the root cause first, and fix forward.
    Revert a merged change only when Pedro explicitly asks.

## Skills routing

Skills are installed per developer (see `docs/skills.md`); third-party ones are not committed.
Skills trigger from their descriptions, which is not reliable enough. Use this table; when a
row matches, invoke the skill before doing anything else for that step.

| Situation | Skill |
|---|---|
| Any new feature or behavior change | `brainstorming` |
| Approved design, architectural task | `writing-plans`, then `executing-plans` or `subagent-driven-development` |
| Independent sub-tasks | `dispatching-parallel-agents` |
| Writing production logic (exceptions listed under Tests) | `test-driven-development` |
| Bug, failing test, unexpected behavior | `systematic-debugging` (and `investigate-first`) |
| About to say "done" | `verification-before-completion`, `verify-and-stop` |
| Restructuring code | `safe-refactor` |
| Review requested or received | `requesting-code-review` / `receiving-code-review` |
| Branch finished | `finishing-a-development-branch` |
| Schema or migration | `database-schema-designer`, `migration`, `golang-database` |
| Go code in `apps/core-api` | the project-scoped `golang-*` skills (database, error-handling, testing, security, concurrency, context, code-style, naming, project-layout, lint, observability, safety, structs-interfaces, stretchr-testify) |
| Editing this file or `CLAUDE.md` | `agent-md-refactor` |

## Subagent delegation (mandatory)

Roles are configured through oh-my-claudecode (user-level config, not committed). The main
session is the **orchestrator** (Sonnet or Opus, never Haiku): it plans, decides, talks to
Pedro and delegates. It does not do the legwork inline.

| Role | Agent | Model | Mode |
|---|---|---|---|
| Researcher / librarian | `explore` | Haiku | read-only |
| Writer | `executor` | Sonnet | writes code and tests |
| Reviewer | `code-reviewer` | Opus | read-only |

**Delegation rules**

1. **Research goes to `explore`.** Anything that needs more than about three files read, a
   search across directories, or a look through `docs/` (ADRs, RFCs, specs, plans) is delegated.
   Do not fill the main context with wide greps.
2. **Implementation goes to `executor`.** Any task from an approved plan that touches more than
   one file or more than about 30 lines is delegated, one plan task per executor. Run several in
   parallel (`dispatching-parallel-agents`) only when they touch disjoint files.
3. **Review goes to `code-reviewer`.** It reviews the full diff before every PR is opened, and
   after each plan task under `subagent-driven-development`. It checks the nine architecture
   rules, tenant isolation, money handling, error handling and test quality. Fix its findings,
   then re-run it on the fix. The writer and the reviewer are never the same agent.
4. **The orchestrator does itself only:** edits of about 10 lines or fewer, git and `gh`
   commands, specs and plans (`brainstorming`, `writing-plans`), decisions, and everything said
   to Pedro.
5. **Every delegation prompt contains:** the goal, the exact files in scope, the relevant rules
   from this file, the command that proves it works, and what not to touch. No delegation
   without an acceptance command.
6. **Do not trust a subagent's report.** The orchestrator re-runs the tests and reads the diff
   (`verification-before-completion`) before saying a task is done.
7. **If a named agent type is not available,** tell Pedro once, say what you would have
   delegated, and continue with a general-purpose agent. Never silently do the whole job inline.
8. Do not set `CLAUDE_CODE_SUBAGENT_MODEL`: it forces every subagent onto one model and defeats
   the roles above.

**How Pedro checks routing works:** `.omc/state/sessions/*/subagent-tracking-state.json` should
list `agent_type` values `explore`, `executor` and `code-reviewer`. Entries named
`untracked-native-fork` mean the work ran in generic forks and the roles are not applied.

## Code style

- Functions: 4-20 lines. Split if longer.
- Files: under 500 lines. Split by responsibility.
- One thing per function, one responsibility per module.
- Names: specific and unique. Avoid `data`, `handler`, `Manager`. Prefer names that return fewer than 5 grep hits.
- Types: explicit. Go: no `interface{}`/`any` unless unavoidable. Python: full type hints, no bare `dict`, `mypy`/`pyright` clean. TypeScript: no `any`.
- No code duplication. Extract shared logic into a function or module.
- Early returns over nested ifs. Max 2 levels of indentation.
- Error messages must include the offending value and the expected shape. Go: wrap with `%w`, never discard errors.
- Money is `decimal` (Go `shopspring/decimal`, Python `Decimal`). A float anywhere near money is a bug.

## Comments

- Keep existing comments. Do not strip them on refactor; they carry intent and provenance.
- Write WHY, not WHAT. Skip `// increment counter` above `i++`.
- Docstrings on public functions: intent plus one usage example.
- Reference ADR/RFC numbers or issue numbers when a line exists because of a specific decision or bug.

## Tests

- One command per app. `core-api`: `make test` (`go test -short ./...`). `ai-engine`: `make test-ai` (`pytest`). `web`: `make test-web`. Root `make test` runs all three.
- Every new function gets a test. Bug fixes get a regression test.
- **TDD scope.** Write the failing test first, watch it fail for the right reason, then
  implement, for all production logic: the pricing package, `InTenantTx` and RLS policies
  (isolation test first), pipeline stage logic, validation of `ai-engine` responses, and connector
  parsers (fixture first). Declared exceptions, where test-first is not required:
  scaffolding, stubs and config; generated code; visual UI layout (behavior still gets component
  tests); LLM prompts and agents (write the evaluation set before tuning a prompt instead); and
  spikes, which are exploratory but must write their gate tests first because those tests are the
  pass/fail criteria. An exception never excuses leaving finished behavior untested.
- **Isolation tests and money tests are mandatory.** Any change touching DB, RLS, tenancy, the
  pricing package or the quote state machine must run the real-Postgres suite
  (`make db-up`, `make test-integration`, `POSTGRES_DSN` from `.env.example`) before the PR is
  opened. `make test` alone does not prove RLS works: `-short` skips those tests. Never mock
  the database in a test that is meant to prove tenant isolation.
- The isolation suite must keep proving: pooled connection reuse A then B leaks nothing;
  identical vectors in orgs A and B queried without a WHERE clause return 0 rows of B; no tenant
  context set returns 0 rows or a clean error.
- Mock external I/O (Resend, Firecrawl, LLM APIs, filesystem) with named fake classes, not
  inline stubs. Connector tests run against recorded fixtures in
  `apps/ai-engine/tests/fixtures/<connector>/{old,new}`; no live network in tests.
- Tests must be F.I.R.S.T: fast, independent, repeatable, self-validating, timely.

## CI/CD

Workflows live in `.github/workflows/`, path-filtered per app plus a shared set. The job names
below are the contract with branch protection: renaming one breaks the required checks.

**Required on every PR into `develop` and `main`**

| Check | What it runs |
|---|---|
| `lint` | `gofmt` + `golangci-lint`; `ruff format --check` + `ruff check` + type check; `eslint` + `prettier --check` + `tsc --noEmit`; `actionlint`; OpenAPI lint |
| `test` | `go test -race -short ./...`; `pytest`; `vitest`; coverage published as an artifact |
| `integration-test` | Postgres 16 + pgvector service container; migrations from scratch; the RLS isolation suite; River job tests |
| `contracts-drift` | regenerate Go, Pydantic and TypeScript from `packages/contracts` and fail on any diff; regenerate `sqlc` and fail on any diff |
| `migrations` | apply all migrations to an empty database; fail on non-linear history or on a migration that drops a tenant policy |
| `security` | `govulncheck`, `gosec`, `pip-audit`, `npm audit --omit=dev`, `gitleaks`, dependency review, `trivy` on the built images |
| `devin-scope` | on PRs from Devin, fail if any changed path is outside `apps/ai-engine/connectors/**` and `apps/ai-engine/tests/**` |
| `build` | build the `core-api`, `ai-engine` and `web` images; no push on PRs |

**After merge**

- Push to `develop`: build and push images, deploy to the dev environment.
- Push to `main`: build, deploy to staging.
- Registry, hosting targets and environments are decided in an ADR (CI/CD and environments),
  not by the agent. Until it is accepted, those two deploy jobs do not exist.

**Workflow hygiene:** pin third-party actions by commit SHA; set `permissions:` to the minimum on
every workflow; cancel superseded runs with `concurrency`; cache Go modules, pip and npm;
secrets only from GitHub environments, never echoed. Run the same checks locally with
`make ci` (lint, test, integration-test, contracts-drift) and keep a pre-commit hook for
`gofmt`, `ruff` and `gitleaks`.

## Dependencies

- Inject dependencies through constructor/parameter, not globals or imports.
- Wrap third-party libraries behind a thin interface owned by this project (Resend, Firecrawl,
  LLM and embedding providers, Supabase Storage).
- Go stack candidates are in ADR-002. Do not add a library that is not there
  without asking.

## Structure

- Follow the framework's convention. Prefer small focused modules over god files.
- `apps/core-api/cmd/server` is a wiring-only composition root: config loading, dependency
  construction, server start. Logic lives in `internal/`: `tenantdb` (InTenantTx), `pricing`
  (pure), `quotes`, `inbox`, `platform`. Nothing under `internal/` imports `cmd/`;
  `internal/pricing` imports no I/O package.
- `apps/ai-engine` never imports a database driver or storage SDK.
- `apps/web` talks to the backend only through the generated client from `packages/contracts`.
- Cross-component shapes change in `packages/contracts` first; code follows via codegen.

## Formatting

- Use the language default formatter and linter: `gofmt` + `golangci-lint`, `ruff format` +
  `ruff check`, `prettier` + `eslint`. Do not discuss style beyond that.

## Logging

- Structured JSON for debugging and observability (`log/slog` in Go, `structlog` or stdlib JSON
  in Python). Plain text only for user-facing CLI output.
- Never log email bodies, attachments, rates or margins. `ai-engine` does not log request
  payloads at all. Always include `correlation_id`; never log `organization_id` from ai-engine.

## Documentation and specs

Committed design documentation is split by kind, one Markdown file per topic:

- **Decisions → `docs/adr/`** (`NNNN-<slug>.md`, status Proposed/Accepted/Superseded).
- **Proposals under discussion → `docs/rfc/`** (`YYYY-MM-DD-rfc-NNN-<slug>.md`). An accepted RFC produces an ADR.
- **Specs, tech specs, design docs → `docs/specs/`** (`YYYY-MM-DD-<slug>.md`). Marcelo's product spec is kept locally and is not in git (the repo is public).
- **Implementation plans → `docs/plans/`** (`YYYY-MM-DD-<slug>.md`).
- **Diagrams → `docs/diagrams/`** (Excalidraw sources plus a PNG preview).

Do not scatter them elsewhere, and do not put a plan in `docs/specs/` or a spec in `docs/plans/`.
The status table of every ADR and RFC is `docs/adr-rfc-plan.md`; update it when one changes status.

**Never create a `docs/superpowers/` directory or commit anything under it.** Some workflow
skills (for example `obra/superpowers` `writing-plans`) default to `docs/superpowers/plans/` and
`docs/superpowers/specs/`. Override that default and write to `docs/plans/` and `docs/specs/`.
`docs/superpowers/` and `.superpowers/` (skill scratch: ledgers, briefs, review diffs) are
git-ignored.

## Check ADR/RFC consistency before implementing

Before implementing any task, read `docs/adr-rfc-plan.md` and the ADRs/RFCs it points to for
the area you are touching. Implementing one piece in isolation risks schemas, interfaces or
contracts that conflict with a sibling piece written later.

- Read the whole section a task belongs to, not only its own bullet.
- Look for overlap: two tasks touching the same table, interface or external contract.
- Propose an order (prerequisite first) or merging two tasks when splitting them is artificial.
  Propose it to Pedro; do not silently pick.
- Do not assume the listed order is already conflict-free.

## Language

Code, comments, ADRs, RFCs, specs, plans and PR descriptions: English. Notion comments and
messages to Marcelo and Amanda: Portuguese.
