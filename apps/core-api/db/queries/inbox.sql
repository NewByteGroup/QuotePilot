-- Inbox queries. Schema lives in db/migrations; sqlc generates internal/inbox/inboxdb.
-- Idempotent insert: replays hit the (organization_id, resend_message_id)
-- unique key, DO NOTHING, and report 0 rows via :execrows.

-- name: InsertInboundMessage :execrows
INSERT INTO app.inbound_messages (organization_id, resend_message_id, subject, declared_value_usd)
VALUES ($1, $2, $3, $4)
ON CONFLICT (organization_id, resend_message_id) DO NOTHING;

-- name: GetInboundMessage :one
SELECT id, organization_id, resend_message_id, subject, declared_value_usd, received_at
FROM app.inbound_messages
WHERE organization_id = $1 AND resend_message_id = $2;

-- name: CountInboundMessagesForOrg :one
SELECT count(*) FROM app.inbound_messages
WHERE organization_id = $1;
