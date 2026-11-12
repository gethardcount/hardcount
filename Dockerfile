# Build stage
# Pin the exact patch, not a floating minor tag such as 1.26, so the shipped
# binary carries the patched standard library. With a floating tag the build
# cache can reuse an older patch, and the image ships CVEs that the Grype gate
# (critical) rejects.
#
# This pin must be at least go.mod's `go` directive. The image runs
# GOTOOLCHAIN=local, so an older builder does not download a newer toolchain:
# it stops with "go.mod requires go >= <version>". The go-version-consistency
# step in .github/workflows/ci.yml checks this on every PR, because the docker
# job runs only on main.
# The builder runs on the machine doing the build and cross-compiles for
# the target, so a multi-platform build needs no emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.11-alpine AS builder

RUN apk add --no-cache ca-certificates

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# VERSION is stamped by the CI release job (git tag) or defaults to "dev".
ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
    -ldflags="-s -w -X github.com/gethardcount/hardcount/internal/version.Version=${VERSION}" \
    -o /hardcount ./cmd/hardcount

# Runtime stage — distroless has no shell, no package manager, minimal CVE surface.
# Pinned by digest: the nonroot tag always names the newest build, which can be
# only days old. Dependabot proposes a new digest each month, but it cannot
# read a gcr.io build's age. Before merging that PR, check the build is at
# least 14 days old: timeUploadedMs for the digest in
# https://gcr.io/v2/distroless/static-debian13/tags/list
# Each new digest also brings newer time zone data, which billing dates use:
# nothing imports time/tzdata.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:18f4cc26c9ffa7f20a359298be7deadac5c607c8e03a02b8119f1a5fc2f82c99

LABEL org.opencontainers.image.source="https://github.com/gethardcount/hardcount"
LABEL org.opencontainers.image.description="Hardcount — usage-based billing engine"
LABEL org.opencontainers.image.licenses="MIT"

COPY --from=builder /hardcount /usr/local/bin/hardcount
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

EXPOSE 8080

ENTRYPOINT ["hardcount"]
