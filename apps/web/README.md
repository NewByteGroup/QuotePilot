# web

React + TypeScript + Vite frontend for QuotePilot (Tailwind CSS 4 + shadcn/ui).

## What belongs here

- UI code under `src/`: components, pages, hooks, and their component tests.
- `src/api/`: the typed HTTP client built from `@quotepilot/contracts` (`openapi-fetch`).
- `src/mocks/`: the contract mock (MSW + `openapi-msw`), so the UI runs without `core-api`.

## What must never go here

- HTTP calls that bypass the generated client from `packages/contracts`.
- Hand-written types for API payloads (use `Schemas[...]` from `src/api/client.ts`).
- Any direct database access.

## Commands

Requires Node 22 (`.nvmrc`) and pnpm (`corepack enable`). Run `pnpm install` from the repo root.

- `pnpm dev`: dev server on http://localhost:5173 with the contract mock on.
- `pnpm build` / `pnpm preview`: production build and local preview.
- `pnpm lint`: eslint, prettier --check and tsc.
- `pnpm test` or `make test-web` (from the repo root): vitest.

## Environment

- `VITE_API_MOCK=false`: disable the contract mock and call a real `core-api`.
- `VITE_API_BASE_URL`: API base URL (default `/api`).
