# ADR-005: Authentication, tenant resolution, the platform (pre-tenant) path and the 1-click approval token

- **Status:** Proposed
- **Date:** 2026-11-08
- **Deciders:** Pedro (backend lead). Review requested from Marcelo and Amanda.
- **Related:** ADR-001 (boundary rules), ADR-002 (`InTenantTx`), ADR-003 (roles: `qp_migrator`, `qp_app`), ADR-004 (River jobs), RFC-002 (email pipeline), RFC-003 (connectors)

## Context

Spec section 4.3 resolves the tenant at the edge through three routes: the web/API (JWT plus `X-Organization-Id`), the Resend inbound webhook (recipient address), and the 1-click approval link (signed token). Each of them has to produce an `organization_id` **before** a tenant transaction exists, so each of them touches data that is not protected by per-tenant RLS: users, memberships, organizations and inbound aliases.

ADR-003 fixed the database roles: the application role `qp_app` is not the table owner and has no `BYPASSRLS`. That rules out the easy answer ("read it with a privileged connection"). Notion comments 6 and 9 on the spec asked for an explicit pre-tenant path and for restricted access to the identity tables instead of free `SELECT`. This ADR is that path.

Constraints from the spec's guardrails: magic link plus JWT plus a memberships table is the whole authentication scope. No SSO, SAML, billing or permission trees.

Facts about Resend that shape the design (verified against its documentation on 2026-11-08): the `email.received` webhook carries metadata only, with body and attachments fetched through the Receiving and Attachments APIs; the attachment `download_url` expires after one hour; webhooks are signed with Svix headers; inbound mail should use a subdomain, because pointing a root domain at Resend routes all of its mail there.

## Decision

### 1. Identity tables and the "Layer A" rule

`organizations`, `users`, `organization_memberships` and the new `auth_login_tokens` and `auth_sessions` are **platform tables**: they carry no `organization_id` RLS policy because they are read before a tenant exists.

- After creating each platform table, the migration runs `REVOKE ALL ON app.<table> FROM qp_app`. This is required because `ALTER DEFAULT PRIVILEGES` in the init script grants `qp_app` DML on every new table in `app`.
- `qp_app` reaches platform tables only through the functions in section 2.
- CI asserts that the set of non-RLS tables in `app` equals an explicit allow-list kept in the test, and that `qp_app` holds no privilege on any of them (extends the checks of ADR-008).

### 2. The platform path: narrow `SECURITY DEFINER` functions

There is **no** `BYPASSRLS` role, no second credential and no generic "run as platform" helper. Pre-tenant work happens through a closed set of functions in schema `app`, owned by `qp_migrator`.

Hygiene rules for every such function:

1. `SECURITY DEFINER` with `SET search_path = pg_catalog, pg_temp` and fully schema-qualified object names (defeats search-path hijacking).
2. Static SQL only, typed arguments, minimal return columns, one purpose per function.
3. Explicit `GRANT EXECUTE ... TO qp_app` in the same migration. Execute on `app` functions is revoked from `PUBLIC` by default (see the init script).
4. A definer function never reads a table under tenant RLS. The owner is subject to `FORCE ROW LEVEL SECURITY` and has no tenant context, so such a read returns nothing: it fails closed.
5. Each function ships with a test that `qp_app` can execute it, cannot read the underlying table directly, and gets only the documented columns.
6. Migrations that create or alter these functions fall under the existing CODEOWNERS rule for `apps/core-api/db/migrations/**`.
7. Go access lives only in `internal/platform`, as typed methods. `depguard` forbids the generic pool elsewhere (ADR-002).

Initial set (names are the contract, signatures are fixed in the migration):

| Function | Used by | Returns |
|---|---|---|
| `platform_find_user_by_email(email)` | login | `user_id` or null |
| `platform_create_login_token(user_id, token_hash, expires_at)`, `platform_consume_login_token(token_hash)` | login | the consume call is one atomic `UPDATE ... WHERE consumed_at IS NULL AND expires_at > now() RETURNING user_id` |
| `platform_create_session`, `platform_rotate_session`, `platform_revoke_session` | sessions | session ids and revocation outcome |
| `platform_list_memberships(user_id)` | Organization Switcher | `organization_id`, `name`, `slug`, `role` |
| `platform_get_membership(user_id, organization_id)` | every authenticated request | `role` or null |
| `platform_resolve_inbound_alias(alias)` | inbound webhook | `organization_id` or null |
| `platform_register_waiter(connector_id, organization_id, rfq_id)` | pricing, inside a tenant tx | nothing; see section 6 |
| `platform_take_waiters(connector_id)` | connector repair job | `organization_id`, `rfq_id` rows |
| `platform_record_snapshot(...)` and the connector-status updates | connector jobs | nothing. Layer C tables (public data) are read-only for `qp_app`; writes go through these functions |

`platform_register_waiter` checks that its `organization_id` argument equals `current_setting('app.current_organization_id', true)`, so a tenant transaction can register only itself.

### 3. Login: magic link

- `POST /auth/login` takes an email, always answers 200 with the same body (no account enumeration), and sends the link through the platform sender (not a tenant brand).
- Token: 32 random bytes, only its SHA-256 is stored, 15-minute expiry, single use.
- Rate limit per email and per IP. In the MVP this is an in-process token bucket, which assumes a single `core-api` instance; revisit if that changes.
- The link lands on a page that performs the exchange with a `POST`, so mail scanners that prefetch links cannot consume the token.

### 4. Sessions

| | Access token | Refresh token |
|---|---|---|
| Form | JWT, HS256 (`golang-jwt/jwt` v5), signed by `core-api` and verified only by it | Opaque, 32 random bytes, stored as SHA-256 |
| Lifetime | 15 minutes | 30 days sliding |
| Claims | `sub` (user id), `sid` (session id), `exp`, `iat`; **no organization or role** | n/a |
| Transport | HttpOnly, Secure, SameSite=Lax cookie | HttpOnly, Secure cookie scoped to the refresh path |

- Refresh rotates on every use. Presenting an already-rotated refresh token revokes the whole session family.
- Signing keys carry a `kid`, so a rotation can run with two keys valid at once. The session key and the approval-token key are different keys.
- Membership and role are **not** in the JWT. Removing someone from an organization takes effect on their next request, and logout revokes the session.
- CSRF: SameSite=Lax cookies plus a required `X-Organization-Id` header on state-changing requests (a cross-site form cannot set it without a CORS preflight that the API refuses), plus an `Origin` check. This assumes the web app and the API share a registrable domain; if hosting decides otherwise (ADR-007), revisit.

### 5. Tenant resolution on API requests

For each request: verify the JWT, read `X-Organization-Id`, call `platform_get_membership(user_id, organization_id)`, put the role on the request context, then `InTenantTx(organization_id, ...)`.

- A user who is not a member, and an organization that does not exist, get the same response, so ids cannot be probed.
- The lookup runs on every request (one indexed query) and is not cached in the MVP.
- Roles are `OWNER`, `ADMIN`, `PRICING_ANALYST`. Authorization is a `RequireRole` check in the handler, not a permission tree. Proposed matrix, to confirm with Marcelo:

| Action | PRICING_ANALYST | ADMIN | OWNER |
|---|---|---|---|
| View RFQs and quotes, edit lines, approve and send | yes | yes | yes |
| Upload tariffs, edit pricing rules | no | yes | yes |
| Invite or remove members, change the inbound alias, branding | no | no | yes |

### 6. Cross-tenant work without a bypass: the waiter registry

When pricing cannot proceed because a public connector is unavailable (RFC-003), the RFQ should be reprocessed after the connector is repaired. Finding "all RFQs waiting for connector X" would normally need a cross-tenant read, which RLS correctly forbids.

Decision: while still inside its own tenant transaction, pricing calls `platform_register_waiter(connector_id, organization_id, rfq_id)`. The row lives in a platform table holding **identifiers only**. When the connector is repaired, a platform job calls `platform_take_waiters(connector_id)` and enqueues **one River job per waiting RFQ**, each carrying its own `{organization_id, correlation_id, payload}` envelope with IDs only (ADR-004). No job ever loops across tenants. The registry reveals which tenant has something waiting, never its content.

### 7. Inbound webhook

1. Read the raw body and verify the Svix signature and timestamp before parsing anything. Reject on failure.
2. For each recipient address in the event, normalise it (lower case, plus-tag removed) and call `platform_resolve_inbound_alias`.
3. Exactly one organization matches: continue. More than one distinct organization (a message addressed to two tenants): quarantine by id and do not guess. None: record the message id and a counter at platform level, store nothing tenant-scoped, and answer 2xx so the sender does not retry. Resend (Svix) retries **any** non-2xx — no 4xx/5xx distinction — eight times over ~27 hours (immediate, 5s, 5m, 30m, 2h, 5h, 10h, +10h), so an error response for a permanently undeliverable message would replay it for a day; the documented way to abort a delivery explicitly is the `webhook-delivery: abort-message` response header. Duplicates are normal under retries, which the `(organization_id, resend_message_id)` idempotent insert in step 4 already covers.
4. In `InTenantTx(organization_id)`: insert the RFQ with `UNIQUE (organization_id, resend_message_id)` and enqueue the ingest job with `InsertTx` (ADR-004). A duplicate delivery inserts and enqueues nothing.
5. The inbound alias is immutable in the MVP, so an old address cannot start resolving to a different tenant.

### 8. 1-click approval token

- A compact signed token (HMAC-SHA256, `kid`) with `{ jti, quote_id, organization_id, quote_version, approver_user_id, action: "approve", exp }`. Expiry is the smaller of 24 hours and the quote's validity.
- The `jti` is recorded in a tenant table with RLS (`approval_tokens`). Approval consumes it with `UPDATE ... SET consumed_at = now() WHERE jti = $1 AND consumed_at IS NULL AND expires_at > now() RETURNING ...` in the same transaction as the state change. A second click finds nothing.
- The token is bound to the quote **version**. A recalculation creates a new version and so invalidates every earlier link.
- **Issued only for quotes in `READY`** (every line at full confidence). A quote in `NEEDS_REVIEW` has no link and must be approved from the workspace with a session, which matches the human-in-the-loop rule of the spec (risk table, section 7).
- The link opens a confirmation page that changes nothing. Approval is a `POST` from that page. Link scanners and prefetchers therefore cannot approve anything.
- The signature is checked without touching the database; then `InTenantTx(organization_id)` does the rest. Audit records `approved_via = TOKEN | SESSION` and the approver.
- Tokens are never logged, only their `jti`.

### 9. Object storage access

The object key always comes from a row read under RLS, never from client input; uploads get a server-generated key under `tenants/{organization_id}/...`. Presigned GET URLs live five minutes and are issued after the membership check. Storage keys bypass RLS (ADR-003), so this application rule is the only barrier and is covered by tests (ADR-008).

### 10. Service-to-service: `core-api` to `ai-engine`

Private network plus a bearer token in `AI_ENGINE_SERVICE_TOKEN`, compared in constant time, with two tokens accepted at once to allow rotation. `ai-engine` never calls `core-api`; there are no callbacks. mTLS is out of scope for the MVP.

## Threats and controls

| Threat | Control |
|---|---|
| Forged `X-Organization-Id` | Membership checked on every request; uniform response for unknown and forbidden |
| Stolen refresh token | Rotation with reuse detection; session family revoked |
| Mail scanner prefetches a login or approval link | Links only open a page; the state change is a `POST` |
| Forwarded approval email | Token only for `READY` quotes; version-bound; single use; short expiry |
| Webhook replay or spoof | Svix signature and timestamp; idempotency key `(organization_id, resend_message_id)` |
| Message addressed to two tenants | Quarantine; never guess |
| Definer function abused or hijacked | Pinned `search_path`, static SQL, narrow returns, per-function tests, CODEOWNERS review |
| App role reads identity tables directly | `REVOKE ALL` on platform tables plus the CI allow-list assertion |
| Cross-tenant job loop | Waiter registry; one River job per RFQ, IDs only |

## Alternatives considered

1. **A `qp_platform` role with `BYPASSRLS`.** Simple, and the most dangerous object in the system: a second credential that can read every tenant. Rejected by ADR-003's reasoning.
2. **RLS on the identity tables, driven by a "user context" setting.** Needs a second context variable and policies for lookups that, by definition, have no context yet (login by email, alias resolution). More moving parts for the same result.
3. **Supabase Auth.** ADR-003 uses Supabase as plain Postgres in production and Cloudflare R2 for object storage; development Postgres runs on Railway. Adopting its auth schema and JWT-in-RLS pattern would put a second identity system next to ours and conflict with `InTenantTx`.
4. **Server-side sessions only, or JWT only.** Sessions-only gives up the stateless verify path the spec asked for; JWT-only cannot revoke. The short access token plus rotating refresh token covers both.
5. **Passwords.** More surface (reset, storage, breach handling) for no benefit with four pilot companies. Magic link only.

## Consequences

**Positive**
- No credential in the system can read across tenants, so a bug in application code cannot become a cross-tenant read by privilege.
- The set of things that run outside RLS is a short, named, tested list.
- The same pattern (identifiers only, one job per tenant) serves reprocessing and any later fan-out.

**Negative / risks**
- `SECURITY DEFINER` functions are code that runs with the owner's privileges. A mistake there is the main new risk. Mitigated by the hygiene rules, per-function tests and review.
- One membership lookup per request. Fine at this scale; cache later if it shows up in profiles.
- Magic link makes login depend on email delivery. Mitigation: a platform admin can issue a login link by hand during the pilot.
- The role matrix is a proposal and may need changing after pilot feedback.

## Open items

- Confirm the role matrix with Marcelo and the pilot companies.
- Whether web and API share a registrable domain (CSRF design in section 4).
- Rate limiting if `core-api` ever runs as more than one instance.
- Where login-event audit records live and how long they are kept (LGPD).
- Impersonation or support access for platform admins: not designed; out of scope until a need appears.
