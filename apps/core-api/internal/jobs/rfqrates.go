package jobs

// rfqrates.go — S3a rfq_fetch_rates (spec §4): the conditional
// carrier-offer fetch with its own carrier_offers checkpoint. Only when
// the validated extraction says FCL + IMPORT + a customer-owes-freight
// Incoterm does the job call the bound connector; every terminal outcome
// — OFFER_FOUND, NO_OFFER, FETCH_FAILED, NOT_APPLICABLE — writes its
// checkpoint row AND enqueues rfq_price in the same tenant tx, so a
// carrier outage never strands an RFQ and a pricing retry never calls
// the carrier again.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/aiengine"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/carrier"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/pricing"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/tenantdb"
)

// rateJobTimeout pins the spec-§3 stage table: S3a is one bounded
// carrier call — ~15 s per attempt, three attempts (envelope.go).
const rateJobTimeout = 15 * time.Second

// Outcome vocabulary persisted on carrier_offers.outcome (migration
// 0010's CHECK). The outcome IS the stage's checkpoint — a row, not an
// absence, so retries and Reprocess can tell "fetch completed" apart
// from "fetch never ran".
const (
	outcomeOfferFound    = "OFFER_FOUND"
	outcomeNoOffer       = "NO_OFFER"
	outcomeFetchFailed   = "FETCH_FAILED"
	outcomeNotApplicable = "NOT_APPLICABLE"
)

// NOT_APPLICABLE sub-reasons ride the audit metadata: the row's outcome
// vocabulary is fixed by the schema CHECK, but "freight not owed" and
// "request could not be normalized" are different stories for the
// analyst reading why no call ran.
const (
	reasonFreightNotOwed    = "freight_not_owed"
	reasonIncompleteRequest = "incomplete_request"
)

// RfqRatesWorker is S3a (spec §4): everything happens inside one
// InTenantTx — the RFQ lock, the checkpoint write and the S3b enqueue
// commit or roll back together.
type RfqRatesWorker struct {
	river.WorkerDefaults[RateArgs]
	// Runner is the only DB path a worker may use (ADR-002).
	Runner tenantdb.TenantRunner
	// Inserter is the River client; assigned post-build (see RfqIngestWorker).
	Inserter JobInserter
	// Fetcher is the bound carrier connector (spec S3a: the interface is
	// owned by core — LIVE over ai-engine, the portal or the mirror).
	Fetcher carrier.Fetcher
	// clock is injected so tests pin fetched_at and the request's
	// ready_date fallback deterministically.
	clock func() time.Time
}

// NewRfqRatesWorker builds the worker. main assigns Inserter once the
// River client exists.
//
//	w := jobs.NewRfqRatesWorker(db, carrier.NewLiveFetcher(engine, "CMA_CGM"), time.Now)
//	... client := river.NewClient(...); w.Inserter = client
func NewRfqRatesWorker(runner tenantdb.TenantRunner, fetcher carrier.Fetcher, clock func() time.Time) *RfqRatesWorker {
	return &RfqRatesWorker{Runner: runner, Fetcher: fetcher, clock: clock}
}

// Timeout bounds one attempt (spec §3: ~15 s for S3a).
func (w *RfqRatesWorker) Timeout(*river.Job[RateArgs]) time.Duration { return rateJobTimeout }

// Work runs S3a for one RFQ inside its tenant transaction. A fetch error
// inside the attempt budget propagates for River's retry; deterministic
// anomalies (stored-row drift, wiring bugs) cancel via JobCancel.
func (w *RfqRatesWorker) Work(ctx context.Context, job *river.Job[RateArgs]) error {
	args := job.Args
	cancelCause, err := w.run(ctx, args, rateAttempt(job), rateBudget(job))
	if err != nil {
		return err
	}
	if cancelCause != nil {
		return river.JobCancel(cancelCause)
	}
	return nil
}

// rateAttempt reads the attempt number; a nil JobRow (direct Work calls
// in tests) counts as the first attempt.
func rateAttempt(job *river.Job[RateArgs]) int {
	if job.JobRow == nil {
		return 1
	}
	return job.Attempt
}

// rateBudget reads the attempt budget; 0 (nil JobRow) means "unknown",
// which never takes the final-attempt FETCH_FAILED path.
func rateBudget(job *river.Job[RateArgs]) int {
	if job.JobRow == nil {
		return 0
	}
	return job.MaxAttempts
}

// run wraps the whole stage in the RFQ's tenant transaction.
func (w *RfqRatesWorker) run(ctx context.Context, args RateArgs, attempt, budget int) (cancelCause error, err error) {
	err = w.Runner.InTenantTx(ctx, args.OrganizationID, func(ctx context.Context, tx pgx.Tx) error {
		cancelCause, err = w.stage(ctx, tx, args, attempt, budget)
		return err
	})
	return cancelCause, err
}

// stage sequences the stage inside the tenant transaction: lock →
// checkpoint lookup → owed decision → fetch → outcome commit.
func (w *RfqRatesWorker) stage(ctx context.Context, tx pgx.Tx, args RateArgs, attempt, budget int) (cancelCause error, err error) {
	if w.Fetcher == nil {
		return errors.New("jobs: rfq_fetch_rates: Fetcher not configured"), nil
	}
	rfq, found, err := lockPriceRfq(ctx, tx, args.RfqID)
	if err != nil {
		return nil, fmt.Errorf("jobs: rfq_fetch_rates: lock rfq %s: %w", args.RfqID, err)
	}
	if !found || rfq.Status != rfqPricing {
		return nil, nil // replay or cross-tenant — idempotent no-op
	}
	fields, err := decodePriceExtraction(rfq.Payload)
	if err != nil {
		// PRICING without a decodable payload is S2-contract drift — the
		// gate writes both in one commit. Deterministic: cancel, never
		// retry (spec §3).
		return fmt.Errorf("jobs: rfq_fetch_rates: extraction on %s: %w", rfq.ID, err), nil
	}
	req, owed := rateRequest(fields, w.clock())
	req.CorrelationID = args.CorrelationID
	hash := carrier.RequestHash(req)
	exists, err := checkpointExists(ctx, tx, rfq.ID, hash)
	if err != nil {
		return nil, fmt.Errorf("jobs: rfq_fetch_rates: checkpoint on %s: %w", rfq.ID, err)
	}
	if exists {
		// Replay: the outcome row already exists, so the paid fetch is
		// never repeated (spec S3a) — the S3b enqueue is still ensured.
		return nil, w.enqueuePrice(ctx, tx, args)
	}
	if !owed.fetch {
		cp := rateCheckpoint{outcome: outcomeNotApplicable, reason: owed.reason}
		return nil, w.commitCheckpoint(ctx, tx, args, hash, cp)
	}
	return nil, w.fetchAndCommit(ctx, tx, args, req, hash, attempt, budget)
}

// rateDecision is the owed-freight gate's answer: fetch says whether the
// connector call runs; reason names the NOT_APPLICABLE sub-reason for
// the audit when it does not.
type rateDecision struct {
	fetch  bool
	reason string
}

// rateRequest normalizes the extraction into the carrier request and
// answers the owed-freight question (spec S3a): fetch only when the
// extraction says FCL + derived IMPORT + an Incoterm that puts ocean
// freight on the requesting customer — the same importMatrix freight
// column the engine prices from (pricing.OwesImportFreight). EXPORT
// checkpoints NOT_APPLICABLE even in the freight-owed family: the export
// matrix is still open (RFC-001), so no offer could be used (flagged on
// the PR). Fields that cannot be established mean "no call was owed
// this run" — NOT_APPLICABLE; S3b re-decodes the extraction and parks
// the real gap (REQUIRED_FIELD_MISSING).
func rateRequest(fields map[string]extractedField, now time.Time) (carrier.Request, rateDecision) {
	var req carrier.Request
	modal, _ := parseModalField(fields)
	incoterm, _ := parseIncotermField(fields)
	req.OriginLOCODE, _ = requiredLocode(fields, "origin_locode")
	req.DestLOCODE, _ = requiredLocode(fields, "dest_locode")
	if equip, ok := fieldString(fields, "equipment_or_unit"); ok {
		req.EquipmentOrUnit = strings.ToUpper(equip)
	}
	if n, ok := requiredContainerCount(fields); ok {
		req.Quantity = n
	}
	req.ReadyDate = quoteDateField(fields, now)
	if !freightOwed(modal, incoterm, req.OriginLOCODE, req.DestLOCODE) {
		return req, rateDecision{reason: reasonFreightNotOwed}
	}
	if req.EquipmentOrUnit == "" || req.Quantity == 0 {
		return req, rateDecision{reason: reasonIncompleteRequest}
	}
	return req, rateDecision{fetch: true}
}

// freightOwed is the spec S3a fetch gate: FCL + derived IMPORT + an
// Incoterm whose import-matrix freight column is true. Direction
// ambiguity (both-BR / neither-BR) answers false — nothing owed is
// provable, and S3b parks the gap itself.
func freightOwed(modal pricing.Modal, incoterm pricing.Incoterm, origin, dest string) bool {
	if modal != pricing.ModalFCL || !pricing.OwesImportFreight(incoterm) {
		return false
	}
	dir, ok := deriveDirection(origin, dest)
	return ok && dir == pricing.DirectionImport
}

// checkpointExists reads back the S3a idempotency key: an outcome row
// for this request means the fetch already ran — the replay is a no-op.
// Dedupe is by (rfq_id, request_hash): the request CONTENT is what
// repeats, whatever carrier binding produced the row (the unique key
// carries carrier for provenance breadth, not for replay detection).
func checkpointExists(ctx context.Context, tx pgx.Tx, rfqID uuid.UUID, hash string) (bool, error) {
	var outcome string
	err := tx.QueryRow(ctx, `
		SELECT outcome FROM app.carrier_offers
		WHERE rfq_id = $1 AND request_hash = $2 LIMIT 1`, rfqID, hash).Scan(&outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// fetchAndCommit is the fetch path: call the bound connector, classify
// the answer into a terminal outcome and checkpoint it with the S3b
// enqueue. A fetch error inside the attempt budget propagates for
// River's retry — nothing commits; on the final attempt FETCH_FAILED
// releases pricing anyway (spec S3a: a carrier outage never strands an
// RFQ — it is recoverable info, never a FAILED mark).
func (w *RfqRatesWorker) fetchAndCommit(ctx context.Context, tx pgx.Tx, args RateArgs, req carrier.Request, hash string, attempt, budget int) error {
	cp, ferr := w.offerOutcome(ctx, req)
	if ferr != nil {
		if budget == 0 || attempt < budget {
			return fmt.Errorf("jobs: rfq_fetch_rates: fetch on %s (attempt %d): %w", args.RfqID, attempt, ferr)
		}
		cp = fetchFailedCheckpoint(ferr)
	}
	return w.commitCheckpoint(ctx, tx, args, hash, cp)
}

// offerOutcome turns the fetcher answer into a checkpoint: a validated
// offer becomes OFFER_FOUND (the verbatim wire bytes persist — the exact
// payload the carrier sent is the audit record), a null/absent offer is
// NO_OFFER, and a sanity-bounds violation is a fetch error the caller
// budgets like any other (rule 6: untrusted payload, never persisted).
func (w *RfqRatesWorker) offerOutcome(ctx context.Context, req carrier.Request) (rateCheckpoint, error) {
	res, err := w.Fetcher.FetchOffer(ctx, req)
	if err != nil {
		return rateCheckpoint{}, err
	}
	if res == nil || carrier.IsNoOfferPayload(res.Offer) {
		cp := rateCheckpoint{outcome: outcomeNoOffer}
		if res != nil {
			cp.offerRef = res.OfferReference
		}
		return cp, nil
	}
	offer, err := carrier.ValidateOffer(res.Offer)
	if err != nil {
		return rateCheckpoint{}, err
	}
	return rateCheckpoint{
		outcome:   outcomeOfferFound,
		payload:   res.Offer,
		offerRef:  res.OfferReference,
		expiresAt: &offer.ExpiresAt,
	}, nil
}

// fetchFailedCheckpoint is the exhausted-retry outcome — the row that
// releases pricing while recording the failure class for the audit.
func fetchFailedCheckpoint(err error) rateCheckpoint {
	return rateCheckpoint{
		outcome:   outcomeFetchFailed,
		reason:    fetchErrorClass(err),
		errDetail: truncate(err.Error(), maxFailureReason),
	}
}

// fetchErrorClass separates untrusted-payload violations (contract) from
// transport/engine failures — audit detail only; both retry identically.
func fetchErrorClass(err error) string {
	var v *carrier.OfferViolation
	if errors.As(err, &v) || aiengine.IsContractViolation(err) {
		return "contract_violation"
	}
	return "fetch_error"
}

// rateCheckpoint is one terminal outcome row of S3a: every outcome
// carries request_hash; only OFFER_FOUND carries offer content
// (migration 0010's CHECK).
type rateCheckpoint struct {
	outcome   string
	payload   []byte
	offerRef  *string
	expiresAt *time.Time
	reason    string
	errDetail string
}

// commitCheckpoint writes the outcome row (ON CONFLICT collapses the
// under-lock impossible double write), audits the decision and enqueues
// S3b — row and job commit in the same tenant tx (spec §3).
func (w *RfqRatesWorker) commitCheckpoint(ctx context.Context, tx pgx.Tx, args RateArgs, hash string, cp rateCheckpoint) error {
	if err := w.insertCheckpoint(ctx, tx, args, hash, cp); err != nil {
		return err
	}
	if err := w.auditCheckpoint(ctx, tx, args, hash, cp); err != nil {
		return err
	}
	return w.enqueuePrice(ctx, tx, args)
}

// insertCheckpoint persists the outcome row. carrier is NULL exactly on
// NOT_APPLICABLE (no carrier call was owed); source comes from the bound
// fetcher so a MIRROR row is labeled simulated at the row (RFC-003).
func (w *RfqRatesWorker) insertCheckpoint(ctx context.Context, tx pgx.Tx, args RateArgs, hash string, cp rateCheckpoint) error {
	var carrierCol any
	if cp.outcome != outcomeNotApplicable {
		carrierCol = w.Fetcher.Carrier()
	}
	var payload any
	if cp.payload != nil {
		payload = string(cp.payload)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO app.carrier_offers
			(organization_id, rfq_id, carrier, outcome, source, request_hash,
			 offer_payload_json, offer_reference, fetched_at, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT DO NOTHING`,
		args.OrganizationID, args.RfqID, carrierCol, cp.outcome, w.Fetcher.Source(),
		hash, payload, cp.offerRef, w.clock(), cp.expiresAt); err != nil {
		return fmt.Errorf("jobs: rfq_fetch_rates: checkpoint insert on %s: %w", args.RfqID, err)
	}
	return nil
}

// auditCheckpoint records the decision: outcome, source, request hash and
// the sub-reason/error detail — the provenance the review UI shows.
func (w *RfqRatesWorker) auditCheckpoint(ctx context.Context, tx pgx.Tx, args RateArgs, hash string, cp rateCheckpoint) error {
	meta := map[string]any{
		"outcome":      cp.outcome,
		"source":       w.Fetcher.Source(),
		"request_hash": hash,
	}
	if cp.outcome != outcomeNotApplicable {
		meta["carrier"] = w.Fetcher.Carrier()
	}
	if cp.reason != "" {
		meta["reason"] = cp.reason
	}
	if cp.offerRef != nil {
		meta["offer_reference"] = *cp.offerRef
	}
	if cp.errDetail != "" {
		meta["error"] = cp.errDetail
	}
	return w.audit(ctx, tx, args, "rfq.rates_fetched", meta)
}

// enqueuePrice chains S3b rfq_price in the same transaction; queue and
// uniqueness come from PriceArgs.InsertOpts() (a re-enqueue dedupes on
// live states — spec S3a's "still ensure" replay rule).
func (w *RfqRatesWorker) enqueuePrice(ctx context.Context, tx pgx.Tx, args RateArgs) error {
	if w.Inserter == nil {
		return errors.New("jobs: rfq_fetch_rates: Inserter not configured")
	}
	_, err := w.Inserter.InsertTx(ctx, tx, PriceArgs(args), nil)
	if err != nil {
		return fmt.Errorf("jobs: rfq_fetch_rates: enqueue rfq_price %s: %w", args.RfqID, err)
	}
	return nil
}

// audit appends one tenant_audit_logs row for the RFQ — worker actions
// carry no actor (a machine did this, not a user).
func (w *RfqRatesWorker) audit(ctx context.Context, tx pgx.Tx, args RateArgs, action string, meta map[string]any) error {
	if _, err := platform.NewStore(tx).InsertAuditLog(ctx, platform.AuditEntry{
		OrganizationID: args.OrganizationID,
		Action:         action,
		EntityType:     "rfq",
		EntityID:       args.RfqID,
		Metadata:       meta,
	}); err != nil {
		return fmt.Errorf("jobs: rfq_fetch_rates: audit %q on %s: %w", action, args.RfqID, err)
	}
	return nil
}
