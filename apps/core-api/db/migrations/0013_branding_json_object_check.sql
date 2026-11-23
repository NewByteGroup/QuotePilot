-- 0013: constrain app.organizations.branding_json to a JSON object.
--
-- `GET /me/organization` decodes branding_json as an object; a scalar or
-- array value would fail at read time, after the row committed. The data-
-- model v1.3 follow-up fixes the shape at the column (adr-rfc-plan open
-- decisions). The column is NOT NULL DEFAULT '{}' so the CHECK only needs
-- the typeof predicate — there is no NULL case to admit.

-- +goose Up
-- +goose StatementBegin
ALTER TABLE app.organizations
    ADD CONSTRAINT organizations_branding_json_object
    CHECK (jsonb_typeof(branding_json) = 'object');
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE app.organizations
    DROP CONSTRAINT organizations_branding_json_object;
-- +goose StatementEnd
