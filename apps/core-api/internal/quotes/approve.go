package quotes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/jobs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

// Quote status strings as the app.quote_status enum stores them (migration
// 0002). Approval keeps DRAFT — the send worker owns the flip to
// APPROVED_SENT; SUPERSEDED arrives only through recalculation (rule 9).
const (
	statusDraft        = "DRAFT"
	statusSuperseded   = "SUPERSEDED"
	statusApprovedSent = "APPROVED_SENT"
)

// approvedViaAPI is the provenance stamped on every endpoint approval.
const approvedViaAPI = "api"

// Endpoints implements the quote operations the strict surface delegates.
// runner is the only DB path (ADR-002); inserter stages the S5 job inside
// the same tenant transaction that stamps the approval — stamp, audit row
// and job commit or roll back together (spec section 3, ADR-004).
type Endpoints struct {
	runner   tenantdb.TenantRunner
	inserter jobs.JobInserter
}

// NewEndpoints binds the delegate. Production passes the tenant runner
// and the River client:
//
//	endpoints.Approver = quotes.NewEndpoints(db, riverClient)
func NewEndpoints(runner tenantdb.TenantRunner, inserter jobs.JobInserter) *Endpoints {
	return &Endpoints{runner: runner, inserter: inserter}
}

// ApproveQuote implements POST /quotes/{id}/approve. The middleware chain
// (Authenticate + ResolveTenant + RequireRole) has already proven
// identity, membership and role; a missing ctx pair means a mis-mounted
// route and answers the uniform 401 like me.go's rule.
func (e *Endpoints) ApproveQuote(
	ctx context.Context,
	req authapi.ApproveQuoteRequestObject,
) (authapi.ApproveQuoteResponseObject, error) {
	orgID, ok := platform.OrgID(ctx)
	userID, ok2 := platform.UserID(ctx)
	if !ok || !ok2 {
		return approveUnauthenticated(), nil
	}
	out, err := e.approve(ctx, orgID, userID, requestCorrelation(ctx), uuid.UUID(req.ID))
	if err != nil {
		return nil, err
	}
	return out.response(uuid.UUID(req.ID)), nil
}

// approveKind is what the tenant transaction decided; response() maps it
// onto the contract's status codes.
type approveKind int

const (
	kindApproved approveKind = iota
	kindAlreadyApproved
	kindNotFound
	kindSuperseded
	kindNegativeMargin
)

// approveOutcome carries the decision plus the stored stamp the 200 body
// needs — for a no-op those are the first approval's values, so a
// re-approve answers byte-identically.
type approveOutcome struct {
	kind       approveKind
	status     string
	approvedAt time.Time
	approvedBy uuid.UUID
}

// quoteRow is the slice of app.quotes the decision reads — locked FOR
// UPDATE so a concurrent approve/expire cannot interleave.
type quoteRow struct {
	id          uuid.UUID
	status      string
	quoteNumber string
	version     int
	approvedAt  *time.Time
	approvedBy  *uuid.UUID
	pricingJSON []byte
}

// approve runs the whole Approval contract inside one InTenantTx: lock →
// refuse → margin gate → stamp → audit → enqueue.
func (e *Endpoints) approve(
	ctx context.Context,
	orgID, userID, correlationID, quoteID uuid.UUID,
) (approveOutcome, error) {
	var out approveOutcome
	err := e.runner.InTenantTx(ctx, orgID, func(ctx context.Context, tx pgx.Tx) error {
		q, found, err := lockQuote(ctx, tx, quoteID)
		if err != nil {
			return fmt.Errorf("quotes: approve: lock %s: %w", quoteID, err)
		}
		if !found {
			out = approveOutcome{kind: kindNotFound}
			return nil
		}
		// Refusal order per spec Approval: a retired version is refused
		// before the approval check — a superseded version that was once
		// approved must still 409, not no-op.
		switch {
		case q.status == statusSuperseded:
			out = approveOutcome{kind: kindSuperseded}
			return nil
		case q.approvedAt != nil || q.status == statusApprovedSent:
			// The no-op: already stamped, or already sent (which implies
			// stamped — a sent quote without approval fields is data
			// drift, surfaced as a fault rather than a false success).
			if q.approvedAt == nil || q.approvedBy == nil {
				return fmt.Errorf("quotes: approve: quote %s is %s without an approval stamp", quoteID, q.status)
			}
			out = approveOutcome{
				kind:       kindAlreadyApproved,
				status:     q.status,
				approvedAt: *q.approvedAt,
				approvedBy: *q.approvedBy,
			}
			return nil
		}
		negative, err := negativeMargin(q.pricingJSON)
		if err != nil {
			return fmt.Errorf("quotes: approve: pricing_result_json on %s: %w", quoteID, err)
		}
		if negative {
			if err := audit(ctx, tx, orgID, &userID, "quote.approve_refused", q.id,
				map[string]any{"reason": "negative_margin"}); err != nil {
				return err
			}
			out = approveOutcome{kind: kindNegativeMargin}
			return nil
		}
		return e.stamp(ctx, tx, orgID, userID, correlationID, q, &out)
	})
	return out, err
}

// lockQuote SELECTs the version FOR UPDATE; RLS scopes the lookup so an
// unknown or other-tenant id is the same found=false.
func lockQuote(ctx context.Context, tx pgx.Tx, quoteID uuid.UUID) (quoteRow, bool, error) {
	var q quoteRow
	err := tx.QueryRow(ctx, `
		SELECT id, status::text, quote_number, version,
		       approved_at, approved_by_user_id, pricing_result_json
		FROM app.quotes WHERE id = $1 FOR UPDATE`, quoteID).
		Scan(&q.id, &q.status, &q.quoteNumber, &q.version,
			&q.approvedAt, &q.approvedBy, &q.pricingJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return quoteRow{}, false, nil
	}
	return q, err == nil, err
}

// negativeMargin decodes the pinned engine output and applies the hard
// block (spec: priced below cost is never sent — checked at approval AND
// at send). A NULL or corrupt blob is contract drift, an error — a priced
// quote always carries its output envelope.
func negativeMargin(raw []byte) (bool, error) {
	if len(raw) == 0 {
		return false, errors.New("pricing_result_json is NULL")
	}
	var out pricing.Output
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, fmt.Errorf("decode: %w", err)
	}
	return out.NegativeMargin(), nil
}

// stamp is the commit sequence: approval fields, the quote.approved audit
// row, then the S5 job — unique on (kind, quote_id) via SendArgs.
func (e *Endpoints) stamp(
	ctx context.Context,
	tx pgx.Tx,
	orgID, userID, correlationID uuid.UUID,
	q quoteRow,
	out *approveOutcome,
) error {
	var approvedAt time.Time
	err := tx.QueryRow(ctx, `
		UPDATE app.quotes
		SET approved_at = now(), approved_by_user_id = $2,
		    approved_via = $3, updated_at = now()
		WHERE id = $1 RETURNING approved_at`,
		q.id, userID, approvedViaAPI).Scan(&approvedAt)
	if err != nil {
		return fmt.Errorf("quotes: approve: stamp %s: %w", q.id, err)
	}
	if err := audit(ctx, tx, orgID, &userID, "quote.approved", q.id,
		map[string]any{"quote_number": q.quoteNumber, "version": q.version}); err != nil {
		return err
	}
	if e.inserter == nil {
		return errors.New("quotes: approve: job inserter not configured")
	}
	if _, err := e.inserter.InsertTx(ctx, tx, jobs.SendArgs{
		OrganizationID: orgID,
		CorrelationID:  correlationID,
		QuoteID:        q.id,
	}, nil); err != nil {
		return fmt.Errorf("quotes: approve: enqueue quote_send %s: %w", q.id, err)
	}
	*out = approveOutcome{
		kind:       kindApproved,
		status:     q.status,
		approvedAt: approvedAt,
		approvedBy: userID,
	}
	return nil
}

// audit writes one tenant_audit_logs row — approval and refusal alike,
// because refusal auditability is the point of the margin gate.
func audit(ctx context.Context, tx pgx.Tx, orgID uuid.UUID, actor *uuid.UUID, action string, quoteID uuid.UUID, meta map[string]any) error {
	if _, err := platform.NewStore(tx).InsertAuditLog(ctx, platform.AuditEntry{
		OrganizationID: orgID,
		ActorUserID:    actor,
		Action:         action,
		EntityType:     "quote",
		EntityID:       quoteID,
		Metadata:       meta,
	}); err != nil {
		return fmt.Errorf("quotes: approve: audit %q on %s: %w", action, quoteID, err)
	}
	return nil
}

// requestCorrelation carries the request's X-Request-Id into the job when
// it is a uuid — tracing one line end to end — and mints a fresh one
// otherwise (the ingest chain roots a fresh id the same way).
func requestCorrelation(ctx context.Context) uuid.UUID {
	if id, err := uuid.Parse(platform.CorrelationID(ctx)); err == nil {
		return id
	}
	return uuid.New()
}

// response maps the decision onto the contract's status codes; each
// refusal carries the machine-readable code the 409 body needs.
func (o approveOutcome) response(quoteID uuid.UUID) authapi.ApproveQuoteResponseObject {
	switch o.kind {
	case kindApproved, kindAlreadyApproved:
		return authapi.ApproveQuote200JSONResponse{
			ID:               quoteID.String(),
			Status:           authapi.QuoteStatus(o.status),
			ApprovedAt:       o.approvedAt,
			ApprovedByUserID: o.approvedBy.String(),
		}
	case kindNotFound:
		return authapi.ApproveQuote404JSONResponse{
			NotFoundJSONResponse: authapi.NotFoundJSONResponse{Error: authapi.ErrorCodeNotFound},
		}
	case kindSuperseded:
		return authapi.ApproveQuote409JSONResponse{
			ConflictJSONResponse: authapi.ConflictJSONResponse{Error: authapi.ErrorCodeQuoteSuperseded},
		}
	case kindNegativeMargin:
		return authapi.ApproveQuote409JSONResponse{
			ConflictJSONResponse: authapi.ConflictJSONResponse{Error: authapi.ErrorCodeNegativeMargin},
		}
	}
	return authapi.ApproveQuote500JSONResponse{
		InternalErrorJSONResponse: authapi.InternalErrorJSONResponse{Error: authapi.ErrorCodeInternal},
	}
}

// approveUnauthenticated is the fail-closed answer for a mis-mounted
// route — the same body the middleware emits for a bad token.
func approveUnauthenticated() authapi.ApproveQuoteResponseObject {
	return authapi.ApproveQuote401JSONResponse{
		UnauthorizedJSONResponse: authapi.UnauthorizedJSONResponse{Error: authapi.ErrorCodeUnauthenticated},
	}
}
