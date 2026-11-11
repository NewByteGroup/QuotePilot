package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// InTxFunc runs fn against a Store bound to one transaction: an endpoint
// whose flow must commit several writes atomically hands its whole body to
// it — fn's error abandons the transaction, a nil return commits it.
// Production passes store.InStoreTx; the fake rigs pass a direct call (the
// in-memory Querier has no Begin), which drives the same flow without a
// real BEGIN — real-Postgres tests prove the atomicity itself.
//
//	endpoints := auth.NewEndpoints(cfg, store, store.InStoreTx, mailer, ipLimiter, emailLimiter, time.Now)
type InTxFunc func(context.Context, func(*platform.Store) error) error

// Endpoints implements authapi.StrictServerInterface for the six-route
// auth/session surface (spec section 8). Dependencies are concrete types so
// tests drive the real Store and Limiter logic behind a stubbed Querier and
// FakeMailSender — the seams are the platform package's own interfaces.
type Endpoints struct {
	cfg          *platform.Config
	store        *platform.Store
	inTx         InTxFunc
	mailer       platform.MailSender
	ipLimiter    *platform.Limiter
	emailLimiter *platform.Limiter
	now          func() time.Time
}

var _ authapi.StrictServerInterface = (*Endpoints)(nil)

// NewEndpoints binds the auth surface to its dependencies. The two limiters
// are the spec section 8.1 pair — production wires them as
// platform.NewLimiter(platform.LoginIPCapacity, platform.LimitWindow, time.Now)
// and the LoginEmailCapacity twin; tests inject small capacities on a fake
// clock. inTx is the atomic-write seam (production: store.InStoreTx — the
// magic-link exchange is one transaction, not a sequence of commits). now is
// shared with the limiters so token expiry and bucket refill read one time
// source.
//
//	endpoints := auth.NewEndpoints(cfg, store, store.InStoreTx, mailer, ipLimiter, emailLimiter, time.Now)
func NewEndpoints(
	cfg *platform.Config,
	store *platform.Store,
	inTx InTxFunc,
	mailer platform.MailSender,
	ipLimiter, emailLimiter *platform.Limiter,
	now func() time.Time,
) *Endpoints {
	return &Endpoints{
		cfg:          cfg,
		store:        store,
		inTx:         inTx,
		mailer:       mailer,
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
// failure — into the same 500: a server fault is not an enumeration signal,
// so no detail of it crosses the wire. (Mail failures no longer reach this
// hook: the send is detached and only ever logged — see sendMagicLink.)
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

// errUnimplemented answers a call to an endpoint whose PR has not landed —
// the route is mounted (the contract mounts all six), so a stray call must
// fail loudly through the uniform 500 rather than silently 200 or 204.
var errUnimplemented = errors.New("auth: endpoint not implemented yet — lands in PRs C5–C6")

func (e *Endpoints) GetMe(context.Context, authapi.GetMeRequestObject) (authapi.GetMeResponseObject, error) {
	return nil, errUnimplemented
}

func (e *Endpoints) GetMeOrganization(context.Context, authapi.GetMeOrganizationRequestObject) (authapi.GetMeOrganizationResponseObject, error) {
	return nil, errUnimplemented
}
