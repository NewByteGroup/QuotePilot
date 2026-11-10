-- Mission 3 spike gate: two tenant tables proving the ADR-003 role model.
-- Applied by qp_migrator (cmd/migrate); qp_app receives DML through the
-- ALTER DEFAULT PRIVILEGES in infra/postgres/init/01-roles-and-schemas.sh,
-- so this file deliberately contains no GRANTs.
--
-- Both policies implement AGENTS.md rule 4:
--   organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid
-- With no tenant context the expression is NULL, so an unscoped connection
-- yields zero rows instead of raising on ''::uuid.

-- +goose Up
-- +goose StatementBegin
CREATE TABLE app.inbound_messages (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    resend_message_id text NOT NULL,
    subject text NOT NULL DEFAULT '',
    declared_value_usd numeric(14, 2) NOT NULL DEFAULT 0,
    received_at timestamptz NOT NULL DEFAULT now(),
    UNIQUE (organization_id, resend_message_id)
);

ALTER TABLE app.inbound_messages ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.inbound_messages FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON app.inbound_messages
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TABLE app.private_tariff_embeddings (
    id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    organization_id uuid NOT NULL,
    -- pgvector 0.8 lives in schema extensions (ADR-003 section 4); qualified so
    -- the type resolves even if search_path is not what we expect.
    embedding extensions.vector(1024) NOT NULL,
    embedding_model text NOT NULL
);

ALTER TABLE app.private_tariff_embeddings ENABLE ROW LEVEL SECURITY;
ALTER TABLE app.private_tariff_embeddings FORCE ROW LEVEL SECURITY;

CREATE POLICY tenant_isolation ON app.private_tariff_embeddings
    AS PERMISSIVE FOR ALL
    USING (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid)
    WITH CHECK (organization_id = NULLIF(current_setting('app.current_organization_id', true), '')::uuid);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE app.private_tariff_embeddings;
DROP TABLE app.inbound_messages;
-- +goose StatementEnd
