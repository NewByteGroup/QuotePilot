# syntax=docker/dockerfile:1
# ADR-007 §1/§6: the image CI builds once per SHA and staging promotes —
# never rebuilt per environment. Both binaries ship in one image: the
# server is the entrypoint, `migrate` runs the deploy step as qp_migrator
# (`railway run /app/migrate` or the service's pre-deploy command).
#
# Build context is apps/core-api (the self-contained Go module): the CI
# trivy job and `docker build apps/core-api` both assume it.

# The official golang image sets GOTOOLCHAIN=local, so a builder behind
# go.mod's directive refuses the module. GOTOOLCHAIN=auto makes go.mod
# itself the toolchain pin (ADR-002's version record) — the builder tag
# only has to exist, the exact compiler ships with the module.
FROM golang:1.26-alpine AS build
ENV GOTOOLCHAIN=auto
WORKDIR /src
# Dependency layer first: go.mod/go.sum rarely change, so `go mod
# download` stays cached across source-only commits.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Static, stripped binaries — distroless has no libc and no shell.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate

# static-debian12 ships CA certs (the Supabase pooler, Resend and
# ai-engine all speak TLS) and nothing else; :nonroot is uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /out/migrate /app/
USER nonroot
# PORT comes from the environment (PaaS convention, see listenAddr) —
# EXPOSE is documentation only.
EXPOSE 8080
ENTRYPOINT ["/app/server"]
