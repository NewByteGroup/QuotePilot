// Package rfqs implements the RFQ-scoped action endpoints the strict
// surface delegates — today POST /v1/rfqs/{id}/reprocess, the review
// queue's release action (spec 2026-11-11 section 4 "Reprocess"). It is
// the rfq-side twin of internal/quotes: synchronous work inside one
// InTenantTx plus the resumed stage's job staged by InsertTx in the same
// transaction (ADR-004).
package rfqs

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/shopspring/decimal"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/jobs"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

// app.rfq_status values the endpoint reads/writes. needsReview and failed
// are the only reprocessable states (spec Reprocess: IGNORED is
// intentionally terminal, everything else 409s).
const (
	statusNeedsReview = "NEEDS_REVIEW"
	statusFailed      = "FAILED"
)

// actionReprocessed is the tenant_audit_logs action every accepted
// reprocess writes — also the row the min-interval gate reads back, so
// no new column exists for the gate (spec Reprocess).
const actionReprocessed = "rfq.reprocessed"

// Endpoints implements the reprocess operation the strict surface
// delegates. runner is the only DB path (ADR-002); inserter stages the
// resumed stage's job inside the same tenant transaction that clears the
// reasons and flips the status — status, audit row and job commit or roll
// back together (spec Reprocess). minInterval is the per-RFQ reprocess
// floor (REPROCESS_MIN_INTERVAL, default 60s) and now is the one clock
// the gate and the S3a hash recompute read.
type Endpoints struct {
	runner      tenantdb.TenantRunner
	inserter    jobs.JobInserter
	minInterval time.Duration
	now         func() time.Time
}

// NewEndpoints binds the delegate. Production wires the tenant runner,
// the River client and the env-parsed interval:
//
//	endpoints.Reprocessor = rfqs.NewEndpoints(db, riverClient, reprocessMinInterval(), time.Now)
func NewEndpoints(runner tenantdb.TenantRunner, inserter jobs.JobInserter, minInterval time.Duration, now func() time.Time) *Endpoints {
	return &Endpoints{runner: runner, inserter: inserter, minInterval: minInterval, now: now}
}

// ReprocessRfq implements POST /v1/rfqs/{id}/reprocess. The middleware
// chain (Authenticate + ResolveTenant + RequireRole) has already proven
// identity, membership and role; a missing ctx pair means a mis-mounted
// route and answers the uniform 401 like approve.go's rule.
func (e *Endpoints) ReprocessRfq(
	ctx context.Context,
	req authapi.ReprocessRfqRequestObject,
) (authapi.ReprocessRfqResponseObject, error) {
	orgID, ok := platform.OrgID(ctx)
	userID, ok2 := platform.UserID(ctx)
	if !ok || !ok2 {
		return reprocessUnauthenticated(), nil
	}
	out, err := e.reprocess(ctx, orgID, userID, platform.CorrelationUUID(ctx), uuid.UUID(req.ID))
	if err != nil {
		return nil, err
	}
	return out.response(), nil
}

// reprocessKind is what the tenant transaction decided; response() maps
// it onto the contract's status codes.
type reprocessKind int

const (
	kindAccepted reprocessKind = iota
	kindNotFound
	kindNotParked
	kindRateLimited
)

// reprocessOutcome carries the decision plus what each response shape
// needs — the refused status for 409, the remaining seconds for 429's
// Retry-After and the resumed stage for 202.
type reprocessOutcome struct {
	kind       reprocessKind
	status     string
	stage      jobs.ResumeStage
	retryAfter int
}

// reprocessRfq is the locked slice of app.rfq_requests the decision
// reads — including the reasons it clears (the audit row needs them).
type reprocessRfq struct {
	ID            uuid.UUID
	Status        string
	ReviewReason  *string
	FailureReason *string
	Payload       []byte
	ConfidenceSet bool
}

// clearedReason is the reason the reprocess cleared — review_reason for
// the business parks, failure_reason for the technical dead-letters (the
// columns never both carry a value; review wins under drift because it is
// the column the review queue displays).
func (r reprocessRfq) clearedReason() *string {
	if r.ReviewReason != nil {
		return r.ReviewReason
	}
	return r.FailureReason
}

// reprocess runs the whole Reprocess contract inside one InTenantTx:
// lock → status gate → min-interval gate → resume decision → clear+status
// → audit → enqueue.
func (e *Endpoints) reprocess(
	ctx context.Context,
	orgID, userID, correlationID, rfqID uuid.UUID,
) (reprocessOutcome, error) {
	var out reprocessOutcome
	err := e.runner.InTenantTx(ctx, orgID, func(ctx context.Context, tx pgx.Tx) error {
		r, found, err := lockRfq(ctx, tx, rfqID)
		if err != nil {
			return fmt.Errorf("rfqs: reprocess: lock %s: %w", rfqID, err)
		}
		if !found {
			out = reprocessOutcome{kind: kindNotFound}
			return nil
		}
		if r.Status != statusNeedsReview && r.Status != statusFailed {
			out = reprocessOutcome{kind: kindNotParked, status: r.Status}
			return nil
		}
		limited, err := e.inInterval(ctx, tx, rfqID)
		if err != nil {
			return err
		}
		if limited > 0 {
			out = reprocessOutcome{kind: kindRateLimited, retryAfter: limited}
			return nil
		}
		return e.resume(ctx, tx, orgID, userID, correlationID, r, &out)
	})
	return out, err
}

// lockRfq SELECTs the RFQ FOR UPDATE; RLS scopes the lookup so an unknown
// or other-tenant id is the same found=false (the uniform 404).
func lockRfq(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (reprocessRfq, bool, error) {
	var r reprocessRfq
	var confidence decimal.NullDecimal
	err := tx.QueryRow(ctx, `
		SELECT id, status::text, review_reason, failure_reason,
		       coalesce(extracted_payload_json::text,''), extraction_confidence
		FROM app.rfq_requests WHERE id = $1 FOR UPDATE`, rfqID).
		Scan(&r.ID, &r.Status, &r.ReviewReason, &r.FailureReason, &r.Payload, &confidence)
	if errors.Is(err, pgx.ErrNoRows) {
		return reprocessRfq{}, false, nil
	}
	r.ConfidenceSet = confidence.Valid
	return r, err == nil, err
}

// inInterval is the min-interval gate (spec Reprocess): the newest
// rfq.reprocessed audit row is the last reprocess time. It returns the
// remaining whole seconds when a row is younger than the floor — the
// 429's Retry-After — and 0 when the gate is open. Reading the audit log
// is deliberate: the row every accepted reprocess writes IS the record,
// so no column is added.
func (e *Endpoints) inInterval(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID) (int, error) {
	var last *time.Time
	err := tx.QueryRow(ctx, `
		SELECT max(created_at) FROM app.tenant_audit_logs
		WHERE action = $1 AND entity_type = 'rfq' AND entity_id = $2`,
		actionReprocessed, rfqID).Scan(&last)
	if err != nil {
		return 0, fmt.Errorf("rfqs: reprocess: interval read on %s: %w", rfqID, err)
	}
	if last == nil {
		return 0, nil
	}
	remaining := e.minInterval - e.now().Sub(*last)
	if remaining <= 0 {
		return 0, nil
	}
	return int(math.Ceil(remaining.Seconds())), nil
}

// resume is the commit sequence: the checkpoint walk picks the stage, the
// RFQ takes the stage's active status with both reasons cleared, the
// rfq.reprocessed audit row records actor/cleared reason/stage, and the
// stage's job is staged — all in this tx.
func (e *Endpoints) resume(
	ctx context.Context,
	tx pgx.Tx,
	orgID, userID, correlationID uuid.UUID,
	r reprocessRfq,
	out *reprocessOutcome,
) error {
	d, err := jobs.DecideResume(ctx, tx, r.ID, r.Payload, r.ConfidenceSet, e.now())
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE app.rfq_requests
		SET status = $2, review_reason = NULL, failure_reason = NULL, updated_at = now()
		WHERE id = $1`, r.ID, d.Status); err != nil {
		return fmt.Errorf("rfqs: reprocess: resume %s: %w", r.ID, err)
	}
	if err := e.auditReprocessed(ctx, tx, orgID, userID, r, d); err != nil {
		return err
	}
	if err := e.enqueue(ctx, tx, orgID, correlationID, r.ID, d); err != nil {
		return err
	}
	*out = reprocessOutcome{kind: kindAccepted, stage: d.Stage}
	return nil
}

// auditReprocessed writes the rfq.reprocessed row the spec requires:
// actor_user_id both on the row and in the metadata with the cleared
// reason and the resumed stage.
func (e *Endpoints) auditReprocessed(ctx context.Context, tx pgx.Tx, orgID, userID uuid.UUID, r reprocessRfq, d jobs.ResumeDecision) error {
	if _, err := platform.NewStore(tx).InsertAuditLog(ctx, platform.AuditEntry{
		OrganizationID: orgID,
		ActorUserID:    &userID,
		Action:         actionReprocessed,
		EntityType:     "rfq",
		EntityID:       r.ID,
		Metadata: map[string]any{
			"actor_user_id":  userID.String(),
			"cleared_reason": r.clearedReason(),
			"resumed_stage":  string(d.Stage),
		},
	}); err != nil {
		return fmt.Errorf("rfqs: reprocess: audit %q on %s: %w", actionReprocessed, r.ID, err)
	}
	return nil
}

// enqueue stages the resumed stage's job — ExtractArgs and its
// RateArgs/PriceArgs twins share fields, so one base converts; render is
// the quote-keyed exception. Uniqueness rides each args' InsertOpts:
// entityUnique dedupes only live states, so a terminal job never blocks
// the reprocess re-enqueue (spec Reprocess).
func (e *Endpoints) enqueue(ctx context.Context, tx pgx.Tx, orgID, correlationID, rfqID uuid.UUID, d jobs.ResumeDecision) error {
	if e.inserter == nil {
		return errors.New("rfqs: reprocess: job inserter not configured")
	}
	base := jobs.ExtractArgs{OrganizationID: orgID, CorrelationID: correlationID, RfqID: rfqID}
	var args river.JobArgs
	switch d.Stage {
	case jobs.ResumeExtract:
		args = base
	case jobs.ResumeRates:
		args = jobs.RateArgs(base)
	case jobs.ResumePrice:
		args = jobs.PriceArgs(base)
	case jobs.ResumeRender:
		args = jobs.RenderArgs{
			OrganizationID: orgID,
			CorrelationID:  correlationID,
			RfqID:          rfqID,
			QuoteID:        d.QuoteID,
		}
	default:
		return fmt.Errorf("rfqs: reprocess: unknown resume stage %q", d.Stage)
	}
	if _, err := e.inserter.InsertTx(ctx, tx, args, nil); err != nil {
		return fmt.Errorf("rfqs: reprocess: enqueue %s on %s: %w", d.Stage, rfqID, err)
	}
	return nil
}

// response maps the decision onto the contract's status codes.
func (o reprocessOutcome) response() authapi.ReprocessRfqResponseObject {
	switch o.kind {
	case kindAccepted:
		return authapi.ReprocessRfq202JSONResponse{
			Status:       authapi.Accepted,
			ResumedStage: authapi.ReprocessStage(o.stage),
		}
	case kindNotFound:
		return authapi.ReprocessRfq404JSONResponse{
			NotFoundJSONResponse: authapi.NotFoundJSONResponse{Error: authapi.ErrorCodeNotFound},
		}
	case kindNotParked:
		return authapi.ReprocessRfq409JSONResponse{
			Error:  authapi.ErrorCodeRfqNotParked,
			Status: o.status,
		}
	case kindRateLimited:
		return authapi.ReprocessRfq429JSONResponse{
			Body:    authapi.Error{Error: authapi.ErrorCodeRateLimited},
			Headers: authapi.ReprocessRfq429ResponseHeaders{RetryAfter: &o.retryAfter},
		}
	}
	return authapi.ReprocessRfq500JSONResponse{
		InternalErrorJSONResponse: authapi.InternalErrorJSONResponse{Error: authapi.ErrorCodeInternal},
	}
}

// reprocessUnauthenticated is the fail-closed answer for a mis-mounted
// route — the same body the middleware emits for a bad token.
func reprocessUnauthenticated() authapi.ReprocessRfqResponseObject {
	return authapi.ReprocessRfq401JSONResponse{
		UnauthorizedJSONResponse: authapi.UnauthorizedJSONResponse{Error: authapi.ErrorCodeUnauthenticated},
	}
}
