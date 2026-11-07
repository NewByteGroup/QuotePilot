# contracts

Source of truth for shapes shared between `core-api`, `ai-engine` and `web`.

## What belongs here

- OpenAPI 3.1 specs and JSON Schema files.
- Codegen configuration for Go, Pydantic and TypeScript.

## What must never go here

- Hand-edited generated code.
- Changes made without Pedro's approval (see `AGENTS.md`).

## Commands

- `make contracts-drift` (from the repo root): regenerate and fail on any diff (stub for now).
