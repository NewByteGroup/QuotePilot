// Package auth serves the auth/session HTTP surface of spec
// 2026-11-10-tenant-resolution-api section 8: the magic-link lifecycle
// (/auth/login, /auth/session, /auth/refresh, /auth/logout) plus the caller
// identity reads (/me, /me/organization). Endpoints implements the
// authapi.StrictServerInterface contract generated from
// packages/contracts/openapi/core-api.yaml; NewServer mounts it with the
// strict hooks that turn binding and driver failures into the spec section 7
// uniform {"error": ...} bodies. PR C1 ships POST /auth/login — the remaining
// operations fail loudly until their own PRs land.
package auth
