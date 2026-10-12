# syntax=docker/dockerfile:1
# One image, three binaries: demo-service, loadgen, slo-engine.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w -X github.com/seshikanthguduri/otel-slo-traffic-monitor/internal/version.Version=${VERSION}" \
      -o /out/ ./cmd/...

# Distroless: no shell, no package manager, runs as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/seshikanthguduri/otel-slo-traffic-monitor" \
      org.opencontainers.image.description="OpenTelemetry SLO/SLA and service traffic monitor" \
      org.opencontainers.image.licenses="Apache-2.0"
COPY --from=build /out/ /usr/local/bin/
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/slo-engine"]
