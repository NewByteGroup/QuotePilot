-- 0017: constrain app.organizations.inbound_email_alias to a resolvable shape.
--
-- resolve_inbound_org (0002) normalizes the recipient: lowercase, plus-tag
-- stripped, then tries exact alias match before the <slug>@in.quotepilot.app
-- fallback. Two stored shapes are therefore bugs, not data:
--
--   * alias ending in @in.quotepilot.app — the slug fallback already resolves
--     that address. Storing it as an alias shadows the slug lookup for any
--     OTHER org holding that slug: the exact match runs first and would pin
--     the message to whichever org registered the alias. The canonical
--     domain is resolved by slug, never by alias row.
--   * alias containing '+' — normalization strips the tag before lookup, so
--     'a+b@x' never equals normalized 'a@x'. The row would sit permanently
--     unmatchable, worse than absent.
--
-- Deferred from the data-model v1.3 open items until now: both CHECKs are
-- additive (existing lowercase CHECK untouched) and there is still no
-- onboarding mutation path writing this column — nothing can violate today.
-- The column stores a full address; only the domain-suffix form is pinned.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE app.organizations
    ADD CONSTRAINT organizations_alias_not_inbound_domain
    CHECK (inbound_email_alias NOT LIKE '%@in.quotepilot.app'),
    ADD CONSTRAINT organizations_alias_no_plus_tag
    CHECK (position('+' IN inbound_email_alias) = 0);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE app.organizations
    DROP CONSTRAINT organizations_alias_not_inbound_domain,
    DROP CONSTRAINT organizations_alias_no_plus_tag;
-- +goose StatementEnd
