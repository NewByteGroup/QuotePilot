package webapi

// GET /v1/session and POST /v1/session/organization — the bootstrap pair.
// Both answer the same Session body; the POST additionally writes the
// qp_org cookie ResolveTenant falls back to on org-scoped routes.
import (
	"context"
	"fmt"
	"net/http"
	"net/mail"
	"slices"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// orgHints carries the active-organization transports GET /v1/session
// resolves before any ResolveTenant could run: the explicit header and
// the qp_org cookie. OrgHints (the plain net/http middleware mounted on
// the route) plants it on the request context.
type orgHints struct {
	header string
	cookie string
}

// ctxKey is the package-private context key type — a distinct struct
// type, same shape platform uses, so no foreign package can plant or
// forge under these keys.
type ctxKey struct{ name string }

var hintsKey = ctxKey{"org-hints"}

// OrgHints plants the request's org hints for the strict handler. It is a
// plain middleware (not a StrictMiddlewareFunc) so it composes in the
// outer mount chain beside Authenticate — the strict surface only ever
// sees the context.
//
//	mux.Handle("GET /v1/session", platform.Authenticate(cfg)(webapi.OrgHints(authSrv)))
func OrgHints(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := orgHints{header: r.Header.Get(platform.OrgHeader)}
		if c, err := r.Cookie(platform.OrgCookie); err == nil {
			h.cookie = c.Value
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), hintsKey, h)))
	})
}

// hintsFromContext reads what OrgHints planted; a zero value means the
// middleware did not run and both transports are absent.
func hintsFromContext(ctx context.Context) orgHints {
	h, _ := ctx.Value(hintsKey).(orgHints)
	return h
}

// sessionFail is the failure vocabulary the two session handlers share —
// the strict generator emits one response type per operation, so the
// shared builder reports the class and each caller translates it into its
// own response shape.
type sessionFail int

const (
	failNone sessionFail = iota
	// failBadHint — a malformed X-Organization-Id reached the session
	// route: ResolveTenant's byte-identical 400 contract.
	failBadHint
	// failOrg — an explicit org hint outside the caller's memberships:
	// the same get_membership-NULL collapse ResolveTenant answers 404.
	failOrg
	// failNoOrgs — the user carries zero memberships; Session's
	// organizations list is minItems 1, so there is no honest 200 body.
	failNoOrgs
)

// GetSession implements GET /v1/session: profile + every membership, with
// the active organization resolved header → qp_org cookie → first
// membership (list_memberships_for_email is created_at-ordered since
// migration 0009, so "first" is deterministic). The route mounts
// Authenticate only — an absent org hint is a fallback, not a 400.
func (e *Endpoints) GetSession(ctx context.Context, _ authapi.GetSessionRequestObject) (authapi.GetSessionResponseObject, error) {
	userID, ok := platform.UserID(ctx)
	if !ok {
		return authapi.GetSession401JSONResponse{
			UnauthorizedJSONResponse: authapi.UnauthorizedJSONResponse(authapi.NewError(authapi.ErrorCodeUnauthenticated)),
		}, nil
	}
	h := hintsFromContext(ctx)
	s, fail, err := e.sessionFor(ctx, userID, h.header, h.cookie)
	if err != nil {
		return nil, err
	}
	if resp := sessionFailResponse(fail); resp != nil {
		return resp, nil
	}
	return getOK{cfg: e.cfg, org: uuid.UUID(s.Organization.ID), body: s}, nil
}

// getOK is the 200 that also writes qp_org for the resolved active org:
// a fresh session has no cookie yet, and ResolveTenantDefault's
// first-membership fallback must converge to the same org the client
// keeps — stamping it here makes the bootstrap self-healing instead of
// leaving the fallback to pick on every read.
type getOK struct {
	cfg  *platform.Config
	org  uuid.UUID
	body authapi.Session
}

// VisitGetSessionResponse sets qp_org then delegates to the generated
// 200 writer — the cookie lands before WriteHeader, exactly like the
// switch response below.
func (r getOK) VisitGetSessionResponse(w http.ResponseWriter) error {
	platform.SetOrgCookie(r.cfg, w, r.org)
	return authapi.GetSession200JSONResponse(r.body).VisitGetSessionResponse(w)
}

// sessionFailResponse maps the builder's failure class onto GetSession's
// response shapes.
func sessionFailResponse(f sessionFail) authapi.GetSessionResponseObject {
	switch f {
	case failBadHint:
		return authapi.GetSession400JSONResponse(authapi.NewError(authapi.ErrorCodeMissingOrganization))
	case failOrg, failNoOrgs:
		return authapi.GetSession404JSONResponse{
			NotFoundJSONResponse: authapi.NotFoundJSONResponse(authapi.NewError(authapi.ErrorCodeNotFound)),
		}
	}
	return nil
}

// SwitchOrganization implements POST /v1/session/organization: the target
// rides the builder as a header hint, so the membership list is the same
// live boundary get_membership enforces on org-scoped routes — a
// non-member and a nonexistent org collapse into the same 404. The 200
// carries the Session for the newly active org and sets qp_org.
func (e *Endpoints) SwitchOrganization(ctx context.Context, req authapi.SwitchOrganizationRequestObject) (authapi.SwitchOrganizationResponseObject, error) {
	userID, ok := platform.UserID(ctx)
	if !ok {
		return authapi.SwitchOrganization401JSONResponse{
			UnauthorizedJSONResponse: authapi.UnauthorizedJSONResponse(authapi.NewError(authapi.ErrorCodeUnauthenticated)),
		}, nil
	}
	if req.Body == nil {
		return authapi.SwitchOrganization400JSONResponse{
			BadRequestJSONResponse: authapi.BadRequestJSONResponse(authapi.NewError(authapi.ErrorCodeBadRequest)),
		}, nil
	}
	target := uuid.UUID(req.Body.OrganizationID)
	s, fail, err := e.sessionFor(ctx, userID, target.String(), "")
	if err != nil {
		return nil, err
	}
	switch fail {
	case failOrg, failNoOrgs:
		return authapi.SwitchOrganization404JSONResponse{
			NotFoundJSONResponse: authapi.NotFoundJSONResponse(authapi.NewError(authapi.ErrorCodeNotFound)),
		}, nil
	}
	return switchOK{cfg: e.cfg, org: target, body: s}, nil
}

// switchOK is the 200 that also writes qp_org: the cookie lands before
// the generated writer's WriteHeader, exactly like the auth cookie pair
// on the login path.
type switchOK struct {
	cfg  *platform.Config
	org  uuid.UUID
	body authapi.Session
}

// VisitSwitchOrganizationResponse sets qp_org then delegates to the
// generated 200 writer.
func (r switchOK) VisitSwitchOrganizationResponse(w http.ResponseWriter) error {
	platform.SetOrgCookie(r.cfg, w, r.org)
	return authapi.SwitchOrganization200JSONResponse(r.body).VisitSwitchOrganizationResponse(w)
}

// sessionFor assembles the Session body: profile → ordered memberships →
// resolved active index → per-org registry rows. header/cookie are the
// already-extracted org hints ("" = absent).
func (e *Endpoints) sessionFor(ctx context.Context, userID uuid.UUID, header, cookie string) (authapi.Session, sessionFail, error) {
	_, email, fullName, found, err := e.store.GetUserProfile(ctx, userID)
	if err != nil {
		return authapi.Session{}, failNone, fmt.Errorf("webapi: session: profile %s: %w", userID, err)
	}
	if !found {
		return authapi.Session{}, failNone, fmt.Errorf("webapi: session: user %s has no profile row", userID)
	}
	memberships, err := e.store.ListMembershipsForEmail(ctx, email)
	if err != nil {
		return authapi.Session{}, failNone, fmt.Errorf("webapi: session: memberships %q: %w", email, err)
	}
	if len(memberships) == 0 {
		return authapi.Session{}, failNoOrgs, nil
	}
	active, fail, err := e.activeOrg(ctx, memberships, header, cookie)
	if err != nil || fail != failNone {
		return authapi.Session{}, fail, err
	}
	orgs, err := e.sessionOrgs(ctx, memberships)
	if err != nil {
		return authapi.Session{}, failNone, err
	}
	return authapi.Session{
		User:          sessionUser(userID, email, fullName, memberships, active),
		Organization:  orgs[active],
		Organizations: orgs,
	}, failNone, nil
}

// activeOrg resolves which membership index is active: the header hint
// wins (an explicit transport — malformed is 400, outside the list is
// 404); a cookie hint resolves silently against the same list, and a
// malformed or stale cookie (a removed member, a dead org) falls through
// to index 0 — the bootstrap that lists the user's orgs must not fail on
// a hint the client cannot repair. The membership list is the same live
// data get_membership reads, so "not in the list" is the identical
// indistinguishable miss.
func (e *Endpoints) activeOrg(_ context.Context, ms []platform.Membership, header, cookie string) (int, sessionFail, error) {
	if header != "" {
		id, err := uuid.Parse(header)
		if err != nil {
			return -1, failBadHint, nil
		}
		idx := slices.IndexFunc(ms, func(m platform.Membership) bool { return m.OrgID == id })
		if idx < 0 {
			return -1, failOrg, nil
		}
		return idx, failNone, nil
	}
	if cookie != "" {
		if id, err := uuid.Parse(cookie); err == nil {
			if idx := slices.IndexFunc(ms, func(m platform.Membership) bool { return m.OrgID == id }); idx >= 0 {
				return idx, failNone, nil
			}
		}
	}
	return 0, failNone, nil
}

// sessionOrgs resolves every membership to its SessionOrganization. The
// inbound alias lives behind RLS on the Layer A registry row, so each org
// costs one InTenantReadTx + get_current_organization() — the same read
// path /me/organization uses; membership count is small and the read-only
// tx is cheap.
func (e *Endpoints) sessionOrgs(ctx context.Context, ms []platform.Membership) ([]authapi.SessionOrganization, error) {
	orgs := make([]authapi.SessionOrganization, 0, len(ms))
	for _, m := range ms {
		var org authapi.SessionOrganization
		err := e.runner.InTenantReadTx(ctx, m.OrgID, func(ctx context.Context, tx pgx.Tx) error {
			row, found, err := platform.NewStore(tx).GetCurrentOrganization(ctx)
			if err != nil {
				return fmt.Errorf("webapi: session: org row %s: %w", m.OrgID, err)
			}
			if !found {
				// membership proved the org — an absent registry row inside
				// its own tenant context is drift, never a quiet zero org.
				return fmt.Errorf("webapi: session: org %s absent inside tenant tx", m.OrgID)
			}
			org = authapi.SessionOrganization{
				ID:             openapi_types.UUID(m.OrgID),
				Name:           row.Name,
				PlanName:       planNameV1,
				InboundAddress: openapi_types.Email(row.InboundEmailAlias),
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		orgs = append(orgs, org)
	}
	return orgs, nil
}

// planNameV1 is the single tier every org reports: no plan concept exists
// in the schema yet, and the switcher renders the field — the honest
// constant stands until billing lands.
const planNameV1 = "standard"

// sessionUser builds the contract's user block: the display name falls
// back to the mailbox local part when full_name is NULL (name is minLength
// 1 — the email itself is the last resort so the field is never empty);
// role is the active org's membership rolled onto the web's two-value
// enum; language is the product default until a preferences endpoint owns
// it (the pilot's forwarders are Brazilian — pt).
func sessionUser(userID uuid.UUID, email, fullName string, ms []platform.Membership, active int) authapi.SessionUser {
	em := openapi_types.Email(email)
	return authapi.SessionUser{
		ID:                openapi_types.UUID(userID),
		Name:              displayName(fullName, email),
		Email:             &em,
		Role:              sessionRole(ms[active].Role),
		OrganizationCount: len(ms),
		Language:          authapi.Pt,
	}
}

// displayName prefers the registry name; the local part is the fallback a
// nameless user still recognizes, and the raw email is the floor so the
// minLength-1 field can never emit "".
func displayName(fullName, email string) string {
	if name := strings.TrimSpace(fullName); name != "" {
		return name
	}
	if addr, err := mail.ParseAddress(email); err == nil && addr.Address != "" {
		if local, _, ok := strings.Cut(addr.Address, "@"); ok && local != "" {
			return local
		}
		return addr.Address
	}
	return email
}

// sessionRole rolls the membership enum onto the web's two-value role:
// OWNER/ADMIN administer, everything else operates.
func sessionRole(role string) authapi.SessionUserRole {
	if role == platform.RoleOwner || role == platform.RoleAdmin {
		return authapi.Admin
	}
	return authapi.Operator
}
