# openrouter connector

Client for **OpenRouter**, covering the three API surfaces QuotePilot's
agents need (ADR-006 model matrix):

- **Decisions** — `POST /api/alpha/decisions`: the TypeSafe/Jev "System
  One" decision API (`typesafe/jev-1.14`) used by the classifier and the
  confidence auditor. Requests carry arbitrary JSON `state` plus a
  `questions` map (`noul` / `choice` / `score` / `box`); `answers` come
  back as a discriminated union on `type`.
- **Chat completions** — `POST {base_url}/chat/completions`
  (OpenAI-compatible): used by the RFQ extractor; `response_format`
  passes a JSON schema through for structured output.
- **Embeddings** — `POST {base_url}/embeddings`: used by similarity
  lookups (`qwen/qwen3-embedding-8b`, 1024 dims). `embed()` requires the
  caller to pass `expected_dimensions` and raises `OpenRouterSchemaError`
  when a returned vector has a different length — a mismatch means the
  upstream model is not the one the pipeline was configured for.

## Usage

```python
async with OpenRouterClient.from_env() as client:
    decision = await client.decide(decide_request)
    answer = decision.answers["is_bug"]  # NoulAnswer — check isinstance
    decision.usage.cost  # Decimal (USD), never float

    reply = await client.chat_completion(chat_request)
    text = reply.choices[0].message.content  # str | None — presence-check

    vectors = await client.embed(embed_request, expected_dimensions=1024)
    vectors.data[0].embedding  # list[float], len == 1024 guaranteed
```

## Auth

`Authorization: Bearer <OPENROUTER_API_KEY>` on every request. The key is
issued at openrouter.ai/keys.

## Configuration (env vars)

| Var | Required | Default |
|---|---|---|
| `OPENROUTER_API_KEY` | yes | — OpenRouter API key (`sk-or-...`) |
| `OPENROUTER_BASE_URL` | no | `https://openrouter.ai/api/v1` |
| `OPENROUTER_DECISIONS_URL` | no | `https://openrouter.ai/api/alpha/decisions` |
| `OPENROUTER_TIMEOUT_SECONDS` | no | `30` |

The vars are registered (names only) in the root `.env.example`.

## Errors

`OpenRouterTransportError` (network), `OpenRouterAuthError` (401/403),
`OpenRouterApiError` (other non-2xx, carries `status` and the parsed
`{"error": {"message", "code", "metadata"}}` body when it matches),
`OpenRouterSchemaError` (success body violating the schema, or an
embedding with the wrong dimensionality), `OpenRouterConfigError`
(missing/empty `OPENROUTER_API_KEY`, bad timeout value).

No retries and no payload logging on purpose — River owns retry policy
and tenant content must not reach logs.

## Tests

Synthetic fixtures in `tests/fixtures/openrouter/old/` (real recorded
replies are not committed per RFC-003 / ADR-008).
`FakeOpenRouterTransport` injects canned responses for all three
endpoints — no live network.
