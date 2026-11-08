.PHONY: test test-ai test-web test-integration db-up db-reset lint contracts-drift ci

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

lint:
	$(MAKE) -C apps/core-api lint
	$(MAKE) -C apps/ai-engine lint
	$(MAKE) -C apps/web lint
	pnpm --filter @quotepilot/contracts lint
	@echo "stub: would run actionlint"

contracts-drift:
	pnpm --filter @quotepilot/contracts generate
	git diff --exit-code -- packages/contracts/generated
	@echo "stub: would regenerate Go, Pydantic and sqlc, and fail on any diff"

# db-up is not part of ci yet; add it before test-integration once that target stops being a stub.
ci:
	$(MAKE) lint
	$(MAKE) test
	$(MAKE) test-integration
	$(MAKE) contracts-drift
