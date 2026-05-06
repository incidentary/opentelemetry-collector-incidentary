# Incidentary Bridge — standalone OTel collector with the Incidentary processor
# pre-loaded.
#
# Multi-arch build (linux/amd64, linux/arm64) is driven from CI via
# `docker buildx build --platform linux/amd64,linux/arm64`. The Dockerfile
# itself uses BuildKit's TARGETOS/TARGETARCH so cross-builds Just Work.

# -----------------------------------------------------------------------------
# Builder
# -----------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace

# Cache dependencies before copying the rest of the source so that source-only
# changes don't bust the module-download layer.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .

# Build a fully-static binary — no libc, no libpthread, no surprises in the
# distroless runtime image.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build \
        -trimpath \
        -ldflags="-s -w" \
        -o /out/incidentaryprocessor \
        ./cmd/incidentaryprocessor

# -----------------------------------------------------------------------------
# Runtime — distroless, non-root, no shell, no package manager
# -----------------------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot

# OCI labels — surfaced by GHCR and consumed by Trivy/Cosign.
LABEL org.opencontainers.image.title="incidentary-bridge"
LABEL org.opencontainers.image.description="OpenTelemetry collector with the Incidentary pre-alert processor pre-loaded."
LABEL org.opencontainers.image.source="https://github.com/incidentary/opentelemetry-collector-incidentary"
LABEL org.opencontainers.image.licenses="Apache-2.0"
LABEL org.opencontainers.image.vendor="Incidentary"

WORKDIR /

COPY --from=builder /out/incidentaryprocessor /incidentaryprocessor

# OTLP gRPC + OTLP HTTP receivers — match the standalone sidecar defaults.
EXPOSE 4317 4318

# Health endpoint placeholder. The real `/health` is wired by the
# health_check extension in the bundled config.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["/incidentaryprocessor", "--health"]

USER 65532:65532

ENTRYPOINT ["/incidentaryprocessor"]
