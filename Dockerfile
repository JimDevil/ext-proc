# Multi-stage build for ext-proc.
#
# The build stage compiles a fully static binary from the in-repo vendor/
# directory (GOPROXY=off — an incomplete vendor dir fails loudly instead of
# hitting the network). The runtime stage is scratch: just the binary and CA
# certificates, running as an unprivileged user.

# ---------- build ----------
ARG GO_VERSION=1.26
FROM golang:${GO_VERSION}-alpine AS builder

# BuildKit injects these per target platform in multi-arch builds.
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT

# Offline build: vendor/ ships with the repo, never download toolchains or
# modules — an incomplete vendor dir fails loudly instead.
ENV GOTOOLCHAIN=local GOPROXY=off

WORKDIR /src

# Dependency layer first (vendor/ ships with the repo) for cache reuse.
COPY go.mod go.sum ./
COPY vendor/ vendor/
COPY . .

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w" -mod=vendor -o /out/ext-proc .

# ---------- runtime ----------
FROM scratch AS runtime

# CA certificates: the processor dials Envoy over gRPC and plugins may dial
# TLS endpoints; scratch has no trust store of its own.
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /out/ext-proc /app/ext-proc

# Default config baked for bare `docker run`; in k8s mount a ConfigMap over
# /app/manifest/config to override it.
COPY manifest/config/config.yaml /app/manifest/config/config.yaml

WORKDIR /app
EXPOSE 50051 19090

# Numeric user (scratch has no /etc/passwd); matches the distroless
# nonroot convention.
USER 65532:65532

ENTRYPOINT ["/app/ext-proc"]
