package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

// InTxFunc runs fn against a Store bound to one transaction — plus the raw
// pgx.Tx that transaction runs on, because the login flow's staged River
// insert lands on the driver tx, not on the Store's view of it. An endpoint
// whose flow must commit several writes atomically hands its whole body to
// it: fn's error abandons the transaction, a nil return commits it.
// Production passes store.InStorePgxTx; the fake rigs pass a direct call
// (the in-memory Querier has no Begin, so their raw tx is nil), which
// drives the same flow without a real BEGIN — real-Postgres tests prove
// the atomicity itself.
//
//	endpoints := auth.NewEndpoints(cfg, store, store.InStorePgxTx, runner, enqueue, ipLimiter, emailLimiter, time.Now)
type InTxFunc func(context.Context, func(*platform.Store, pgx.Tx) error) error

// EnqueueMailFunc stages the magic-link send inside the login transaction:
// the auth_mail_outbox row is written by the same tx, and the job carries
// the outbox id only — never the link, which river_job's qp_app-readable
// args would expose as a second live credential (migration 0004).
// Production wires the River client's InsertTx adapted to AuthMailArgs; the
// fake rigs capture the id and return an injected error.
//
//	err := enqueue(ctx, tx, outboxID)
type EnqueueMailFunc func(ctx context.Context, tx pgx.Tx, outboxID uuid.UUID) error

// Endpoints implements authapi.StrictServerInterface for the six-route
// auth/session surface (spec section 8). Dependencies are concrete types so
// tests drive the real Store and Limiter logic behind a stubbed Querier and
// a fake enqueue — the seams are the platform package's own interfaces, and
// the runner seam is the one tenantdb exports (inbox and jobs already hold
// the same interface).
type Endpoints struct {
	cfg          *platform.Config
	store        *platform.Store
	inTx         InTxFunc
	runner       tenantdb.TenantRunner
	enqueue      EnqueueMailFunc
	ipLimiter    *platform.Limiter
	emailLimiter *platform.Limiter
	now          func() time.Time
}

var _ authapi.StrictServerInterface = (*Endpoints)(nil)

// NewEndpoints binds the auth surface to its dependencies. runner is the
// tenant transaction GET /me/organization reads through — the only route on
// this surface that enters a tenant tx. The two limiters are the spec
// section 8.1 pair — production wires them as
// platform.NewLimiter(platform.LoginIPCapacity, platform.LimitWindow, time.Now)
// and the LoginEmailCapacity twin; tests inject small capacities on a fake
// clock. inTx is the atomic-write seam (production: store.InStorePgxTx —
// the magic-link exchange AND the login staging are one transaction, not a
// sequence of commits), and enqueue is the durable-send seam inside it
// (production: the River client's InsertTx — the worker owns the send, so
// no MailSender is wired here at all). now is shared with the limiters so
// token expiry and bucket refill read one time source.
//
//	endpoints := auth.NewEndpoints(cfg, store, store.InStorePgxTx, db, enqueue, ipLimiter, emailLimiter, time.Now)
func NewEndpoints(
	cfg *platform.Config,
	store *platform.Store,
	inTx InTxFunc,
	runner tenantdb.TenantRunner,
	enqueue EnqueueMailFunc,
	ipLimiter, emailLimiter *platform.Limiter,
	now func() time.Time,
) *Endpoints {
	return &Endpoints{
		cfg:          cfg,
		store:        store,
		inTx:         inTx,
		runner:       runner,
		enqueue:      enqueue,
		ipLimiter:    ipLimiter,
		emailLimiter: emailLimiter,
		now:          now,
	}
}

// NewServer mounts the endpoints exactly as production will (authapi/doc.go):
// the strict handler carries the ctx-planting pair (ExtractClientIP for the
// login limiter, ExtractRefreshCookie for the refresh/logout cookie) plus
// the two strict hooks, and the std wrapper carries the binding-failure
// hook, so every rejection is one of the spec section 7 {"error": ...}
// bodies — never http.Error text. The returned handler routes all six
// contract paths itself and caps JSON bodies before the generated binder
// can read them; cmd/server mounts it alongside the inbox surface.
//
//	http.ListenAndServe(addr, auth.NewServer(endpoints))
func NewServer(endpoints *Endpoints) http.Handler {
	strict := authapi.NewStrictHandlerWithOptions(
		endpoints,
		[]authapi.StrictMiddlewareFunc{ExtractClientIP, ExtractRefreshCookie},
		authapi.StrictHTTPServerOptions{
			RequestErrorHandlerFunc:  writeBadRequest,
			ResponseErrorHandlerFunc: writeInternal,
		})
	return limitBody(authapi.HandlerWithOptions(strict, authapi.StdHTTPServerOptions{
		ErrorHandlerFunc: writeBadRequest,
	}))
}

// maxJSONBody caps every auth request body — the six routes each carry one
// small JSON object, so anything larger is hostile, not bigger.
const maxJSONBody = 8 << 10

// limitBody caps request bodies before the generated binder reads them —
// an oversized body dies as a uniform 400, spending no limiter budget and
// bounded parse effort. The declared length is checked up front so a short
// object riding before megabytes of trailing data cannot slip past:
// MaxBytesReader bounds only what is read, and the binder decodes a single
// value without ever reading the tail.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > maxJSONBody {
			writeError(w, http.StatusBadRequest, authapi.ErrorCodeBadRequest)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
		next.ServeHTTP(w, r)
	})
}

// writeBadRequest turns a JSON-bind failure into the uniform 400 (spec
// section 7): malformed bodies carry no information worth distinguishing.
func writeBadRequest(w http.ResponseWriter, _ *http.Request, _ error) {
	writeError(w, http.StatusBadRequest, authapi.ErrorCodeBadRequest)
}

// writeInternal collapses every handler-side failure — driver error, mint
// failure, a rolled-back staging transaction — into the same 500: a server
// fault is not an enumeration signal, so no detail of it crosses the wire.
// (Provider mail failures never reach this hook: sends run in the
// auth_mail_send worker off the response path entirely — migration 0004.)
func writeInternal(w http.ResponseWriter, _ *http.Request, _ error) {
	writeError(w, http.StatusInternalServerError, authapi.ErrorCodeInternal)
}

// writeError emits the contract's Error shape with the same content type the
// generated Visit*Response writers set (platform.writeError's twin — it is
// unexported there). The JSON encoder adds the trailing newline every
// generated error body already carries.
func writeError(w http.ResponseWriter, status int, code authapi.ErrorCode) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(authapi.Error{Error: code})
}
