-- Ops console reads (ADR-005 platform path). Schema lives in db/migrations;
-- sqlc generates internal/inbox/inboxdb. These run as qp_app OUTSIDE any
-- tenant transaction — the ops surface has no organization context (the
-- RequirePlatformAdmin gate, not RLS, is the boundary). app.connectors and
-- app.healing_sessions are Layer C: qp_app keeps SELECT, so plain queries
-- suffice. app.inbound_quarantine is Layer A (REVOKE ALL); it is read only
-- through the migration-0018 SECURITY DEFINER function — never a direct
-- table reference here.

-- name: ListOpsConnectors :many
SELECT id, slug, display_name, target_url, status::text AS status,
       confidence_score, last_scraped_at, created_at, updated_at
FROM app.connectors
ORDER BY slug;

-- name: ListOpsHealingSessions :many
SELECT id, connector_id, status::text AS status, failing_snapshot_id,
       failing_payload_hash, failing_payload_storage_key,
       devin_session_id, devin_session_url, github_pr_url, pytest_passed,
       created_at, resolved_at
FROM app.healing_sessions
ORDER BY created_at DESC, id DESC;
