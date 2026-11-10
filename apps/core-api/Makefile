# Requires .env at the repo root with POSTGRES_DSN, MIGRATOR_DSN, PG_SUPER_DSN
# and QP_ADMIN_DSN exported (see .env.example for the shapes); the root
# Makefile wraps docker compose.
.PHONY: test integration-test lint generate migrate

test:
	go test -short ./...

# Applies all migrations as qp_migrator, then runs the full suite (the real-Postgres
# tests refuse -short skips and need all four DSNs in the environment).
integration-test:
	go run ./cmd/migrate
	go test -count=1 -race ./...

# Regenerates internal/inbox/inboxdb from db/queries + db/migrations. Output is
# committed; never edit generated files (CI contracts-drift checks the diff).
generate:
	sqlc generate

migrate:
	go run ./cmd/migrate

lint:
	@test -z "$$(gofmt -l . | tee /dev/stderr)" \
		|| (echo "gofmt: files listed above are not formatted" && exit 1)
	go vet ./...
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run; \
	else \
		echo "golangci-lint not installed; ran gofmt + go vet only"; \
	fi
