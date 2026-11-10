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
.PHONY: generate

OAPI_CODEGEN_VERSION := v2.8.2

generate:
	cd ../../apps/core-api && \
		go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@$(OAPI_CODEGEN_VERSION) \
		-config oapi-codegen.yaml ../../packages/contracts/openapi/core-api.yaml
