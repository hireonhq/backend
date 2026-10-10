# syntax=docker/dockerfile:1

# Multi-stage build following the multi-stage-dockerfile skill:
#   deps (cache) -> build (compile) -> test (gate) -> runtime (minimal, non-root)
# The toolchain is pinned; an unpinned `golang` tag makes builds non-reproducible.
ARG GO_VERSION=1.25.8-alpine

# ---------- deps: module cache only ----------
# go.mod/go.sum are copied alone so editing source does not invalidate the module
# download layer, which is the slowest step in the build.
FROM golang:${GO_VERSION} AS deps
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# ---------- build: compile a static binary ----------
FROM deps AS build
WORKDIR /src
COPY . .

ARG VERSION=dev
ARG COMMIT=unknown

# CGO is disabled: the runtime image has no libc, and pgx is pure Go.
# -trimpath removes local paths, -s -w strip symbols, and ldflags stamp version
# information so a running binary is identifiable.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build \
      -trimpath \
      -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/api ./cmd/api

# ---------- test: gate the image on the suite ----------
# Runs in its own stage so a failure stops the build before the runtime layer is
# produced. The previous pipeline deployed straight to Cloud Run with no test step.
FROM build AS test
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go vet ./... && go test ./...

# ---------- runtime: distroless, non-root, no shell ----------
# distroless/static carries CA certificates (needed for Supabase JWKS and R2 over
# TLS) but no shell or package manager, so there is nothing to pivot to if the
# process is compromised. The binary is statically linked, so no libc is required.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

ARG VERSION=dev
ARG COMMIT=unknown
LABEL org.opencontainers.image.title="hireon-backend" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"

WORKDIR /app
COPY --from=build --chown=nonroot:nonroot /out/api /app/api

# Cloud Run injects PORT; 8080 is the local default.
ENV PORT=8080
EXPOSE 8080

# uid/gid 65532, provided by the base image.
USER nonroot:nonroot
ENTRYPOINT ["/app/api"]
