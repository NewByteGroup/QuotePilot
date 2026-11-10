-- Catalog stub for sqlc ONLY — this file is NOT in db/migrations and is never
-- applied by goose. The schemas are provisioned by
-- infra/postgres/init/01-roles-and-schemas.sh: qp_migrator has no
-- database-level CREATE (ADR-003 section 1), so a migration can never run
-- CREATE SCHEMA, not even idempotently. sqlc only needs the names in its
-- catalog to resolve `app.*` relations and the `extensions.vector` type.
CREATE SCHEMA app;
CREATE SCHEMA river;
CREATE SCHEMA extensions;
CREATE EXTENSION vector SCHEMA extensions;
