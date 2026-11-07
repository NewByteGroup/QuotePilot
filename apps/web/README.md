# web

React + TypeScript + Vite frontend for QuotePilot.

## What belongs here

- UI code under `src/`: components, pages, hooks, and their component tests.

## What must never go here

- HTTP calls that bypass the generated client from `packages/contracts`.
- Any direct database access.

## Commands

- `make test-web` (from the repo root): runs vitest (stub for now).
