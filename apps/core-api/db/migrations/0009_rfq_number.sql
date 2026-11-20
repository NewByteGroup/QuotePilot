-- 0009: rfq_number + org_rfq_counters (web inbox contract).
--
-- The web inbox addresses an RFQ by a small per-company integer (rfqNumber
-- in core-api.yaml), the same shape quote_number has on quotes. Allocation
-- mirrors org_quote_counters (0002, comment 13): INSERT ... ON CONFLICT
-- (organization_id) DO UPDATE SET next_seq = next_seq + 1 RETURNING
-- next_seq - 1 inside the ingest tenant transaction — the row lock
-- serializes allocation per tenant, so concurrent ingests cannot collide.
-- Existing rows are backfilled in received_at order per organization.
--
-- list_memberships_for_email gains a deterministic ORDER BY: GET
-- /v1/session falls back to the FIRST membership when no org context
-- (header or qp_org cookie) resolves, and an unordered setof made "first"
-- undefined. CREATE OR REPLACE keeps the signature, the grant and the
-- SECURITY DEFINER property; the schema lint pins all three.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE app.org_rfq_counters (
    organization_id uuid PRIMARY KEY REFERENCES app.organizations (id) ON DELETE CASCADE,
    next_seq integer NOT NULL DEFAULT 1
);

ALTER TABLE app.org_rfq_counters ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.org_rfq_counters FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation_policy ON app.org_rfq_counters
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- The backfill reads every tenant's rows and writes every tenant's
-- counters, so FORCE must come off both tables for the window: under it
-- even qp_migrator (the owner) sees only the current_organization_id rows
-- — the UPDATE would silently backfill nothing before SET NOT NULL fails,
-- and the counter INSERT would hit the table's own WITH CHECK. RLS stays
-- ENABLED the whole time — other roles keep filtering; only the owner's
-- FORCE is lifted.
-- +goose StatementBegin
ALTER TABLE app.rfq_requests NO FORCE ROW LEVEL SECURITY;
ALTER TABLE app.org_rfq_counters NO FORCE ROW LEVEL SECURITY;

ALTER TABLE app.rfq_requests ADD COLUMN rfq_number integer;

UPDATE app.rfq_requests AS r
SET rfq_number = s.n
FROM (
    SELECT id, row_number() OVER (PARTITION BY organization_id ORDER BY received_at, id) AS n
    FROM app.rfq_requests
) AS s
WHERE r.id = s.id;

INSERT INTO app.org_rfq_counters (organization_id, next_seq)
SELECT organization_id, max(rfq_number) + 1
FROM app.rfq_requests
GROUP BY organization_id;

ALTER TABLE app.rfq_requests ALTER COLUMN rfq_number SET NOT NULL;
ALTER TABLE app.rfq_requests
    ADD CONSTRAINT rfq_requests_organization_id_rfq_number_key UNIQUE (organization_id, rfq_number);

ALTER TABLE app.rfq_requests FORCE ROW LEVEL SECURITY;
ALTER TABLE app.org_rfq_counters FORCE ROW LEVEL SECURITY;
-- +goose StatementEnd

-- p_email, not email: in a LANGUAGE sql function a column name wins over a
-- parameter name, so an "email" parameter would bind to u.email and the
-- predicate would collapse to a tautology, leaking every membership row.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION app.list_memberships_for_email(p_email text)
    RETURNS TABLE (organization_id uuid, name text, slug text, role app.membership_role)
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = app
AS $fn$
    SELECT m.organization_id, o.name, o.slug, m.role
    FROM app.organization_memberships AS m
    JOIN app.users AS u ON u.id = m.user_id
    JOIN app.organizations AS o ON o.id = m.organization_id
    WHERE lower(u.email) = lower(btrim(p_email))
    ORDER BY m.created_at, m.organization_id;
$fn$;
-- +goose StatementEnd

-- +goose Down
-- Reverse order: function first (independent), then the rfq_number column
-- family, then the counter table. The function is restored exactly as 0003
-- left it — no ORDER BY.
-- +goose StatementBegin
-- p_email, not email: in a LANGUAGE sql function a column name wins over a
-- parameter name, so an "email" parameter would bind to u.email and the
-- predicate would collapse to a tautology, leaking every membership row.
CREATE OR REPLACE FUNCTION app.list_memberships_for_email(p_email text)
    RETURNS TABLE (organization_id uuid, name text, slug text, role app.membership_role)
    LANGUAGE sql
    STABLE
    SECURITY DEFINER
    SET search_path = app
AS $fn$
    SELECT m.organization_id, o.name, o.slug, m.role
    FROM app.organization_memberships AS m
    JOIN app.users AS u ON u.id = m.user_id
    JOIN app.organizations AS o ON o.id = m.organization_id
    WHERE lower(u.email) = lower(btrim(p_email));
$fn$;
-- +goose StatementEnd

-- +goose StatementBegin
ALTER TABLE app.rfq_requests DROP CONSTRAINT rfq_requests_organization_id_rfq_number_key;
ALTER TABLE app.rfq_requests DROP COLUMN rfq_number;
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE app.org_rfq_counters;
-- +goose StatementEnd
