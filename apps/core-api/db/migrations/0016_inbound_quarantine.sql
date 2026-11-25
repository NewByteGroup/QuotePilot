-- 0016: the inbound quarantine RFC-002 reserved for the ADR-005 section 7
-- drop path. Until now a webhook whose recipients resolved to zero or
-- several organizations was acknowledged-and-dropped with only a log line —
-- the right delivery answer (a non-2xx would make Svix replay a permanently
-- undeliverable message for ~27h) but no audit trail and no count. This
-- migration adds both: one row per distinct dropped message, and a shared
-- platform counter that reads as "how many inbound mails we could not route".
--
-- app.inbound_quarantine is Layer A like the 0003/0004 auth tables, for the
-- same structural reason plus one of its own: the drop happens precisely
-- BECAUSE no unique tenant context exists — a no_match row has no org to
-- scope to and a multi_match row has several, so organization_id cannot be
-- carried at all and RLS has nothing to filter on. REVOKE ALL undoes the
-- migrator default grant; qp_app reaches the table only through
-- record_inbound_quarantine, and reaches nothing back out — qp_app can
-- never SELECT it, so hammering the webhook cannot probe whether a given
-- message landed in quarantine. qp_admin receives nothing, same as the
-- outbox family.
--
-- app.platform_counters is the generic ops-counter companion: one row per
-- named counter, Layer A for the same reason — counters span tenants by
-- definition. Today it holds only inbound_unmatched_total; the row shape is
-- deliberately generic so a future counter needs no schema change.
--
-- Threat model: every column of a quarantine row is attacker-controlled
-- mail metadata (the to array, subject, from) or our own resolution output
-- (reason, matched ids). It is triage data only — nothing downstream
-- consumes it, and it carries no credential class like 0004's link_url, so
-- the honest residual is smaller: a compromised qp_app could insert junk
-- quarantine rows through the function, inflating the counter and filling
-- the table with noise, but could still neither read the table nor reach a
-- tenant row through it. The function guards its own inputs (non-empty key,
-- known reason) so a buggy caller cannot collapse every drop onto one
-- dedupe key or a nonsense reason.
--
-- resend_message_id is UNIQUE because a drop is recorded per MESSAGE, not
-- per delivery: Svix redelivers when the 2xx response is lost, and the
-- second delivery must find the row already there — no second row, no
-- second count. The counter therefore counts distinct quarantined
-- messages, not deliveries.

-- +goose Up

-- =========================================================================
-- 1. The tables. Layer A: no RLS (no organization_id to scope on — the
-- absence of a unique org is the thing being recorded) and no qp_app
-- grant, functions only. No organizations FK on matched_org_ids: the
-- candidates may outlive an org teardown, and the row is an audit record
-- of what resolution saw, not a live reference. No index beyond the
-- dedupe key — drops are rare and triage scans the table whole.
-- =========================================================================
-- +goose StatementBegin
CREATE TABLE app.inbound_quarantine (
    id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    resend_message_id text NOT NULL UNIQUE,
    reason            text NOT NULL CHECK (reason IN ('no_match', 'multi_match')),
    recipients        jsonb NOT NULL,
    matched_org_ids   uuid[] NOT NULL DEFAULT '{}',
    subject           text NOT NULL DEFAULT '',
    from_addr         text NOT NULL DEFAULT '',
    received_at       timestamptz NOT NULL,
    quarantined_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE app.platform_counters (
    key        text PRIMARY KEY,
    value      bigint NOT NULL DEFAULT 0,
    updated_at timestamptz NOT NULL DEFAULT now()
);

REVOKE ALL ON app.inbound_quarantine FROM qp_app;
REVOKE ALL ON app.platform_counters FROM qp_app;
-- +goose StatementEnd

-- =========================================================================
-- 2. record_inbound_quarantine: the drop path's only write. Insert-or-skip
-- on the dedupe key, and bump inbound_unmatched_total only when the row
-- was new — the boolean tells the caller which happened. Everything runs
-- inside the function's single call, so a redelivery racing the first
-- delivery still lands exactly one row and exactly one count.
-- =========================================================================
-- +goose StatementBegin
CREATE FUNCTION app.record_inbound_quarantine(
    p_resend_message_id text,
    p_reason text,
    p_recipients jsonb,
    p_matched_org_ids uuid[],
    p_subject text,
    p_from_addr text,
    p_received_at timestamptz
) RETURNS boolean
    LANGUAGE plpgsql
    SECURITY DEFINER
    SET search_path = pg_catalog, pg_temp
AS $fn$
DECLARE
    v_id uuid;
BEGIN
    -- An empty key would dedupe every drop onto one row and freeze the
    -- counter at 1; an unknown reason would pass the CHECK as a raw
    -- 23514. Both are refused with the offending value first.
    IF p_resend_message_id IS NULL OR btrim(p_resend_message_id) = '' THEN
        RAISE EXCEPTION 'record_inbound_quarantine: resend_message_id must be a non-empty string, got ''%''', p_resend_message_id;
    END IF;
    IF p_reason IS NULL OR p_reason NOT IN ('no_match', 'multi_match') THEN
        RAISE EXCEPTION 'record_inbound_quarantine: reason must be no_match or multi_match, got ''%''', p_reason;
    END IF;

    INSERT INTO app.inbound_quarantine AS q
        (resend_message_id, reason, recipients, matched_org_ids, subject, from_addr, received_at)
    VALUES
        (p_resend_message_id, p_reason, p_recipients,
         coalesce(p_matched_org_ids, '{}'), coalesce(p_subject, ''), coalesce(p_from_addr, ''),
         p_received_at)
    ON CONFLICT (resend_message_id) DO NOTHING
    RETURNING q.id INTO v_id;

    IF v_id IS NULL THEN
        -- A redelivery of an already-recorded drop: the row exists and the
        -- counter already counted this message.
        RETURN false;
    END IF;

    INSERT INTO app.platform_counters AS c (key, value)
    VALUES ('inbound_unmatched_total', 1)
    ON CONFLICT (key) DO UPDATE
        SET value = c.value + 1,
            updated_at = now();
    RETURN true;
END;
$fn$;

REVOKE EXECUTE ON FUNCTION app.record_inbound_quarantine(text, text, jsonb, uuid[], text, text, timestamptz) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION app.record_inbound_quarantine(text, text, jsonb, uuid[], text, text, timestamptz) TO qp_app;
-- +goose StatementEnd

-- =========================================================================
-- +goose Down
-- The function first (its body depends on both tables), then the tables.
-- =========================================================================
-- +goose StatementBegin
DROP FUNCTION app.record_inbound_quarantine(text, text, jsonb, uuid[], text, text, timestamptz);
-- +goose StatementEnd

-- +goose StatementBegin
DROP TABLE app.inbound_quarantine;
DROP TABLE app.platform_counters;
-- +goose StatementEnd
