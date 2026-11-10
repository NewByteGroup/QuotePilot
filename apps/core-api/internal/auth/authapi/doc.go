// Package authapi holds the oapi-codegen output for
// packages/contracts/openapi/core-api.yaml — the six-route auth/session slice
// of the tenant-resolution spec (docs/specs/2026-11-10-tenant-resolution-api.md
// section 8), generated per ADR-002 (std-http-server + strict-server,
// committed output).
//
// Handlers implement StrictServerInterface and are mounted through
// NewStrictHandlerWithOptions + HandlerFromMux; the strictHandler's
// RequestErrorHandlerFunc/ResponseErrorHandlerFunc options are where the
// uniform {"error": ...} bodies of spec section 7 plug in (the generated
// default writes http.Error text, not the contract shape). Cookies are read
// and written by the handlers and the internal/platform middleware — the
// contract carries no cookie parameters on purpose, since a required-cookie
// parameter would 400 where the spec wants the uniform 401.
//
// authapi.gen.go is generated; regenerate with `make generate` in
// apps/core-api and never edit it by hand (CI contracts-drift fails on any
// diff). This doc.go is the only hand-written file in the package.
package authapi
