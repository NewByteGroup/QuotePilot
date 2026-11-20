# Regenerates every artifact derived from the contract files. Today that is the
# Go server interface in apps/core-api/internal/auth/authapi (ADR-002:
# oapi-codegen std-http-server + strict-server); the Pydantic (ai-engine) and
# TypeScript (web) outputs join when their codegen configs land.
#
# sqlc is deliberately NOT regenerated here: it derives from db/queries +
# db/migrations, not from contracts — the CI contracts-drift job checks it in
# its own step.
#
# `go run <module>@<version>` pins the tool and needs no prior install (the CI
# contracts-drift job runs this step before its dedicated oapi-codegen step, so
# relying on a PATH binary would fail). Keep OAPI_CODEGEN_VERSION in sync with
# the `go install` pin in .github/workflows/ci.yml.
#
# `check` syntax-validates the hand-written JSON Schemas under schemas/; it is a
# prerequisite of `generate` so contracts-drift cannot pass on a broken schema.
#
# `test` runs the schema-behavior suite (unittest + the jsonschema package on
# the same system python3 `check` uses): it proves the contracts accept the
# shapes the pipelines emit, e.g. KG units on AIR tariff rows and the optional
# declared freight/insurance extraction fields (RFC-001 sections 4-5).
.PHONY: generate check test

OAPI_CODEGEN_VERSION := v2.8.2

PNPM := npx --yes pnpm@10.34.9

generate: check
	cd ../../apps/core-api && \
		go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION) \
		-config oapi-codegen.yaml ../../packages/contracts/openapi/core-api.yaml
	$(PNPM) --dir ../.. install --frozen-lockfile
	$(PNPM) --dir ../.. --filter @quotepilot/contracts generate

check:
	@for f in schemas/*.schema.json; do \
		python3 -c "import json,sys; json.load(open(sys.argv[1]))" "$$f" \
			|| exit 1; \
	done
	@echo "contracts: schema JSON syntax OK"

test:
	python3 tests/test_contract_schemas.py -v
