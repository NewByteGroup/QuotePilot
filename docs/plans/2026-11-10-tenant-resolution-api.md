# Plan — tenant resolution and session API

- **Spec:** `docs/specs/2026-11-10-tenant-resolution-api.md` (read first —
  every rule below resolves to it; ADR-005 §§1–5 is the governing source)
- **Branches:** one per PR, all off `develop` — `feat/core-auth-session-schema`
  (A), `feat/core-platform-package` (B), `feat/core-tenant-middleware` (B2),
  `feat/contracts-core-api-openapi` (C0, recommended), then one
  `feat/core-auth-<endpoint>` per route (C1–C6)
- **Scope:** `apps/core-api` request edge + the two infra check files.
  No webhook changes, no approval tokens, no waiter registry, no `apps/web`
  work — spec §10 non-goals stay out.
- **Gate:** ADR-005 is Proposed and the spec carries three SPEC DECISION
  REQUIRED items (§3.1 function naming ratification, §3.3 switcher
  function, §8.2 contract plumbing). **PR A does not open until Pedro
  answers all three.**

## Constraints that apply while coding

- Files < 500 lines, functions 4–20 lines; stdlib `ServeMux` only (ADR-002:
  no router). `golang-jwt/jwt` v5 is the only new dependency; the rate
  bucket is hand-rolled — `x/time/rate` is not on the allow-list.
- Every `SECURITY DEFINER` function in one migration ships with its
  `REVOKE … FROM PUBLIC` + `GRANT … TO qp_app` and its real-Postgres test;
  `p_`-prefixed parameters (0002's column-shadows trap); `SET search_path
  = app`; `STABLE` on reads. `qp_admin` gets nothing new.
- Token discipline: 32 bytes from `crypto/rand`; only SHA-256 hex reaches
  Postgres; never log a token or a cookie value.
- Uniformity is a security property, not a style: non-member vs
  nonexistent org → byte-identical 404; every auth failure → identical
  401; login → identical 200 body. Write the tests *for* the uniformity,
  not just the happy path.
- The allowlist triplet (§3.4) moves inside PR A: lint test lists,
  `platform-functions.sql`, `negative-tests.sh`. `assertRLSForced` /
  `roles.sql` [M] stay untouched — the new tables have no
  `organization_id`.
- TDD: failing test first for every function, middleware rule and
  endpoint behavior. DB proofs run as `qp_app` via `testenv.Open`, seeds
  via `testenv.MigratorPool` — never mocked.
- `rotate_session` keeps consume+create inside one function — do not
  split it into a read-then-write pair in Go (TOCTOU on reuse detection).

## File layout

```
# PR A — migration 0003
apps/core-api/db/migrations/0003_auth_session.sql
apps/core-api/internal/tenantdb/schema_lint_test.go          # allowlists
apps/core-api/internal/tenantdb/schema_enforcement_test.go   # + auth fn proofs
infra/postgres/checks/platform-functions.sql                 # surface assertions
infra/postgres/checks/negative-tests.sh                      # new mutation cases

# PR B — internal/platform
apps/core-api/internal/platform/{config,jwt,token,store,middleware,ratelimit,mail}.go
apps/core-api/internal/platform/{jwt,token,middleware,ratelimit}_test.go
apps/core-api/internal/platform/store_integration_test.go    # real-Postgres accessors
.env.example                                                 # AUTH_* / RESEND_* names

# PR C0 (recommended) — contract plumbing
packages/contracts/openapi/core-api.yaml
apps/core-api/oapi-codegen.yaml                              # std-http-server (ADR-002)
apps/core-api/internal/auth/authapi/                         # generated, committed
apps/core-api/Makefile                                       # generate += oapi-codegen
Makefile                                                     # contracts-drift gains this path

# PR C1–C6 — one endpoint per PR; cumulative layout
apps/core-api/internal/auth/{server,login,session,refresh,logout,me}.go
apps/core-api/internal/auth/*_test.go                        # incl. e2e vs real Postgres
apps/core-api/cmd/server/main.go                             # C1: compose auth + inbox muxes
```

## Task order (executor-dispatchable, one task per PR)

1. **PR A — migration 0003 + allowlist triplet.** Tables per spec §3.2;
   functions per §5 with paired grants; the §3.3 decision applied
   (recommended: drop + recreate `list_memberships_for_email` with the
   four-column return and update its enforcement rows). Extend the three
   lint lists, `platform-functions.sql` (new `qp_app` grants asserted,
   `qp_admin` still pinned to the platform five) and matching
   `negative-tests.sh` mutations (grant a new fn to `qp_admin` → fail;
   revoke one from `qp_app` → fail).
   *Tests:* every §5 behavior in the spec §9 "migration proofs" list,
   real Postgres, `qp_app` credential.
   *Accept:* `make db-up && make -C apps/core-api integration-test` (or
   `go run ./cmd/migrate && go test -count=1 ./internal/tenantdb/...`),
   `make db-check`, `make db-check-negative`, `go build ./...`.

2. **PR B — `internal/platform`.** Config + env names; `jwt.go` sign/
   verify with `kid` set (HS256 pinned, alg-confusion rejected);
   `token.go` (32B `crypto/rand`, SHA-256 hex); `store.go` typed
   accessors over a `Querier` interface satisfied by `*pgxpool.Pool` and
   `pgx.Tx`; `ratelimit.go` bucket with injected clock; `mail.go`
   `MailSender` interface + Resend impl + `FakeMailSender` for tests.
   *Tests:* jwt round-trip / wrong-key / wrong-kid / expired / forged;
   token shape+uniqueness; bucket exhaust+refill; store accessors against
   real Postgres incl. NULL → `(zero,false)`.
   *Accept:* `go test -count=1 -race ./internal/platform/...`,
   `go vet ./... && gofmt -l .`, `go build ./...`; integration half under
   `make -C apps/core-api integration-test`.

2b. **PR B2 — tenant middleware.** `middleware.go` `Authenticate` →
   `ResolveTenant` → `RequireRole` with private ctx keys and the uniform
   writers; ships alone with its test matrix so the mission's core gets
   its own review.
   *Tests:* the spec §9 matrix in full — forged JWT → uniform 401;
   non-member and nonexistent org → identical 404 (status and body
   byte-equal); removed member refused on the next request; missing
   `X-Organization-Id` on a mutation → 400; `RequireRole` 403.
   *Accept:* `go test -count=1 -race ./internal/platform/...`,
   `go vet ./... && gofmt -l .`.

3. **PR C0 (recommended) — contract plumbing.** Write
   `packages/contracts/openapi/core-api.yaml` for the six routes (cookie
   auth scheme, shared error shape, header param `X-Organization-Id` on
   tenant routes); add `oapi-codegen.yaml` (`std-http-server` +
   `strict-server` per ADR-002); commit the generated interface under
   `internal/auth/authapi/`; wire `make -C apps/core-api generate` and
   make root `contracts-drift` regenerate and fail on diff for this
   path. No handlers.
   *Accept:* regen produces zero diff (`make generate && git diff
   --exit-code`), `go build ./...`, spec lints clean (the lint job's
   OpenAPI step).

4. **PR C1 — `cmd/server` + `POST /auth/login`.** `cmd/server/main.go`
   composes the auth mux with `inbox.NewServer` (the webhook keeps its
   payload-carried org until its own PR, spec §10) plus sane
   `http.Server` timeouts; then the first endpoint: `{email}` → uniform
   200, bucket, `MailSender` with the exchange link.
   *Tests:* identical 200 bodies for known/unknown/malformed-adjacent
   emails, 400 on malformed JSON, 429 on bucket exhaustion, fake sender
   receives the token link only when the user exists.
   *Accept:* `go test -count=1 -race ./internal/auth/...`, `go build
   ./...`; `cmd/server` boots against the dev DSN and serves the webhook
   unchanged.

5. **PR C2 — `POST /auth/session`.** `{token}` → `consume_login_token` →
   `create_session` + JWT + both cookies → 200 with the switcher payload;
   used/expired/unknown → uniform 401.
   *Tests:* single-use consume, expiry rejection, cookie flag assertions
   (HttpOnly/Secure/SameSite/path), no org/role claims inside the JWT.
   *Accept:* `go test -count=1 -race ./internal/auth/...` against real
   Postgres.

6. **PR C3 — `POST /auth/refresh`.** `qp_refresh` → `rotate_session`;
   rotated → new JWT + scoped cookie; reuse → family revoked + cleared
   cookies + 401; empty → 401 + cleared cookies.
   *Tests:* rotation happy path, reuse → family revocation, expired
   refresh → 401.
   *Accept:* `go test -count=1 -race ./internal/auth/...`.

7. **PR C4 — `POST /auth/logout`.** `qp_refresh` → `revoke_session`
   family; always 204 + clears both cookies (spec §8 — works when
   `qp_access` is already expired; no validity oracle).
   *Tests:* logout after access-expiry still revokes the family;
   repeated logout stays 204.
   *Accept:* `go test -count=1 -race ./internal/auth/...`.

8. **PR C5 — `GET /me`.** `Authenticate` → `get_user_profile` + the §3.3
   membership list.
   *Tests:* authenticated payload shape; forged/absent JWT → 401.
   *Accept:* `go test -count=1 -race ./internal/auth/...`.

9. **PR C6 — `GET /me/organization`.** `Authenticate` + `ResolveTenant`
   + `get_current_organization` inside `InTenantTx` — the end-to-end
   proof of the whole mission.
   *Tests:* org row through the real chain; non-member vs nonexistent org
   → identical 404; forged JWT → 401; missing header → 400.
   *Accept:* `go test -count=1 -race ./internal/auth/...`, full
   `make -C apps/core-api integration-test`, `go build ./...`.

## Verification (acceptance)

```
cd apps/core-api
go test -count=1 -race ./...
go vet ./... && gofmt -l .
go build ./...
cd ../.. && make db-up && make db-check && make db-check-negative
make test-integration   # real-Postgres suite incl. the new proofs
```

The e2e claim this mission makes is precise: a test logs in through
`POST /auth/session`, reads `GET /me/organization` with
`X-Organization-Id`, and gets the seeded org back — request → JWT →
tenant → `InTenantTx` — while a second org's id earns the identical 404.

## Review gates

Local `code-reviewer` pass on each PR's full diff before opening:
ADR-005 §2 hygiene on every function (pinned `search_path`, static SQL,
minimal returns, paired grants), the allowlist triplet actually moved,
uniform-response proof by body comparison, token/hash discipline, no
org or role in JWT claims, tests prove behavior against real Postgres —
never mocks. Then PR → Devin Review → fix rounds → Pedro merge decision;
PRs land in dependency order A → B → (C0 ∥) → C.
