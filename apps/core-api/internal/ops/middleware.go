package ops

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// AdminGate is the flag lookup RequirePlatformAdmin runs per request —
// *platform.Store satisfies it (store.IsPlatformAdmin →
// app.is_platform_admin, migration 0018); the fake rig substitutes the
// decision. One function call per request, the same never-cached posture
// ResolveTenant's membership probe keeps (ADR-005 section 5): the flag is
// deliberately not a JWT claim, so losing it closes the surface on the
// next call instead of at token expiry.
type AdminGate interface {
	IsPlatformAdmin(ctx context.Context, userID uuid.UUID) (bool, error)
}

// RequirePlatformAdmin admits the request only when Authenticate's sub
// resolves to a users row flagged is_platform_admin — the platform path's
// whole authorization model (ADR-005). No user context means the chain was
// mis-mounted without Authenticate and denies as unauthenticated, the same
// fail-closed shape RequireRole keeps. A gate error is a server fault,
// not an authorization answer — 500, like checkMembership's driver-error
// branch. Everything else — non-admin, unknown user — is the uniform
// 403 {"error":"forbidden"}: the flag read cannot distinguish them by
// contract, so neither can the wire.
//
//	mux.Handle("GET /ops/quarantine",
//		platform.Correlation(platform.Authenticate(cfg)(ops.RequirePlatformAdmin(store)(authSrv))))
func RequirePlatformAdmin(gate AdminGate) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID, ok := platform.UserID(r.Context())
			if !ok {
				writeError(r.Context(), w, http.StatusUnauthorized, authapi.ErrorCodeUnauthenticated)
				return
			}
			admin, err := gate.IsPlatformAdmin(r.Context(), userID)
			if err != nil {
				slog.Default().WarnContext(r.Context(), "ops: platform-admin check failed",
					"correlation_id", platform.CorrelationID(r.Context()), "error", err)
				writeError(r.Context(), w, http.StatusInternalServerError, authapi.ErrorCodeInternal)
				return
			}
			if !admin {
				writeError(r.Context(), w, http.StatusForbidden, authapi.ErrorCodeForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeError is platform.writeError's twin for the ops gate (it is
// unexported there): one fixed {"error":"<code>","message":"<text>"} body,
// one content type, and only the status and code as inputs — the same
// uniform-failure contract the middleware file documents. Marshal, not a
// string splice, keeps a message change from corrupting the body.
func writeError(ctx context.Context, w http.ResponseWriter, status int, code authapi.ErrorCode) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, err := json.Marshal(authapi.NewError(code))
	if err != nil {
		return
	}
	if _, err := w.Write(body); err != nil {
		slog.Default().WarnContext(ctx, "ops: error response write failed",
			"correlation_id", platform.CorrelationID(ctx),
			"status", status, "code", code, "error", err)
	}
}
