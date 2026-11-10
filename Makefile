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
# roles.sql gates the init-phase role model; platform-functions.sql asserts the
# qp_admin/qp_app function-EXECUTE surface (vacuous-true before migrations,
# real afterwards — safe to run in either phase).
db-check:
	docker compose exec -T db sh -c 'psql -X -v ON_ERROR_STOP=1 -U "$$POSTGRES_USER" -d "$$POSTGRES_DB"' < infra/postgres/checks/roles.sql
	docker compose exec -T db sh -c 'psql -X -v ON_ERROR_STOP=1 -U "$$POSTGRES_USER" -d "$$POSTGRES_DB"' < infra/postgres/checks/platform-functions.sql

# Proves each roles.sql/platform-functions.sql assertion fails on a mutation; every mutation runs in a rolled-back transaction.
db-check-negative:
	bash infra/postgres/checks/negative-tests.sh

lint:
	$(MAKE) -C apps/core-api lint
	$(MAKE) -C apps/ai-engine lint
	$(MAKE) -C apps/web lint
	@echo "stub: would run actionlint and OpenAPI lint"

contracts-drift:
	$(MAKE) -C packages/contracts generate
	cd apps/core-api && go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1 generate -f sqlc.yaml
	git diff --exit-code
	@untracked="$$(git ls-files --others --exclude-standard)"; \
	if [ -n "$$untracked" ]; then \
		echo "contracts-drift: regeneration produced untracked files:"; \
		echo "$$untracked"; exit 1; \
	fi

# Local parity with the CI workflow (.github/workflows/ci.yml): lint, unit
# tests, bring Postgres up, prove the role model, then the integration suite
# and the contracts drift check. `db-up` needs `.env` — `cp .env.example .env`
# and fill it in first.
ci:
	$(MAKE) lint
	$(MAKE) test
	$(MAKE) db-up
	$(MAKE) db-check
	$(MAKE) db-check-negative
	$(MAKE) test-integration
	$(MAKE) contracts-drift
