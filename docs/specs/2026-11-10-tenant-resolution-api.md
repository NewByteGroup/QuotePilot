# Tech spec — tenant resolution and session API (`internal/platform`, `internal/auth`)

- **Status:** Draft for implementation — **gated on ADR-005, which is still
  Proposed**. Pedro's sign-off on the ADR and on the two SPEC DECISION
  REQUIRED items (§3.3, §8.2) precedes PR A. Open items are flagged, never
  silently decided.
- **Date:** 2026-11-10
- **Author:** Devin (for Pedro)
- **Governs:** the `apps/core-api` request edge — migration 0003 (Layer A
  auth tables + accessor functions), `internal/platform` (config, JWT,
  tokens, typed function accessors, middleware), `internal/auth` (session
  endpoints), `cmd/server` wiring, and the first slice of
  `packages/contracts/openapi/core-api.yaml`
- **Sources:** ADR-005 §§1–5 (Proposed) — the governing document;
  data-model spec §3/§5/§9 (Layer A conventions; its open items defer
  auth/session functions to this mission); migration 0002 (the *shipped*
  naming and grant convention); ADR-002 (ServeMux, oapi-codegen, library
  allow-list, `InTenantTx`); AGENTS.md rules 3, 4, 6, 7

## 1. Purpose and hard rules

The goal is the **first HTTP slice that proves request → JWT → tenant →
`InTenantTx` end to end**: a caller logs in by magic link, holds a short
access JWT plus a rotating refresh cookie, names an organization in a
header, and reaches a tenant transaction whose RLS context was set by the
middleware chain — with every pre-tenant lookup going through narrow
`SECURITY DEFINER` functions, never a table read.

Rules that must never bend:

- **`qp_app` never touches Layer A tables.** `auth_login_tokens` and
  `auth_sessions` join `organizations`, `users` and
  `organization_memberships` behind `REVOKE ALL` (ADR-005 §1). All access
  is through the function family in §5 — the same function-gated pattern
  migration 0002 already shipped.
- **Membership is checked on every request** (ADR-005 §5) — one indexed
  `get_membership` call per tenant-scoped request, never cached, and role
  and organization are **never in the JWT**. Removing a member takes
  effect on their next request.
- **Uniform failures.** A non-member and a nonexistent organization get
  byte-identical responses (§7); every authentication failure is the same
  401 body. Nothing in a response may reveal whether an email, an
  organization or a token exists.
- **Only hashes are stored.** Login tokens and refresh tokens are 32
  bytes from `crypto/rand`; Postgres keeps their SHA-256 hex, the token
  itself lives only in the email link or the cookie. Tokens are never
  logged — a `jti`-style identifier is the most a log line may carry.
- **Links change nothing on `GET`.** The magic link and any future
  approval link open a page; the state change is a `POST` the page makes,
  so mail scanners prefetching links cannot consume or approve anything
  (ADR-005 §3, §8).
- **Every definer function ships with its grant and its test** in the
  same migration and PR (ADR-005 §2 hygiene): `SECURITY DEFINER`, pinned
  `search_path`, static SQL, typed arguments, minimal return columns, one
  purpose each — plus the three allowlist locations updated together
  (§3.4).
- **No new dependencies.** `golang-jwt/jwt` v5 is the ADR-002 choice;
  the rate-limit bucket is hand-rolled (`golang.org/x/time/rate` is not
  on the allow-list — adding it needs Pedro's approval and an ADR-002
  edit, same as any library).

## 2. PR split and dependency order

One unit of behavior per PR, ~500 lines of non-generated code as the
sweet spot (AGENTS.md, small-PR rule):

| PR | Branch | Content | Depends on |
|---|---|---|---|
| A | `feat/core-auth-session-schema` | Migration 0003: two Layer A tables + the §5 function family + allowlist triplet | ADR-005 sign-off; §3.3 decision; §3.1 naming ratification |
| B | `feat/core-platform-package` | `internal/platform`: config, JWT/token helpers, typed accessors | A |
| B2 | `feat/core-tenant-middleware` | `Authenticate` → `ResolveTenant` → `RequireRole` chain + its httptest proofs | B |
| C0 | `feat/contracts-core-api-openapi` | `packages/contracts/openapi/core-api.yaml` + oapi-codegen plumbing (recommended — see §8.2) | none; can run parallel to A/B |
| C1–C6 | `feat/core-auth-<endpoint>` | One endpoint per PR (AGENTS.md small-PR rule), in order: `login`, `session`, `refresh`, `logout`, `me`, `me/organization` | B2 (and C0 if codegen is used) |

`C6` (`GET /me/organization`) is the slice that proves the whole chain
end to end, so it lands last. Deferred explicitly: rewiring the Resend webhook to
`resolve_inbound_org`, 1-click approval tokens, the waiter registry, and
admin/teardown endpoints (§10).

## 3. Migration 0003 — Layer A auth tables

### 3.1 Naming: shipped convention vs ADR-005 drift

Migration 0002 established the convention the schema lint enforces:
**unprefixed** names are the tenant-facing request-path surface granted
to `qp_app` (`resolve_inbound_org`, `list_memberships_for_email`,
`get_current_organization`); the **`platform_` prefix is reserved for
`qp_admin`-only** writes and teardown (`platform_insert_public_snapshot`,
`platform_delete_organization`, …). ADR-005 §2's function table predates
that split and calls every pre-tenant function `platform_*` — under the
shipped convention that name would mean "grant to `qp_admin`", which is
wrong: the login/session/membership functions run on request paths where
the only credential is `qp_app`.

This spec fixes the drifted names (§5 has the full signatures):

| ADR-005 §2 name (drifted) | This spec | Granted to |
|---|---|---|
| `platform_find_user_by_email` | `find_user_by_email` | `qp_app` |
| `platform_create_login_token` / `platform_consume_login_token` | `create_login_token` / `consume_login_token` | `qp_app` |
| `platform_create_session` / `platform_rotate_session` / `platform_revoke_session` | `create_session` / `rotate_session` / `revoke_session` | `qp_app` |
| `platform_get_membership` | `get_membership` | `qp_app` |
| `platform_list_memberships` | see §3.3 — decision required | `qp_app` |

ADR-005's `platform_*` entries that are genuinely `qp_admin` work —
waiter registry and Layer C snapshot writes — keep their prefix when
their own mission lands; they are out of scope here.

> **SPEC DECISION REQUIRED (naming ratification):** amend ADR-005 §2's
> table to the names above so the ADR, the lint allowlists and the
> migrations cannot drift apart again. Until then this spec and ADR-005
> disagree deliberately, and this spec wins because it follows the
> *shipped* convention.

### 3.2 Tables

```sql
CREATE TABLE app.auth_login_tokens (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     uuid NOT NULL REFERENCES app.users (id),   -- NO ACTION: audit preservation, data-model spec §9
    token_hash  text NOT NULL UNIQUE,                      -- SHA-256 hex of the 32-byte token
    expires_at  timestamptz NOT NULL,
    consumed_at timestamptz,                               -- NULL until the single use
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE app.auth_sessions (
    id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id            uuid NOT NULL REFERENCES app.users (id),
    family_id          uuid NOT NULL,                      -- one family per login; rotation descends
    refresh_token_hash text NOT NULL UNIQUE,               -- SHA-256 hex of the current generation
    expires_at         timestamptz NOT NULL,               -- sliding 30 days, reset on each rotation
    rotated_at         timestamptz,                        -- set when this generation was exchanged
    revoked_at         timestamptz,                        -- logout or family kill on reuse
    created_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX auth_sessions_family ON app.auth_sessions (family_id);

REVOKE ALL ON app.auth_login_tokens, app.auth_sessions FROM qp_app;
```

Both carry no `organization_id` — they are read before a tenant exists,
so they are Layer A platform tables (ADR-005 §1): no RLS, no `qp_app`
privileges, access only through §5 functions. No IP or user-agent columns
are collected pending the LGPD login-audit decision (§11) — data
minimization until retention rules exist. Users are provisioned by
seeding/`qp_migrator` during the pilot; there is no self-signup and
`find_user_by_email` must stay the only email→id lookup.

### 3.3 The org-switcher gap — SPEC DECISION REQUIRED

ADR-005's `platform_list_memberships(user_id)` returned
`organization_id, name, slug, role` for the Organization Switcher. The
shipped `list_memberships_for_email(p_email)` returns only
`(organization_id, role)` — not enough to render a picker.

- **Recommended:** extend `list_memberships_for_email` to
  `RETURNS TABLE (organization_id uuid, name text, slug text, role
  app.membership_role)` via a join on `app.organizations` — the switcher
  gets display names in the same single lookup, no second query, and no
  tenth function. Cost: a return-type change needs `DROP FUNCTION` +
  recreate (not `CREATE OR REPLACE`) with the grant re-issued; the lint
  signature `app.list_memberships_for_email(text)` is unchanged, but the
  enforcement test's row shape must be updated. `GET /me` resolves the
  email through `get_user_profile` anyway, so the email-keyed signature
  is not a real constraint.
- Alternative: a separate `list_memberships(p_user_id uuid)` with the
  four-column return. Adds a function but keeps the shipped one
  untouched.

**Decide before PR A** — the migration text and the enforcement test
both depend on it.

### 3.4 The allowlist triplet moves with the migration

The same PR must update every place the layer model is asserted, or CI
fails on a table it was never told about:

- `internal/tenantdb/schema_lint_test.go`: `platformTablesNoRLS` +=
  `auth_login_tokens`, `auth_sessions`; `layerATables` += both;
  `tenantFunctions` += the §5 signatures; the definer-set assertion then
  covers them automatically.
- `infra/postgres/checks/platform-functions.sql`: extend the surface
  assertions so `make db-check` proves the new `qp_app` grants and keeps
  `qp_admin` pinned to the platform five (the existing catalog-diff
  already flags any `qp_admin` grant on the new functions as drift);
  matching mutation cases in `infra/postgres/checks/negative-tests.sh`.
- `tenantdb.assertRLSForced` and `roles.sql` [M] are **unchanged** — the
  new tables carry no `organization_id`, so they never enter that
  allowlist. Any migration that gives them one is a bug.

## 4. Sessions and tokens — the crypto contract (ADR-005 §3–4)

| | Access token | Refresh token |
|---|---|---|
| Form | JWT, HS256 (`golang-jwt/jwt` v5), `kid` header | Opaque, 32 bytes `crypto/rand`, base64url in the cookie |
| Stored | nowhere (stateless) | SHA-256 hex in `auth_sessions` |
| Lifetime | 15 minutes | 30 days, sliding on every rotation |
| Claims | `sub` (user id), `sid` (session id), `exp`, `iat` — **no org, no role** | n/a |
| Transport | `qp_access` cookie: HttpOnly, Secure, `SameSite=Lax`, `Path=/` | `qp_refresh` cookie: HttpOnly, Secure, `SameSite=Strict`, `Path=/auth/refresh` |

- **Rotation:** every `POST /auth/refresh` exchanges the presented
  refresh token for the next generation in the same `family_id` and
  slides `expires_at` to now+30d. Presenting an already-rotated
  (`rotated_at IS NOT NULL`) or already-revoked token is **reuse**: the
  whole family is revoked in the same atomic call (§5
  `rotate_session`), the cookies are cleared, and the client gets the
  uniform 401.
- **Keys:** signing keys carry a `kid`; verification accepts any key in
  the configured set while signing uses the active one, so rotation runs
  with two keys valid at once. The session key family is **not** the
  future approval-token key (ADR-005 §4). Verification rejects any alg
  other than HS256 — `alg=none` and algorithm-confusion tokens fail like
  any other forgery.
- **Revocation lag, stated plainly:** logout revokes the session row, so
  the refresh path dies immediately; an access JWT still verifies until
  its `exp` (≤ 15 min) because `Authenticate` is deliberately stateless.
  A per-request session-liveness function is *not* added in the MVP —
  noted as a deliberate residual, not an oversight.
- **`SameSite=Strict` on the refresh cookie** is this spec's tightening:
  the cookie only ever travels to our own `POST /auth/refresh`, so Strict
  costs nothing; ADR-005 specified the path scoping without pinning the
  flag.

## 5. Function family (migration 0003, all `SECURITY DEFINER`, all → `qp_app`)

Same hygiene as 0002: `SET search_path = app`, `p_`-prefixed parameters
(the `LANGUAGE sql` column-shadows-parameter trap is documented in 0002),
explicit `REVOKE ... FROM PUBLIC` + `GRANT ... TO qp_app` per function,
`STABLE` for reads. `qp_admin` receives **no** grant on any of them —
the platform-functions.sql catalog diff fails CI if one appears.

| Function | Returns | Semantics |
|---|---|---|
| `find_user_by_email(p_email text)` | `uuid` or NULL | `STABLE`. `WHERE email = lower(btrim(p_email))`; unknown → NULL. |
| `create_login_token(p_user_id uuid, p_token_hash text, p_expires_at timestamptz)` | `uuid` (token id) | Insert only; the caller computed the hash and the 15-min expiry. |
| `consume_login_token(p_token_hash text)` | `uuid` or NULL | One atomic `UPDATE ... SET consumed_at = now() WHERE token_hash = $1 AND consumed_at IS NULL AND expires_at > now() RETURNING user_id` (ADR-005 §2 verbatim) — the single-use guarantee is the row lock. |
| `create_session(p_user_id uuid, p_refresh_token_hash text, p_expires_at timestamptz)` | `uuid` (session id) | Inserts generation 0 with `family_id = id` — the family is rooted at its first session. |
| `rotate_session(p_presented_hash text, p_new_hash text, p_new_expires_at timestamptz)` | `TABLE(session_id uuid, user_id uuid, rotated boolean)` | `SELECT ... FOR UPDATE` the row by hash, then exactly one of: **empty set** — unknown hash, revoked, or `expires_at <= now()`; **`rotated=false`** — `rotated_at` already set (reuse): revoke the whole family (`UPDATE ... SET revoked_at = now() WHERE family_id = $fam`) and return the row with NULL ids; **`rotated=true`** — stamp `rotated_at`, insert the successor in the same `family_id` with `p_new_hash`/`p_new_expires_at`, return its id and the user. Consume-and-create are atomic by construction — no TOCTOU window. |
| `revoke_session(p_session_id uuid)` | `boolean` | `UPDATE ... SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`; logout. The JWT's `sid` is always the family's live generation, so single-row revoke is sufficient — prior generations are already rotated dead. |
| `get_membership(p_user_id uuid, p_organization_id uuid)` | `app.membership_role` or NULL | `STABLE`. The per-request tenant check; NULL indistinguishably covers non-member and nonexistent org — that indistinguishability is the security property. |
| `get_user_profile(p_user_id uuid)` | `TABLE(id uuid, email text, full_name text)` | `STABLE`. **Spec addition beyond ADR-005's table** — `GET /me` needs it; minimal columns, same argument-trust model as `get_membership`. |
| `list_memberships_for_email(p_email text)` | extended to `(organization_id, name, slug, role)` — per §3.3 | Org-switcher data in the existing lookup; drop + recreate in this migration. |

## 6. `internal/platform` package (PR B)

Everything pre-tenant or request-edge, as typed methods — the only Go
package allowed to name the platform path (ADR-005 §2 rule 7; depguard
keeps the raw pool out of everywhere else):

```
internal/platform/
  config.go      — Config{AppDSN, AdminDSN, WebURL, JWTKeys{kid→secret},
                   ActiveKID, AccessTTL=15m, RefreshTTL=30d, LoginTTL=15m,
                   ResendAPIKey, MagicFrom, CookieSecure}; LoadConfig(env)
  jwt.go         — SignAccess(userID, sessionID, now), VerifyAccess(raw)
                   → claims; kid header; HS256 pinned; exp/iat enforced
  token.go       — NewOpaqueToken() (32B crypto/rand → base64url),
                   HashToken() SHA-256 hex — the only two shapes in play
  store.go       — Store over a narrow Querier interface (satisfied by
                   *pgxpool.Pool and pgx.Tx): typed wrappers for every §5
                   function, NULL → (zero, false)
  middleware.go  — Authenticate, ResolveTenant, RequireRole + private
                   ctx keys + Accessors UserID/SessionID/OrgID/Role +
                   the uniform writeError
  ratelimit.go   — hand-rolled token bucket: map+mutex, clock injected;
                   per-email and per-IP limiters for /auth/login
  mail.go        — MailSender interface (SendMagicLink(ctx, to, url));
                   Resend-backed impl; FakeMailSender lives in tests
```

Env names land in `.env.example` in the same PR: `AUTH_JWT_KEYS`
(`kid=secret` comma-separated, first = active), `AUTH_JWT_ACTIVE_KID`,
the three TTLs, `AUTH_COOKIE_SECURE` (default true; local-http escape),
`RESEND_API_KEY`, `AUTH_MAGIC_FROM`. `POSTGRES_DSN`, `QP_ADMIN_DSN` and
`WEB_URL` already exist.

## 7. Middleware chain and the uniform-failure contract

`Authenticate` → `ResolveTenant` → `RequireRole(...)` — hand-written
wrappers on stdlib `ServeMux` (ADR-002: no router; `inbox.NewServer`
sets the per-surface mux precedent).

- `Authenticate`: read `qp_access`, verify signature + `exp` with any
  configured `kid`, put `sub`/`sid` on the context. **No DB.**
  Missing/malformed/expired/wrong-kid/forged → the same 401
  `{"error":"unauthenticated"}`.
- `ResolveTenant`: read `X-Organization-Id`; absent or non-UUID on a
  tenant-scoped route → 400 `{"error":"missing_organization"}` (the
  header is part of the CSRF posture — a cross-site form cannot set it,
  ADR-005 §4). Call `get_membership(sub, org)`; NULL → **404
  `{"error":"not_found"}` — byte-identical for non-member and
  nonexistent org**, status, body and shape. Membership → role + org on
  the context.
- `RequireRole(roles...)`: role on context must be listed, else 403
  `{"error":"forbidden"}`. A 403 here leaks nothing — the caller already
  proved membership to reach it. Roles are `OWNER | ADMIN |
  PRICING_ANALYST`; the action matrix stays the ADR-005 proposal pending
  Marcelo (§11).
- Timing: the 404-vs-404 pairing must not differ in a way an oracle can
  use — same code path, same lookup, same response writer; no early
  "org exists" shortcut.

## 8. HTTP surface (`internal/auth`, PRs C1–C6)

| Route | Chain | Behavior |
|---|---|---|
| `POST /auth/login` | rate limit → handler | `{email}` → **always 200 `{"status":"ok"}`** for any well-formed email. If the user exists: `create_login_token` + `MailSender` with `${WEB_URL}/auth/exchange?token=…`. The platform sender, not a tenant brand (ADR-005 §3). Malformed body → 400. Bucket exhausted → 429. |
| `POST /auth/session` | — | `{token}` → `consume_login_token`; success → `create_session` + sign JWT + set both cookies → 200 with the profile + memberships payload the switcher needs. Used/expired/unknown → uniform 401. |
| `POST /auth/refresh` | — | `qp_refresh` cookie → `rotate_session`; `rotated` → new JWT + new scoped cookie; reuse → family already revoked + clear cookies + 401; empty → 401 + clear cookies. |
| `POST /auth/logout` | — (refresh cookie) | Reads `qp_refresh` — still valid after `qp_access` expires — and calls `revoke_session` for the session family; always 204 + clears both cookies, valid token or not (no validity oracle). |
| `GET /me` | `Authenticate` | `get_user_profile` + the §3.3 membership list → `{id, email, full_name, memberships:[…]}`. |
| `GET /me/organization` | `Authenticate` + `ResolveTenant` | **The end-to-end proof**: inside `InTenantTx(org)` call `get_current_organization()` → `{id, name, slug, inbound_email_alias, branding_json}` — the first endpoint where request → JWT → tenant → tenant transaction runs for real. |

`cmd/server/main.go` composes this mux with the existing
`inbox.NewServer` (the webhook keeps its payload-carried org until its
own rewiring PR — see §10) and `http.Server` with sane timeouts.

### 8.1 Rate limiting

Hand-rolled bucket, `ratelimit.go`: proposed defaults
**5 tokens / email / hour** and **30 requests / IP / hour** on
`/auth/login` only — single-use tokens and uniform responses make the
other endpoints poor abuse targets for the MVP. In-process state assumes
one `core-api` instance (ADR-005 §3); the multi-instance consequence is
an open item (§11).

### 8.2 OpenAPI / codegen — PR C0 recommended

ADR-002 already chose `oapi-codegen` (`std-http-server` + `strict-server`,
generated code committed, `contracts-drift` diffs it). The webhook spike
hand-rolled its mux because no contract existed; the auth surface is the
first one that *can* start on a contract. **Recommendation:** dedicate
PR C0 to `packages/contracts/openapi/core-api.yaml` (the six endpoints,
cookie security scheme, shared error shape) plus the codegen config and
Makefile wiring, so each handler PR stays small. Falling back to
hand-rolled handlers is viable but is drift from ADR-002 that Pedro must
bless explicitly.

## 9. Testing contract

TDD per AGENTS.md; DB, tenancy and session behavior are proven against
**real Postgres** (`testenv.Open`/`AdminDB`/`MigratorPool` — never a
mock, never `-short`-skippable when run via `make test-integration`).
Allowlists move with the migration in the same PR (§3.4).

**Migration / function proofs (as `qp_app` via pool; seeds as
`qp_migrator`):** each §5 function executes for `qp_app` and is denied to
`qp_admin` and PUBLIC; direct `SELECT` on both new tables is denied;
`consume_login_token` is single-use (second consume → NULL) and refuses
expired/unknown hashes; `rotate_session` happy path descends the family,
reuse revokes every generation, revoked/expired/unknown → empty;
`revoke_session` kills the refresh path; `get_membership` returns the
role for a member and NULL for non-member *and* nonexistent org alike;
`get_user_profile` returns only the documented columns; the extended
list returns `name`+`slug`.

**Middleware proofs (httptest + fake store):** forged JWT → uniform
401 (rejected by `Authenticate` before any tenant lookup); non-member
and nonexistent org produce an **identical** 404 (status and body
byte-equal); a member removed between requests is refused on the next
one (nothing cached); missing `X-Organization-Id` on a mutation → 400;
`RequireRole` refuses a `PRICING_ANALYST` on an `OWNER`-only route with
403 while admitting all roles on `/me/organization`.

**Session proofs:** expired and wrong-`kid` JWTs → uniform 401; refresh
rotates (new pair set, old cookie dead); presenting the rotated token
again → 401 *and* the successor is dead too (family revoked); logout →
subsequent refresh → 401.

**Login proofs:** existing and unknown emails get an identical 200 body;
the fake sender received a link only for the existing user; the link's
token works exactly once through `POST /auth/session`; an expired token
→ 401; the bucket returns 429 after its limit.

**Verification commands (house standard — every PR, recorded in the PR
body):**

```
cd apps/core-api
go test -count=1 -race ./...
go vet ./... && gofmt -l .
go build ./...
make db-check          # from repo root — roles.sql + platform-functions.sql
make db-check-negative # mutation cases still trip the assertions
```

## 10. Explicit non-goals (this spec does not solve)

- **Webhook rewiring** to `resolve_inbound_org`: the spike shortcut that
  trusts `payload.organization_id` stays until its own PR removes it —
  this spec changes nothing on `/webhooks/resend/inbound`.
- **1-click approval tokens** (ADR-005 §8): separate key family,
  `approval_tokens` tenant table, `READY`-gate — its own mission.
- **Waiter registry** (`platform_register_waiter`/`platform_take_waiters`)
  and all `qp_admin` `platform_*` write functions beyond 0002's set.
- **Admin/teardown/member-management endpoints**: invites, member
  removal and `platform_delete_organization` stay DB-only for now.
- **Org onboarding and the exchange page itself** — `apps/web` owns the
  page that POSTs the token; this spec only fixes the POST contract.
- **CORS beyond the Origin check**, presigned storage URLs, service
  auth to `ai-engine` (already decided in ADR-005 §9–10, lands with
  those features).

## 11. Open items (recorded, not decided here)

- **ADR-005 remains Proposed** — Pedro's sign-off gates PR A; this spec
  encodes §§1–5 and flags where it deliberately departs (§3.1 names,
  §3.3 switcher, §8.2 codegen ordering).
- **SPEC DECISION REQUIRED — org switcher:** extend
  `list_memberships_for_email` (recommended, §3.3) or add
  `list_memberships(user_id)`.
- **SPEC DECISION REQUIRED — contract plumbing:** dedicated PR C0 for
  OpenAPI + oapi-codegen (recommended) vs hand-rolled handlers this
  slice (drift from ADR-002 Pedro must bless).
- **Function naming convention ratification:** amend ADR-005 §2's table
  to the unprefixed/`platform_` split migration 0002 shipped.
- **Role matrix confirmation** with Marcelo and the pilot companies —
  the ADR-005 §5 proposal stands unconfirmed.
- **Shared registrable domain** between web and API — the SameSite=Lax +
  `X-Organization-Id` + Origin CSRF posture assumes it (ADR-005 §4,
  ADR-007 territory).
- **Multi-instance rate limiting** — the in-process bucket assumes a
  single `core-api` instance.
- **LGPD login-audit retention** — where login-event audit records live
  and how long they are kept; until decided, no IP/UA columns exist
  (§3.2).
- **Impersonation / support access** for platform admins — not
  designed; out of scope until a need appears (ADR-005 open item).
- **Access-token revocation lag** (≤ 15 min after logout, §4) — accepted
  residual; add a per-request session-liveness function only if Pedro
  wants hard logout.
