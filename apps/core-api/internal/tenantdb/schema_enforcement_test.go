package tenantdb_test

// Behavioural enforcement for the v1.3 data model (spec section 7.3): the
// catalog assertions live in schema_lint_test.go; these tests prove the
// triggers, revokes and SECURITY DEFINER functions actually fire — always on
// the real qp_app role through InTenantTx, never a mock (AGENTS.md, ADR-008
// section 3).

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/NewByteGroup/QuotePilot/apps/core-api/internal/testenv"
)

// TestSchemaEnforcementSnapshotImmutability proves the trigger (not just the
// grants) blocks UPDATE/DELETE: qp_migrator owns the table, so grants cannot
// explain a failure — only block_snapshot_mutation can (plan task 3).
func TestSchemaEnforcementSnapshotImmutability(t *testing.T) {
	testenv.RequirePostgres(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()

	slug := "lint-snap-" + uuid.NewString()
	_, err := migrator.Exec(ctx,
		`INSERT INTO app.connectors (slug, display_name, target_url) VALUES ($1, 'Lint Connector', 'https://example.test/feed')`,
		slug)
	require.NoError(t, err)
	var snapshotID uuid.UUID
	require.NoError(t, migrator.QueryRow(ctx, `
		INSERT INTO app.public_snapshots (connector_id, parser_version, raw_markdown_hash, extracted_rates_json, source_url, scraped_at)
		SELECT id, 'p1', 'sha256:deadbeef', '{}'::jsonb, target_url, now()
		FROM app.connectors WHERE slug = $1
		RETURNING id`, slug).Scan(&snapshotID))

	_, err = migrator.Exec(ctx,
		`UPDATE app.public_snapshots SET confidence_score = 0.5 WHERE id = $1`, snapshotID.String())
	require.Error(t, err, "UPDATE on public_snapshots must raise even as qp_migrator")
	assert.Contains(t, err.Error(), "immutable")
	_, err = migrator.Exec(ctx,
		`DELETE FROM app.public_snapshots WHERE id = $1`, snapshotID.String())
	require.Error(t, err, "DELETE on public_snapshots must raise even as qp_migrator")
	assert.Contains(t, err.Error(), "immutable")
}

// TestSchemaEnforcementApprovedQuoteImmutability exercises
// block_approved_quote_mutation as qp_app: on APPROVED_SENT the only legal
// write is status -> SUPERSEDED alone; SUPERSEDED is then terminal — every
// further UPDATE and every DELETE raises (spec section 7.3, rule 9).
func TestSchemaEnforcementApprovedQuoteImmutability(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()

	org := uuid.New()
	testenv.SeedOrganization(t, org, "lint-org-quote")

	var quoteID uuid.UUID
	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		rfq := seedRFQ(t, ctx, tx, org)
		quoteID = seedQuote(t, ctx, tx, org, rfq)
		approveQuote(t, ctx, tx, quoteID)
		return nil
	}))

	for _, stmt := range []string{
		`UPDATE app.quotes SET final_price_brl = 1.00 WHERE id = $1`,
		`UPDATE app.quotes SET status = 'DRAFT' WHERE id = $1`,
		`DELETE FROM app.quotes WHERE id = $1`,
	} {
		denyTx(t, db, org, stmt, "APPROVED_SENT", quoteID.String())
	}

	// The one permitted mutation: retire the sent version to SUPERSEDED so the
	// correction (a new version) can take its place.
	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE app.quotes SET status = 'SUPERSEDED' WHERE id = $1`, quoteID.String())
		return err
	}), "APPROVED_SENT -> SUPERSEDED is the only legal update")

	// SUPERSEDED is terminal: the freeze now covers UPDATE and DELETE alike.
	for _, stmt := range []string{
		`UPDATE app.quotes SET final_price_brl = 1.00 WHERE id = $1`,
		`DELETE FROM app.quotes WHERE id = $1`,
	} {
		denyTx(t, db, org, stmt, "SUPERSEDED", quoteID.String())
	}
}

// TestSchemaEnforcementSentQuoteLineImmutability exercises
// block_sent_quote_line_mutation: once a quote is sent (or retired), its
// lines are as locked as the header — INSERT, UPDATE and DELETE all raise.
func TestSchemaEnforcementSentQuoteLineImmutability(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()

	org := uuid.New()
	testenv.SeedOrganization(t, org, "lint-org-lines")

	var quoteID, lineID, draftQuoteID, draftLineID uuid.UUID
	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		rfq := seedRFQ(t, ctx, tx, org)
		quoteID = seedQuote(t, ctx, tx, org, rfq)
		lineID = seedQuoteLine(t, ctx, tx, org, quoteID)
		// A second DRAFT quote proves the guard is scoped to sent rows; the
		// (rfq_id, version) unique key gives it its own rfq.
		draftQuoteID = seedQuote(t, ctx, tx, org, seedRFQ(t, ctx, tx, org))
		draftLineID = seedQuoteLine(t, ctx, tx, org, draftQuoteID)
		approveQuote(t, ctx, tx, quoteID)
		return nil
	}))

	// All three write kinds against a sent quote's lines raise.
	denyTx(t, db, org,
		`INSERT INTO app.quote_line_items (organization_id, quote_id, category, source_type, original_currency, original_amount, fx_rate_used, cost_amount_brl, sell_amount_brl, line_confidence)
		 VALUES ($1, $2, 'SURCHARGE', 'MANUAL_OVERRIDE', 'USD', 1.00, 5.2, 5.20, 6.00, 1.0000)`,
		"APPROVED_SENT", org.String(), quoteID.String())
	denyTx(t, db, org, `UPDATE app.quote_line_items SET sell_amount_brl = 1.00 WHERE id = $1`,
		"APPROVED_SENT", lineID.String())
	denyTx(t, db, org, `DELETE FROM app.quote_line_items WHERE id = $1`,
		"APPROVED_SENT", lineID.String())

	// The OLD parent governs UPDATE: repointing a sent quote's line to the
	// DRAFT quote is refused, not detached into mutability (regression for
	// the coalesce(NEW.quote_id, OLD.quote_id) hole). The NEW parent governs
	// too: a DRAFT line cannot be attached to the sent quote either.
	denyTx(t, db, org, `UPDATE app.quote_line_items SET quote_id = $2 WHERE id = $1`,
		"APPROVED_SENT", lineID.String(), draftQuoteID.String())
	denyTx(t, db, org, `UPDATE app.quote_line_items SET quote_id = $2 WHERE id = $1`,
		"APPROVED_SENT", draftLineID.String(), quoteID.String())

	// The same three writes on the DRAFT quote stay legal.
	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		mustExec(t, ctx, tx,
			`UPDATE app.quote_line_items SET sell_amount_brl = 601.00 WHERE id = $1`, draftLineID.String())
		mustExec(t, ctx, tx, `DELETE FROM app.quote_line_items WHERE id = $1`, draftLineID.String())
		return nil
	}), "DRAFT quote lines must stay writable")
}

// TestSchemaEnforcementAppendOnly proves the revokes as qp_app, not just in
// the catalog: writes on private_tariffs, tenant_audit_logs and
// carrier_offers fail with a permission error inside a tenant transaction
// (spec section 7.3).
func TestSchemaEnforcementAppendOnly(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()

	org := uuid.New()
	testenv.SeedOrganization(t, org, "lint-org-append")

	var tariff, offer uuid.UUID
	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		_, tariff = seedBatchTariff(t, ctx, tx, org)
		mustExec(t, ctx, tx,
			`INSERT INTO app.tenant_audit_logs (organization_id, action) VALUES ($1, 'TARIFF_BATCH_UPLOADED')`,
			org.String())
		rfq := seedRFQ(t, ctx, tx, org)
		require.NoError(t, tx.QueryRow(ctx, `
			INSERT INTO app.carrier_offers (organization_id, rfq_id, carrier, offer_payload_json, fetched_at)
			VALUES ($1, $2, 'HAPAG', '{}'::jsonb, now()) RETURNING id`,
			org.String(), rfq.String()).Scan(&offer))
		return nil
	}))

	// UPDATE and DELETE are revoked symmetrically on all three tables.
	denyTx(t, db, org, `UPDATE app.private_tariffs SET carrier_or_agent = 'X' WHERE id = $1`, "permission denied", tariff.String())
	denyTx(t, db, org, `UPDATE app.tenant_audit_logs SET action = 'X' WHERE organization_id = $1`, "permission denied", org.String())
	denyTx(t, db, org, `UPDATE app.carrier_offers SET carrier = 'X' WHERE id = $1`, "permission denied", offer.String())
	denyTx(t, db, org, `DELETE FROM app.private_tariffs WHERE id = $1`, "permission denied", tariff.String())
	denyTx(t, db, org, `DELETE FROM app.tenant_audit_logs WHERE organization_id = $1`, "permission denied", org.String())
	denyTx(t, db, org, `DELETE FROM app.carrier_offers WHERE id = $1`, "permission denied", offer.String())
}

// TestSchemaEnforcementBatchActivation exercises enforce_batch_activation:
// pre-activation edits stay allowed (except the stamp), the one-way
// PENDING_REVIEW -> ACTIVE transition stamps activated_at once, and ACTIVE is
// terminal. DELETE is a grant revocation, not a trigger case.
func TestSchemaEnforcementBatchActivation(t *testing.T) {
	db := testenv.Open(t)
	ctx := context.Background()

	org := uuid.New()
	testenv.SeedOrganization(t, org, "lint-org-batch")

	var batch uuid.UUID
	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		require.NoError(t, tx.QueryRow(ctx,
			`INSERT INTO app.private_tariff_batches (organization_id, source_type) VALUES ($1, 'CSV_UPLOAD') RETURNING id`,
			org.String()).Scan(&batch))
		// Pre-activation edit stays allowed.
		mustExec(t, ctx, tx,
			`UPDATE app.private_tariff_batches SET source_filename = 'tariff.csv' WHERE id = $1`, batch.String())
		return nil
	}), "PENDING_REVIEW -> PENDING_REVIEW edit must be allowed")

	// But the stamp is not editable pre-activation.
	denyTx(t, db, org, `UPDATE app.private_tariff_batches SET activated_at = now() WHERE id = $1`,
		"activated_at", batch.String())

	// The activation transition stamps activated_at by itself.
	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		mustExec(t, ctx, tx,
			`UPDATE app.private_tariff_batches SET status = 'ACTIVE' WHERE id = $1`, batch.String())
		var stamped bool
		return tx.QueryRow(ctx,
			`SELECT activated_at IS NOT NULL FROM app.private_tariff_batches WHERE id = $1`,
			batch.String()).Scan(&stamped)
	}), "activation itself must succeed and stamp activated_at")

	// ACTIVE is terminal: a second activation and any other UPDATE raise.
	for _, stmt := range []string{
		`UPDATE app.private_tariff_batches SET status = 'ACTIVE' WHERE id = $1`,
		`UPDATE app.private_tariff_batches SET activated_at = now() WHERE id = $1`,
		`UPDATE app.private_tariff_batches SET source_filename = 'x.csv' WHERE id = $1`,
	} {
		denyTx(t, db, org, stmt, "ACTIVE", batch.String())
	}

	// DELETE is revoked outright — a bad batch is superseded, never deleted.
	denyTx(t, db, org, `DELETE FROM app.private_tariff_batches WHERE id = $1`,
		"permission denied", batch.String())
}

// TestSchemaEnforcementPlatformFunctions exercises each SECURITY DEFINER
// function on the role that owns it: the three tenant-facing ones as qp_app,
// the platform_* write path as qp_admin — qp_app is denied EXECUTE on the
// platform surface entirely (Devin Review). They return only the documented
// shape, and the underlying tables stay unreadable directly (ADR-005 section
// 2 hygiene rule 5).
func TestSchemaEnforcementPlatformFunctions(t *testing.T) {
	db := testenv.Open(t)
	admin := testenv.AdminDB(t)
	migrator := testenv.MigratorPool(t)
	ctx := context.Background()

	org := uuid.New()
	orgSlug := testenv.SeedOrganization(t, org, "lint-org-fn")
	slug := "lint-ptax-" + uuid.NewString()
	_, err := migrator.Exec(ctx,
		`INSERT INTO app.connectors (slug, display_name, target_url) VALUES ($1, 'Lint PTAX', 'https://olinda.bcb.gov.br/ptax')`,
		slug)
	require.NoError(t, err)
	userEmail := "lint-user-" + uuid.NewString() + "@test.dev"
	var userID uuid.UUID
	require.NoError(t, migrator.QueryRow(ctx,
		`INSERT INTO app.users (email, full_name) VALUES ($1, 'Lint User') RETURNING id`,
		userEmail).Scan(&userID))
	_, err = migrator.Exec(ctx,
		`INSERT INTO app.organization_memberships (organization_id, user_id, role) VALUES ($1, $2, 'OWNER')`,
		org.String(), userID.String())
	require.NoError(t, err)

	// Decoy membership in a different org: list_memberships_for_email must
	// filter on the argument (regression guard for the LANGUAGE sql
	// column-shadows-parameter trap — an unfiltered body returns this row too).
	otherOrg := uuid.New()
	testenv.SeedOrganization(t, otherOrg, "lint-org-other")
	var otherUserID uuid.UUID
	require.NoError(t, migrator.QueryRow(ctx,
		`INSERT INTO app.users (email, full_name) VALUES ($1, 'Other User') RETURNING id`,
		"lint-other-"+uuid.NewString()+"@test.dev").Scan(&otherUserID))
	_, err = migrator.Exec(ctx,
		`INSERT INTO app.organization_memberships (organization_id, user_id, role) VALUES ($1, $2, 'PRICING_ANALYST')`,
		otherOrg.String(), otherUserID.String())
	require.NoError(t, err)

	// Alias match, including "Name <local+tag@domain>" normalization.
	var resolved *uuid.UUID
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.resolve_inbound_org($1)`,
		"Lint Org <"+orgSlug+"+spam@in.test>").Scan(&resolved))
	require.NotNil(t, resolved)
	assert.Equal(t, org, *resolved, "alias resolution must normalize case, display name and plus-tag")
	// The slug fallback resolves only on the canonical inbound domain.
	resolved = nil
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.resolve_inbound_org($1)`, orgSlug+"@in.quotepilot.app").Scan(&resolved))
	require.NotNil(t, resolved)
	assert.Equal(t, org, *resolved, "<slug>@in.quotepilot.app fallback must resolve")
	resolved = nil
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.resolve_inbound_org($1)`, orgSlug+"@in.evil.test").Scan(&resolved))
	assert.Nil(t, resolved, "<slug>@in.evil.test must return NULL — the fallback is domain-pinned")
	// A second '@' must not truncate into a match (review finding): without
	// the split_part(addr,'@',3) guard this parsed as slug@in.quotepilot.app.
	resolved = nil
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.resolve_inbound_org($1)`, orgSlug+"@in.quotepilot.app@evil.test").Scan(&resolved))
	assert.Nil(t, resolved, "<slug>@in.quotepilot.app@evil.test must return NULL — no silent truncation")
	resolved = nil
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT app.resolve_inbound_org($1)`, "nobody@elsewhere.test").Scan(&resolved))
	assert.Nil(t, resolved, "unknown recipient must resolve to NULL")

	// Direct table read is denied even though the function works.
	var direct int
	directErr := db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM app.organizations`).Scan(&direct)
	require.Error(t, directErr, "qp_app must not SELECT app.organizations directly")
	assert.Contains(t, directErr.Error(), "permission denied")

	// Memberships surface the org-switcher row (organization_id, name, slug,
	// role) since migration 0003 (spec 2026-11-10-tenant-resolution-api
	// section 3.3); lookup is case-insensitive. The decoy row must not come
	// back for our email.
	var memberOrg uuid.UUID
	var memberName, memberSlug, memberRole string
	var memberRows int
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT count(*) FROM app.list_memberships_for_email($1)`,
		strings.ToUpper(userEmail)).Scan(&memberRows))
	assert.Equal(t, 1, memberRows, "the function must return only this email's memberships")
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT organization_id, name, slug, role FROM app.list_memberships_for_email($1)`,
		strings.ToUpper(userEmail)).Scan(&memberOrg, &memberName, &memberSlug, &memberRole))
	assert.Equal(t, org, memberOrg)
	assert.Equal(t, "Test "+orgSlug, memberName, "the switcher needs the org's display name")
	assert.Equal(t, orgSlug, memberSlug)
	assert.Equal(t, "OWNER", memberRole)

	// get_current_organization inside a tenant tx; NULL outside one.
	require.NoError(t, db.InTenantTx(ctx, org, func(ctx context.Context, tx pgx.Tx) error {
		var got uuid.UUID
		require.NoError(t, tx.QueryRow(ctx,
			`SELECT (app.get_current_organization()).id`).Scan(&got))
		require.Equal(t, org, got)
		return nil
	}))
	var nilOrg *uuid.UUID
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT (app.get_current_organization()).id`).Scan(&nilOrg))
	assert.Nil(t, nilOrg, "no tenant context must yield NULL, not an ''::uuid error")

	// The tenant credential is locked out of the whole platform surface
	// (Devin Review): every platform_* call as qp_app is a permission error,
	// not a scope error — even teardown is unreachable.
	_, err = db.Pool().Exec(ctx,
		`SELECT app.platform_update_connector_status($1, 'DEGRADED', NULL)`, slug)
	require.Error(t, err, "qp_app must not EXECUTE platform_update_connector_status")
	assert.Contains(t, err.Error(), "permission denied")
	_, err = db.Pool().Exec(ctx,
		`SELECT app.platform_delete_organization($1)`, org.String())
	require.Error(t, err, "qp_app must not EXECUTE platform_delete_organization")
	assert.Contains(t, err.Error(), "permission denied")

	// Platform write path: qp_admin writes Layer C only through the functions.
	var snapshotID uuid.UUID
	require.NoError(t, admin.Pool().QueryRow(ctx, `
		SELECT app.platform_insert_public_snapshot(
			$1, 'p1', 'https://example.test/feed', 'sha256:cafe',
			'platform/snapshots/lint.json', '{}'::jsonb, 0.98, '[]'::jsonb, now())`,
		slug).Scan(&snapshotID))
	require.NotEqual(t, uuid.Nil, snapshotID)
	_, err = admin.Pool().Exec(ctx,
		`SELECT app.platform_update_connector_status($1, 'DEGRADED', 0.42)`, slug)
	require.NoError(t, err)
	// The connector machine is the HEALTHY -> DEGRADED -> HEALING -> HEALTHY
	// cycle only: edges that skip a stage or go backwards are refused.
	_, err = admin.Pool().Exec(ctx,
		`SELECT app.platform_update_connector_status($1, 'HEALTHY', NULL)`, slug)
	require.Error(t, err, "DEGRADED -> HEALTHY must pass through HEALING")
	assert.Contains(t, err.Error(), "not an allowed transition")
	var sessionID uuid.UUID
	require.NoError(t, admin.Pool().QueryRow(ctx, `
		SELECT app.platform_insert_healing_session(
			$1, $2, 'sha256:beef', 'platform/healing/lint.bin',
			'devin-lint-0001', 'https://app.devin.ai/sessions/lint')`,
		slug, snapshotID.String()).Scan(&sessionID))
	require.NotEqual(t, uuid.Nil, sessionID)
	_, err = admin.Pool().Exec(ctx,
		`SELECT app.platform_update_connector_status($1, 'HEALING', NULL)`, slug)
	require.NoError(t, err)
	_, err = admin.Pool().Exec(ctx,
		`SELECT app.platform_update_connector_status($1, 'HEALTHY', NULL)`, slug)
	require.NoError(t, err)
	_, err = admin.Pool().Exec(ctx,
		`SELECT app.platform_update_connector_status($1, 'HEALING', NULL)`, slug)
	require.Error(t, err, "HEALTHY -> HEALING skips DEGRADED")
	assert.Contains(t, err.Error(), "not an allowed transition")

	// Sessions leave OPEN only through platform_update_healing_session:
	// progress first, then a terminal status stamps resolved_at. The direct
	// table reads below stay on qp_app — qp_admin holds no table privileges
	// at all (roles.sql [N]).
	_, err = admin.Pool().Exec(ctx,
		`SELECT app.platform_update_healing_session($1, 'PR_READY', 'https://github.com/x/pull/1', true, NULL)`,
		sessionID.String())
	require.NoError(t, err)
	var status string
	var resolvedAt *string
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT status::text, resolved_at::text FROM app.healing_sessions WHERE id = $1`,
		sessionID.String()).Scan(&status, &resolvedAt))
	assert.Equal(t, "PR_READY", status)
	assert.Nil(t, resolvedAt, "non-terminal status must not stamp resolved_at")
	// The session machine refuses non-forward edges.
	_, err = admin.Pool().Exec(ctx,
		`SELECT app.platform_update_healing_session($1, 'OPEN', NULL, NULL, NULL)`, sessionID.String())
	require.Error(t, err, "PR_READY -> OPEN goes backwards")
	assert.Contains(t, err.Error(), "not an allowed transition")
	_, err = admin.Pool().Exec(ctx,
		`SELECT app.platform_update_healing_session($1, 'MERGED', NULL, NULL, NULL)`, sessionID.String())
	require.NoError(t, err)
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT status::text, resolved_at::text FROM app.healing_sessions WHERE id = $1`,
		sessionID.String()).Scan(&status, &resolvedAt))
	assert.Equal(t, "MERGED", status)
	assert.NotNil(t, resolvedAt, "MERGED must stamp resolved_at")
	// MERGED is terminal: the session cannot reopen with resolved_at stamped.
	_, err = admin.Pool().Exec(ctx,
		`SELECT app.platform_update_healing_session($1, 'OPEN', NULL, NULL, NULL)`, sessionID.String())
	require.Error(t, err, "MERGED is terminal")
	assert.Contains(t, err.Error(), "not an allowed transition")

	// A healing session cannot pin another connector's snapshot: the join in
	// platform_insert_healing_session refuses the cross-connector id.
	otherSlug := "lint-ptax-b-" + uuid.NewString()
	_, err = migrator.Exec(ctx,
		`INSERT INTO app.connectors (slug, display_name, target_url) VALUES ($1, 'Lint PTAX B', 'https://example.test/other')`,
		otherSlug)
	require.NoError(t, err)
	var foreignSnapshot uuid.UUID
	require.NoError(t, admin.Pool().QueryRow(ctx, `
		SELECT app.platform_insert_public_snapshot(
			$1, 'p1', 'https://example.test/other', 'sha256:f00d',
			'platform/snapshots/lint-b.json', '{}'::jsonb, 0.5, '[]'::jsonb, now())`,
		otherSlug).Scan(&foreignSnapshot))
	_, err = admin.Pool().Exec(ctx,
		`SELECT app.platform_insert_healing_session($1, $2, NULL, NULL, NULL, NULL)`,
		slug, foreignSnapshot.String())
	require.Error(t, err, "a session must not pin a snapshot of a different connector")
	assert.Contains(t, err.Error(), "does not belong")

	var lastScrape bool
	require.NoError(t, db.Pool().QueryRow(ctx,
		`SELECT last_scraped_at IS NOT NULL FROM app.connectors WHERE slug = $1`, slug).Scan(&lastScrape))
	assert.True(t, lastScrape, "insert snapshot must refresh connectors.last_scraped_at")
}
