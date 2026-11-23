package webapi

// GET /v1/inbox/quotes — the org-scoped inbox page: filtered items, the
// five status buckets and the summary counters, all inside one
// InTenantReadTx so the page is one consistent snapshot.
import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/shopspring/decimal"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/auth/authapi"
	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/platform"
)

// ListInboxQuotes implements the inbox page. ResolveTenant on the mount
// already proved membership; an absent org context means a mis-mounted
// route and answers the fail-closed 401 — platform's own rule for a
// missing identity pair.
func (e *Endpoints) ListInboxQuotes(ctx context.Context, req authapi.ListInboxQuotesRequestObject) (authapi.ListInboxQuotesResponseObject, error) {
	orgID, ok := platform.OrgID(ctx)
	if !ok {
		return authapi.ListInboxQuotes401JSONResponse{
			UnauthorizedJSONResponse: authapi.UnauthorizedJSONResponse{Error: authapi.ErrorCodeUnauthenticated},
		}, nil
	}
	if req.Params.Status != nil && !req.Params.Status.Valid() {
		return authapi.ListInboxQuotes400JSONResponse{
			BadRequestJSONResponse: authapi.BadRequestJSONResponse{Error: authapi.ErrorCodeBadRequest},
		}, nil
	}
	var page authapi.InboxQuotePage
	err := e.runner.InTenantReadTx(ctx, orgID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		page, err = e.inboxPage(ctx, tx, req.Params, e.now())
		return err
	})
	if err != nil {
		return nil, err
	}
	return authapi.ListInboxQuotes200JSONResponse(page), nil
}

// inboxPage runs the three reads that make the response — items, counts,
// summary — against the one tx's snapshot.
func (e *Endpoints) inboxPage(ctx context.Context, tx pgx.Tx, p authapi.ListInboxQuotesParams, now time.Time) (authapi.InboxQuotePage, error) {
	items, err := listInboxRows(ctx, tx, p, now)
	if err != nil {
		return authapi.InboxQuotePage{}, err
	}
	counts, err := inboxStatusCounts(ctx, tx)
	if err != nil {
		return authapi.InboxQuotePage{}, err
	}
	summary, err := inboxSummary(ctx, tx, now)
	if err != nil {
		return authapi.InboxQuotePage{}, err
	}
	return authapi.InboxQuotePage{
		Items:        items,
		StatusCounts: counts,
		Summary:      summary,
		RefreshedAt:  now,
	}, nil
}

// listInboxRows is the filtered item read: kind='RFQ' only, IGNORED
// always out, newest first. The status filter expands to its DB statuses
// as a text[] — comparing status::text avoids the enum-array cast — and
// the q needle is an escaped ILIKE over subject, customer and the
// rfq_number's text form (the contract's "RFQ number, customer name or
// subject"). RLS scopes everything to the tx's tenant.
func listInboxRows(ctx context.Context, tx pgx.Tx, p authapi.ListInboxQuotesParams, now time.Time) ([]authapi.InboxQuote, error) {
	statuses, pattern := inboxFilters(p)
	rows, err := tx.Query(ctx, `
		SELECT r.id, r.rfq_number, coalesce(r.customer_name, r.customer_email), r.subject,
		       r.status::text, r.received_at, r.extracted_payload_json, r.extraction_confidence,
		       q.id, q.pricing_result_json
		FROM app.rfq_requests AS r
		LEFT JOIN LATERAL (
			SELECT id, pricing_result_json FROM app.quotes
			WHERE rfq_id = r.id ORDER BY version DESC LIMIT 1
		) AS q ON true
		WHERE r.kind = 'RFQ' AND r.status <> 'IGNORED'
		  AND ($1::text[] IS NULL OR r.status::text = ANY($1))
		  AND ($2::text IS NULL OR r.subject ILIKE $2 ESCAPE '\'
		       OR r.customer_name ILIKE $2 ESCAPE '\'
		       OR r.customer_email ILIKE $2 ESCAPE '\'
		       OR r.rfq_number::text ILIKE $2 ESCAPE '\')
		ORDER BY r.received_at DESC, r.id DESC`, statuses, pattern)
	if err != nil {
		return nil, fmt.Errorf("webapi: inbox: list: %w", err)
	}
	defer rows.Close()
	items := []authapi.InboxQuote{}
	for rows.Next() {
		var r inboxRow
		if err := rows.Scan(&r.id, &r.rfqNumber, &r.customerName, &r.subject,
			&r.status, &r.receivedAt, &r.extraction, &r.confidence,
			&r.quoteVersionID, &r.pricingJSON); err != nil {
			return nil, fmt.Errorf("webapi: inbox: scan: %w", err)
		}
		item, err := mapInboxQuote(r, now)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("webapi: inbox: rows: %w", err)
	}
	return items, nil
}

// inboxFilters turns the contract params into the query's two nullable
// args: the expanded DB status list and the escaped ILIKE pattern — both
// nil when unfiltered, so the query's IS NULL guards collapse away.
func inboxFilters(p authapi.ListInboxQuotesParams) (statuses []string, pattern any) {
	if p.Status != nil {
		statuses = dbStatuses[string(*p.Status)]
	}
	if p.Q != nil {
		if q := strings.TrimSpace(*p.Q); q != "" {
			pattern = "%" + likeEscaper.Replace(truncateRunes(q, maxQueryLen)) + "%"
		}
	}
	return statuses, pattern
}

// inboxStatusCounts is the unfiltered five-bucket tally — the counts
// describe the whole inbox regardless of the item filter, so the client
// can badge every tab on one response.
func inboxStatusCounts(ctx context.Context, tx pgx.Tx) (authapi.InboxStatusCounts, error) {
	rows, err := tx.Query(ctx, `
		SELECT status::text, count(*) FROM app.rfq_requests
		WHERE kind = 'RFQ' AND status <> 'IGNORED'
		GROUP BY status`)
	if err != nil {
		return authapi.InboxStatusCounts{}, fmt.Errorf("webapi: inbox: counts: %w", err)
	}
	defer rows.Close()
	var c authapi.InboxStatusCounts
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return authapi.InboxStatusCounts{}, fmt.Errorf("webapi: inbox: count scan: %w", err)
		}
		c.All += n
		switch inboxStatus(status) {
		case "processing":
			c.Processing += n
		case "awaiting_review":
			c.AwaitingReview += n
		case "ready_to_send":
			c.ReadyToSend += n
		case "sent":
			c.Sent += n
		}
	}
	if err := rows.Err(); err != nil {
		return authapi.InboxStatusCounts{}, fmt.Errorf("webapi: inbox: count rows: %w", err)
	}
	return c, nil
}

// inboxSummary fills the header counters. receivedToday/Yesterday bucket
// by UTC calendar day off the injected clock — the org's own timezone is
// a v1-open question the spec leaves to the UTC default. The SLA block
// reuses the list's own constants: at-risk means an unanswered RFQ past
// slaDueAt - 30min, and the most urgent one is the earliest received.
func inboxSummary(ctx context.Context, tx pgx.Tx, now time.Time) (authapi.InboxSummary, error) {
	s := authapi.InboxSummary{ResponseTargetMinutes: int(responseSLA.Minutes())}
	if err := loadSummaryCounts(ctx, tx, now, &s); err != nil {
		return s, err
	}
	if err := loadMostUrgent(ctx, tx, now, &s); err != nil {
		return s, err
	}
	if err := loadAutoApprove(ctx, tx, &s); err != nil {
		return s, err
	}
	return s, loadAvgResponse(ctx, tx, &s)
}

// loadSummaryCounts fills the day buckets, the ready queue and the at-risk
// tally in one scan over the inbox's base set.
func loadSummaryCounts(ctx context.Context, tx pgx.Tx, now time.Time, s *authapi.InboxSummary) error {
	nowUTC := now.UTC()
	todayStart := time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), 0, 0, 0, 0, time.UTC)
	yesterdayStart := todayStart.AddDate(0, 0, -1)
	tomorrowStart := todayStart.AddDate(0, 0, 1)
	riskCutoff := now.Add(-(responseSLA - slaRiskWindow))
	return tx.QueryRow(ctx, `
		SELECT
		  count(*) FILTER (WHERE r.received_at >= $1 AND r.received_at < $2),
		  count(*) FILTER (WHERE r.received_at >= $3 AND r.received_at < $1),
		  count(*) FILTER (WHERE r.status = 'READY'),
		  count(*) FILTER (WHERE r.status <> 'APPROVED_SENT' AND r.received_at < $4)
		FROM app.rfq_requests AS r
		WHERE r.kind = 'RFQ' AND r.status <> 'IGNORED'`,
		todayStart, tomorrowStart, yesterdayStart, riskCutoff).
		Scan(&s.ReceivedToday, &s.ReceivedYesterday, &s.ReadyToSend, &s.SLAAtRiskCount)
}

// loadMostUrgent finds the earliest received_at still at risk — earliest
// deadline too, since slaDueAt is received_at plus a constant.
func loadMostUrgent(ctx context.Context, tx pgx.Tx, now time.Time, s *authapi.InboxSummary) error {
	var customer string
	var received time.Time
	err := tx.QueryRow(ctx, `
		SELECT coalesce(customer_name, customer_email), received_at
		FROM app.rfq_requests
		WHERE kind = 'RFQ' AND status <> 'IGNORED' AND status <> 'APPROVED_SENT'
		  AND received_at < $1
		ORDER BY received_at ASC, id ASC
		LIMIT 1`, now.Add(-(responseSLA-slaRiskWindow))).
		Scan(&customer, &received)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("webapi: inbox: most urgent: %w", err)
	}
	s.MostUrgentSLARisk = &authapi.SLARiskHighlight{
		CustomerName: customer,
		SLADueAt:     received.Add(responseSLA),
	}
	return nil
}

// loadAutoApprove fills the org's catch-all auto-approve threshold as a
// percent; max() keeps the single-row contract when no rule row exists,
// and the 1.0 fallback is the column's own DEFAULT — never auto-approve.
func loadAutoApprove(ctx context.Context, tx pgx.Tx, s *authapi.InboxSummary) error {
	var threshold decimal.Decimal
	err := tx.QueryRow(ctx, `
		SELECT coalesce(max(auto_approve_threshold), 1.0)
		FROM app.pricing_rules
		WHERE modal = 'ALL' AND origin_locode IS NULL AND dest_locode IS NULL`).
		Scan(&threshold)
	if err != nil {
		return fmt.Errorf("webapi: inbox: auto-approve: %w", err)
	}
	s.AutoApproveConfidencePercent = int(threshold.Mul(decimal.NewFromInt(100)).Round(0).IntPart())
	return nil
}

// loadAvgResponse fills the mean minutes between RFQ receipt and the
// quote version's approval stamp — approved_at is the sent-mark the spec
// pins (the send worker flips status; there is no separate sent clock).
func loadAvgResponse(ctx context.Context, tx pgx.Tx, s *authapi.InboxSummary) error {
	var avg decimal.Decimal
	err := tx.QueryRow(ctx, `
		SELECT coalesce(round(avg(extract(epoch from (q.approved_at - r.received_at)) / 60)), 0)
		FROM app.quotes AS q
		JOIN app.rfq_requests AS r ON r.id = q.rfq_id
		WHERE q.status = 'APPROVED_SENT' AND q.approved_at IS NOT NULL`).
		Scan(&avg)
	if err != nil {
		return fmt.Errorf("webapi: inbox: avg response: %w", err)
	}
	s.AvgResponseMinutes = int(avg.IntPart())
	return nil
}
