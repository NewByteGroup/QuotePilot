.PHONY: test test-ai test-web test-integration db-up db-reset db-check db-check-negative lint contracts-drift ci

test:
	$(MAKE) -C apps/core-api test
	$(MAKE) -C apps/ai-engine test
	$(MAKE) -C apps/web test

test-ai:
	$(MAKE) -C apps/ai-engine test

test-web:
	$(MAKE) -C apps/web test

test-integration:
	$(MAKE) -C apps/core-api integration-test

# Requires .env: `cp .env.example .env` and fill in the values.
db-up:
	docker compose up -d --wait db

# Destroys the local Postgres volume and re-runs infra/postgres/init/ on a fresh one.
db-reset:
	docker compose down -v
	$(MAKE) db-up

# Runs infra/postgres/checks/roles.sql against the running db container (read-only assertions).
db-check:
	docker compose exec -T db sh -c 'psql -X -v ON_ERROR_STOP=1 -U "$$POSTGRES_USER" -d "$$POSTGRES_DB"' < infra/postgres/checks/roles.sql

# Proves each roles.sql assertion fails on a mutation; every mutation runs in a rolled-back transaction.
db-check-negative:
	bash infra/postgres/checks/negative-tests.sh

lint:
	$(MAKE) -C apps/core-api lint
	$(MAKE) -C apps/ai-engine lint
	$(MAKE) -C apps/web lint
	@echo "stub: would run actionlint and OpenAPI lint"

contracts-drift:
	@echo "stub: would regenerate Go, Pydantic and TypeScript from packages/contracts and sqlc, and fail on any diff"

# db-up is not part of ci yet; add it before test-integration once that target stops being a stub.
ci:
	$(MAKE) lint
	$(MAKE) test
	$(MAKE) test-integration
	$(MAKE) contracts-drift
