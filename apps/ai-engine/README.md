# ai-engine

Python, stateless service: LLM agents, Firecrawl connectors, document rendering and
embeddings calls. Core-api is its only caller.

## What belongs here

- `agents/`: LLM agents.
- `connectors/`: connectors, code with fixture tests.
- `render/`: PDF and email HTML rendering.
- `tests/`: tests; fixtures under `tests/fixtures/<connector>/{old,new}`.

## What must never go here

- Database drivers, storage SDKs, or DB and storage credentials.
- The `organization_id` field: tenant context never reaches this service.
- Logging of request payloads.
- Money arithmetic.
- Hot-reloaded LLM-written parsers.
- Live network calls in tests.

Devin may only change `connectors/**` and `tests/**`.

## Commands

- `make test-ai` (from the repo root): runs pytest (stub for now).
