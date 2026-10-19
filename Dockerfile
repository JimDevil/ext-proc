# syntax=docker/dockerfile:1.7
# Multi-stage build for ext-proc.
#
# The build stage compiles a fully static binary from the in-repo vendor/
# directory (GOPROXY=off — an incomplete vendor dir fails loudly instead of
# hitting the network). The runtime stage is scratch: just the binary and CA
# certificates, running as an unprivileged user.

# ---------- build ----------
ARG GO_VERSION=1.26
FROM golang:${GO_VERSION}-alpine AS builder

# Build metadata, injected into main.version/commit/date.
ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

# BuildKit injects these per target platform in multi-arch builds.
ARG TARGETOS
ARG TARGETARCH

# Offline build: vendor/ ships with the repo, never download toolchains or
# modules — an incomplete vendor dir fails loudly instead.
ENV GOTOOLCHAIN=local GOPROXY=off

WORKDIR /src

# Dependency layer first (vendor/ ships with the repo) for cache reuse.
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY . .

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
      -mod=vendor -o /out/ext-proc .

# ---------- runtime ----------
FROM scratch AS runtime

ARG VERSION=dev
ARG COMMIT=none
ARG BUILD_DATE=unknown

LABEL org.opencontainers.image.title="ext-proc" \
      org.opencontainers.image.description="Envoy ext_proc plugin responsibility-chain server" \
      org.opencontainers.image.source="https://github.com/JimDevil/ext-proc" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}" \
      org.opencontainers.image.licenses="Apache-2.0"

# CA certificates: the processor dials Envoy over gRPC and plugins may dial
# TLS endpoints; scratch has no trust store of its own.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/ext-proc /app/ext-proc

# Default config baked for bare `docker run`; in k8s mount a ConfigMap over
# /app/manifest/config to override it.
COPY manifest/config/config.yaml /app/manifest/config/config.yaml

ENV SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt

WORKDIR /app
EXPOSE 50051 19090
STOPSIGNAL SIGTERM

# Numeric user (scratch has no /etc/passwd); matches the distroless
# nonroot convention.
USER 65532:65532

# Works in scratch: the binary performs the HTTP request itself.
HEALTHCHECK --interval=10s --timeout=2s --start-period=5s --retries=3 \
  CMD ["/app/ext-proc", "--healthcheck"]

ENTRYPOINT ["/app/ext-proc"]
