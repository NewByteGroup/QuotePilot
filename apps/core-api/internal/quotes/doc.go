// Package quotes holds the tenant-facing quote operations of the staged
// pipeline — today POST /quotes/{id}/approve, the analyst's central act
// (spec 2026-11-11-staged-quote-pipeline section 4 "Approval"). The
// endpoint plugs into the auth strict surface as its QuoteApprover
// delegate: oapi-codegen emits one StrictServerInterface for the whole
// contract, so the method is generated in authapi while the handler lives
// here with its tenant-transaction dependencies.
package quotes
