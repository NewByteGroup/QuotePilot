// Package authapi holds the oapi-codegen output for
// packages/contracts/openapi/core-api.yaml — the six-route auth/session slice
// of the tenant-resolution spec (docs/specs/2026-11-10-tenant-resolution-api.md
// section 8) plus path-less payload models such as the Resend
// `email.received` webhook envelope decoded by the hand-mounted inbox
// handler — generated per ADR-002 (std-http-server + strict-server,
// committed output, skip-prune so schemas with no referencing path still
// emit a type).
//
// Handlers implement StrictServerInterface and are mounted through
// NewStrictHandlerWithOptions + HandlerWithOptions — HandlerWithOptions, not
// HandlerFromMux, because the uniform {"error": ...} bodies of spec section 7
// plug in at three distinct hooks:
//   - StdHTTPServerOptions.ErrorHandlerFunc catches wrapper-level binding
//     failures (none today: X-Organization-Id is deliberately OPTIONAL in the
//     contract so ResolveTenant — not the binder — emits
//     {"error":"missing_organization"} AFTER Authenticate; a required binding
//     would 400 an unauthenticated request before the uniform 401);
//   - StrictHandlerOptions.RequestErrorHandlerFunc catches JSON-bind errors
//     inside strict handlers — 400 {"error":"bad_request"};
//   - StrictHandlerOptions.ResponseErrorHandlerFunc catches errors returned
//     by the handler itself — 500 {"error":"internal"}.
//
// StrictServerInterface methods receive only (ctx, request): cookies are
// READ by a StrictMiddlewareFunc or outer middleware that sees *http.Request
// and plants values on ctx (Authenticate for qp_access, a refresh-cookie
// reader for qp_refresh), and WRITTEN by per-operation response types whose
// Visit…Response(w) calls Set-Cookie before the body — the generated empty
// response structs cannot carry cookies. Per-operation concerns (the login
// rate limit, Authenticate on /me* but not /auth/*) key off the operationID
// argument StrictMiddlewareFunc receives — StdHTTPServerOptions.Middlewares
// are plain http.Handler wraps with no operationID in ctx, so dispatch there
// means matching r.URL.Path or wrapping at the mux level.
//
// The contract carries no cookie parameters on purpose: a required-cookie
// parameter would emit a 400 binding error where the spec mandates the
// uniform 401.
//
// authapi.gen.go is generated; regenerate with `make generate` in
// apps/core-api and never edit it by hand (CI contracts-drift fails on any
// diff). This doc.go is the only hand-written file in the package.
package authapi
