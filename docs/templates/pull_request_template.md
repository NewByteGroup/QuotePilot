## Summary

<!-- One or two sentences: what changes and why. -->

## Links

- Spec: `docs/specs/...` or "in-chat design approved by Pedro on <date>"
- Plan: `docs/plans/...` or n/a
- ADR / RFC: `docs/adr/...`, `docs/rfc/...` or n/a
- Issue: #

## Type

- [ ] feat
- [ ] fix
- [ ] refactor
- [ ] test
- [ ] docs
- [ ] chore / ci

Scope: <!-- core-api | ai-engine | web | contracts | infra | docs | ci -->

## What changed

<!-- Bullet list of the behavior added or changed. Not a file list. -->

## Architecture rules touched

<!-- Tick every rule this PR touches or could affect, then say in one line how it is kept. -->

- [ ] Only core-api touches Postgres/storage; ai-engine has no credentials
- [ ] ai-engine never receives `organization_id`
- [ ] Tenant queries go through `InTenantTx` (`set_config`, no string-formatted SQL)
- [ ] RLS: ENABLE + FORCE, `NULLIF(current_setting(...))` policy, non-owner role without BYPASSRLS
- [ ] Money: decimal only, pure `internal/pricing`
- [ ] ai-engine responses validated as untrusted
- [ ] River job args are IDs only
- [ ] Connectors are code with fixtures; Devin scope respected
- [ ] Quote reproducibility: snapshot ids and FX recorded, new version on recalculation
- [ ] None of the above

Notes:

## Tests and evidence

<!-- Paste the exact commands and their output. "make test passed" is not evidence for DB, tenancy or money changes. -->

```
$ make test
$ make test-integration   # required when touching DB, RLS, tenancy, pricing, quote state machine
```

- [ ] New or changed behavior has tests; bug fixes have a regression test
- [ ] Isolation suite run and passing (or n/a: no DB/tenancy change)

## Contracts, migrations and config

- [ ] `packages/contracts` changed and codegen is committed (or n/a)
- [ ] Migration added, applies from scratch, reversible or documented as not (or n/a)
- [ ] New env vars are in `.env.example` (names only, no values)

## Risk and rollback

<!-- What can go wrong, who is affected, how to undo it. -->

## Review notes

<!-- Where to look first; anything you are unsure about; what was deliberately left out. -->

## Checklist

- [ ] Branch name and commit messages follow `AGENTS.md`
- [ ] PR is small: one unit of behavior
- [ ] `code-reviewer` ran on the diff and its findings are fixed
- [ ] No secrets, tenant data or email content in code, fixtures or logs
- [ ] Merge with a **merge commit** (no squash, no rebase)
