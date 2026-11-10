// Package authapi holds the oapi-codegen output for
// packages/contracts/openapi/core-api.yaml — the six-route auth/session slice
// of the tenant-resolution spec (docs/specs/2026-11-10-tenant-resolution-api.md
// section 8), generated per ADR-002 (std-http-server + strict-server,
// committed output).
//
// Handlers implement StrictServerInterface and are mounted through
// NewStrictHandlerWithOptions + HandlerWithOptions — HandlerWithOptions, not
// HandlerFromMux, because the uniform {"error": ...} bodies of spec section 7
// plug in at three distinct hooks:
//   - StdHTTPServerOptions.ErrorHandlerFunc catches binding failures before
//     the strict layer — RequiredHeaderError on X-Organization-Id must map to
//     400 {"error":"missing_organization"}, not the generated plain-text
//     default;
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
// response structs cannot carry cookies. StdHTTPServerOptions.Middlewares
// applies to all six routes, so per-operation concerns (the login rate
// limit, Authenticate on /me* but not /auth/*) gate on ctx.Value's
// operationID or wrap at the mux level instead.
//
// The contract carries no cookie parameters on purpose: a required-cookie
// parameter would emit a 400 binding error where the spec mandates the
// uniform 401.
//
// authapi.gen.go is generated; regenerate with `make generate` in
// apps/core-api and never edit it by hand (CI contracts-drift fails on any
// diff). This doc.go is the only hand-written file in the package.
package authapi
