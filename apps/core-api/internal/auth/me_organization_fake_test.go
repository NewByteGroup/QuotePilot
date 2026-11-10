package auth_test

// Test doubles for the GET /me/organization proofs in me_organization_test.go:
// an in-memory platform.Querier covering the two function calls on the path
// (get_membership for ResolveTenant, get_current_organization inside the
// tenant tx), a pgx.Tx shim so the real platform.Store runs inside the fake
// tenant transaction, and the TenantRunner double that records how the
// handler entered it. Same seams as session_fake_test.go — production SQL
// text and NULL handling run against the fakes.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

// memberKey is the (user, org) pair get_membership answers for.
type memberKey struct {
	user uuid.UUID
	org  uuid.UUID
}

// orgRec is one app.organizations row as the get_current_organization scan
// sees it; a nil branding stands in for the NULL of the empty composite row.
type orgRec struct {
	name     string
	slug     string
	alias    string
	branding json.RawMessage
}

// orgQuerier is the in-memory platform.Querier for the /me/organization
// path. get_current_organization takes no argument — the function reads the
// tenant GUC — so current stands in for it: the runner double sets it to the
// org it was invoked with, mirroring applyTenantContext's set_config, and the
// org row resolves against it. An org absent from orgs produces the same
// all-NULL composite the function emits outside a tenant context.
type orgQuerier struct {
	roles           map[memberKey]string
	orgs            map[uuid.UUID]orgRec
	current         uuid.UUID
	fail            map[string]error
	membershipCalls []memberKey
	orgReads        int
}

var _ platform.Querier = (*orgQuerier)(nil)

func (q *orgQuerier) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	switch {
	case strings.Contains(sql, "get_membership"):
		return q.membershipRow(args)
	case strings.Contains(sql, "get_current_organization"):
		return q.organizationRow()
	default:
		return stubRow{scan: func(...any) error {
			return fmt.Errorf("orgQuerier: unexpected sql %q", sql)
		}}
	}
}

func (q *orgQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("orgQuerier: Query is not on the /me/organization path")
}

// membershipRow answers SELECT app.get_membership($1, $2)::text: the recorded
// (user, org) pair and the role, or NULL — the indistinguishable miss for
// non-member and nonexistent org alike.
func (q *orgQuerier) membershipRow(args []any) pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["get_membership"]; err != nil {
			return err
		}
		key := memberKey{user: args[0].(uuid.UUID), org: args[1].(uuid.UUID)}
		q.membershipCalls = append(q.membershipCalls, key)
		out := dest[0].(**string)
		role, ok := q.roles[key]
		if !ok {
			*out = nil
			return nil
		}
		*out = &role
		return nil
	}}
}

// organizationRow answers the five-column get_current_organization select:
// the row for current, or all NULLs — the NULL-composite shape the real
// function returns with no tenant context, which the Store maps to
// found=false.
func (q *orgQuerier) organizationRow() pgx.Row {
	return stubRow{scan: func(dest ...any) error {
		if err := q.fail["get_current_organization"]; err != nil {
			return err
		}
		q.orgReads++
		rec, ok := q.orgs[q.current]
		if !ok {
			*(dest[0].(**uuid.UUID)) = nil
			*(dest[1].(**string)) = nil
			*(dest[2].(**string)) = nil
			*(dest[3].(**string)) = nil
			*(dest[4].(*json.RawMessage)) = nil
			return nil
		}
		id := q.current
		*(dest[0].(**uuid.UUID)) = &id
		*(dest[1].(**string)) = &rec.name
		*(dest[2].(**string)) = &rec.slug
		*(dest[3].(**string)) = &rec.alias
		*(dest[4].(*json.RawMessage)) = rec.branding
		return nil
	}}
}

// orgTx adapts a platform.Querier to pgx.Tx so the runner double hands the
// handler a transaction the real platform.Store can query — the two query
// methods delegate, and every other member of the eleven-method interface
// fails the test like panicQuerier does (httptest drives the handler on the
// test's own goroutine, so Fatal is legal here).
type orgTx struct {
	t *testing.T
	q platform.Querier
}

var _ pgx.Tx = (*orgTx)(nil)

func (tx *orgTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return tx.q.QueryRow(ctx, sql, args...)
}

func (tx *orgTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return tx.q.Query(ctx, sql, args...)
}

func (tx *orgTx) Begin(context.Context) (pgx.Tx, error) {
	tx.t.Fatal("orgTx.Begin: nested tenant txs are forbidden")
	return nil, nil
}

func (tx *orgTx) Commit(context.Context) error {
	tx.t.Fatal("orgTx.Commit: the runner owns the transaction lifecycle")
	return nil
}

func (tx *orgTx) Rollback(context.Context) error {
	tx.t.Fatal("orgTx.Rollback: the runner owns the transaction lifecycle")
	return nil
}

func (tx *orgTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	tx.t.Fatal("orgTx.CopyFrom: not on the /me/organization path")
	return 0, nil
}

func (tx *orgTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults {
	tx.t.Fatal("orgTx.SendBatch: not on the /me/organization path")
	return nil
}

func (tx *orgTx) LargeObjects() pgx.LargeObjects {
	tx.t.Fatal("orgTx.LargeObjects: not on the /me/organization path")
	return pgx.LargeObjects{}
}

func (tx *orgTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	tx.t.Fatal("orgTx.Prepare: not on the /me/organization path")
	return nil, nil
}

func (tx *orgTx) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	tx.t.Fatal("orgTx.Exec: not on the /me/organization path")
	return pgconn.CommandTag{}, nil
}

func (tx *orgTx) Conn() *pgx.Conn {
	tx.t.Fatal("orgTx.Conn: not on the /me/organization path")
	return nil
}

// orgRunner is the TenantRunner double: it records which method ran and the
// org the handler resolved, mirrors set_config by stashing that org as the
// querier's GUC stand-in, and invokes fn against the tx shim. fail returns
// before fn — the driver-error shape. InTenantTx answers an error instead of
// failing: the handler would surface it as a 500, so a write-tx slip is
// distinguishable from a crash in assertions.
type orgRunner struct {
	q      *orgQuerier
	tx     *orgTx
	fail   error
	called bool
	readTx bool
	org    uuid.UUID
}

var _ tenantdb.TenantRunner = (*orgRunner)(nil)

func (r *orgRunner) InTenantReadTx(ctx context.Context, org uuid.UUID, fn func(context.Context, pgx.Tx) error) error {
	r.called = true
	r.readTx = true
	r.org = org
	if r.fail != nil {
		return r.fail
	}
	r.q.current = org
	return fn(ctx, r.tx)
}

func (r *orgRunner) InTenantTx(context.Context, uuid.UUID, func(context.Context, pgx.Tx) error) error {
	r.called = true
	r.readTx = false
	return errors.New("orgRunner: write tx requested — /me/organization must use InTenantReadTx")
}

// neverRunner is the TenantRunner for the other five routes' rigs: none of
// them may enter a tenant transaction, so any call fails the test (the
// panicQuerier precedent — an unexpected dependency is a bug, not a skip).
type neverRunner struct{ t *testing.T }

var _ tenantdb.TenantRunner = (*neverRunner)(nil)

func (r *neverRunner) InTenantTx(context.Context, uuid.UUID, func(context.Context, pgx.Tx) error) error {
	r.t.Fatal("unexpected InTenantTx: this route must never enter a tenant transaction")
	return nil
}

func (r *neverRunner) InTenantReadTx(context.Context, uuid.UUID, func(context.Context, pgx.Tx) error) error {
	r.t.Fatal("unexpected InTenantReadTx: this route must never enter a tenant transaction")
	return nil
}

// meOrgRig is a mounted /me/organization surface plus the test's windows:
// authed is the production chain (Authenticate → ResolveTenant → server),
// owner adds RequireRole(OWNER) at the tail, authOnly stops before
// ResolveTenant (a half-mounted chain — user ctx without org ctx) and bare is
// the generated mux alone, the mis-mount the fail-closed proofs drive.
type meOrgRig struct {
	authed   http.Handler
	owner    http.Handler
	authOnly http.Handler
	bare     http.Handler
	clock    *fakeClock
	cfg      *platform.Config
	q        *orgQuerier
	runner   *orgRunner
}

// newMeOrgRig wires the endpoints the way the per-route cmd mount will:
// real Store over the fake Querier shared by ResolveTenant and the
// tx-leg read, the runner double for the tenant tx, mounted through
// auth.NewServer behind the platform chain — a mux-level wrap, since strict
// middlewares cannot emit the platform's byte-exact bodies (authapi/doc.go).
func newMeOrgRig(t *testing.T, cfg *platform.Config) *meOrgRig {
	t.Helper()
	clock := &fakeClock{now: time.Now()}
	q := &orgQuerier{
		roles: map[memberKey]string{},
		orgs:  map[uuid.UUID]orgRec{},
		fail:  map[string]error{},
	}
	runner := &orgRunner{q: q, tx: &orgTx{t: t, q: q}}
	store := platform.NewStore(q)
	endpoints := auth.NewEndpoints(cfg, store, directInTx(store), runner, &platform.FakeMailSender{},
		platform.NewLimiter(platform.LoginIPCapacity, platform.LimitWindow, clock.Now),
		platform.NewLimiter(platform.LoginEmailCapacity, platform.LimitWindow, clock.Now),
		clock.Now)
	server := auth.NewServer(endpoints)
	return &meOrgRig{
		authed:   platform.Authenticate(cfg)(platform.ResolveTenant(store)(server)),
		owner:    platform.Authenticate(cfg)(platform.ResolveTenant(store)(platform.RequireRole(platform.RoleOwner)(server))),
		authOnly: platform.Authenticate(cfg)(server),
		bare:     server,
		clock:    clock,
		cfg:      cfg,
		q:        q,
		runner:   runner,
	}
}

// getMeOrg fires one request at the given mount; a nil cookie or empty
// orgHeader leaves that credential off the wire entirely.
func (r *meOrgRig) getMeOrg(
	t *testing.T,
	mount http.Handler,
	cookie *http.Cookie,
	orgHeader string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/me/organization", nil)
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if orgHeader != "" {
		req.Header.Set("X-Organization-Id", orgHeader)
	}
	rec := httptest.NewRecorder()
	mount.ServeHTTP(rec, req)
	return rec
}

// meOrgAccessCookie mints a real access JWT for userID — the sid is a fresh
// uuid, and nothing on this path re-validates it: Authenticate is stateless
// (spec section 4).
func (r *meOrgRig) meOrgAccessCookie(t *testing.T, userID uuid.UUID) *http.Cookie {
	t.Helper()
	raw, err := r.cfg.SignAccess(userID, uuid.New(), r.clock.now)
	require.NoError(t, err)
	return &http.Cookie{Name: "qp_access", Value: raw}
}
