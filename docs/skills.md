# Skills

Skills are installed per developer. Third-party skills are not committed to this repo; only
skills written by this team may live (committed) under `.claude/skills/`. Which skill to use
when is in the routing table in `AGENTS.md`.

## Installed (Pedro, 2026-11-06)

| Source | Skills | Install |
|---|---|---|
| Superpowers 7.0.0 (user scope) | brainstorming, dispatching-parallel-agents, executing-plans, finishing-a-development-branch, receiving-code-review, requesting-code-review, subagent-driven-development, systematic-debugging, test-driven-development, using-git-worktrees, using-superpowers, verification-before-completion, writing-plans, writing-skills, diagnosing-superpowers | `claude plugin install superpowers@claude-plugins-official --scope user` |
| samber/cc-skills-golang (14, copied to `.claude/skills/`, git-ignored) | golang-database, golang-error-handling, golang-testing, golang-stretchr-testify, golang-security, golang-concurrency, golang-context, golang-project-layout, golang-code-style, golang-naming, golang-lint, golang-observability, golang-safety, golang-structs-interfaces | copy the directories from the upstream repo; confirm its license before any redistribution |

## Missing

`investigate-first`, `lean-build`, `safe-refactor`, `verify-and-stop`, `agent-md-refactor`,
`database-schema-designer`, `migration`, `find-skills`. Likely sources: the public
`vercel-labs/skills` (find-skills) and `softaworks/agent-toolkit` (agent-md-refactor,
database-schema-designer) repos, unverified; the rest are Pedro's own or employer skills and
are not committed. Until a skill is installed, do not invoke it: the routing table falls back to
the closest Superpowers skill (`verification-before-completion`, `systematic-debugging`).

## Known gap

`writing-plans` defaults to `docs/superpowers/`. `AGENTS.md` overrides it in prose and the path
is git-ignored. If the first plan lands there anyway, add a `PreToolUse` hook in
`.claude/settings.json` that blocks writes under `docs/superpowers/**` (Mission 2).
