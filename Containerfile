# Helm sidecar: wraps the Helm v4 Go SDK directly (no helm CLI, no shell)
# behind an HTTP/JSON API on loopback TCP, for another container in the
# same pod to call. See internal/helmrunner and internal/api.
#
# Build stage uses the official Go toolchain image; the final image is
# distroless (no shell, no package manager) to minimize CVE surface. The
# helm sidecar binary is a static Go binary with no libc dependency, so
# distroless/static is sufficient -- it doesn't need distroless/base.

ARG GO_VERSION=1.26
FROM docker.io/library/golang:${GO_VERSION}-bookworm AS builder

WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ ./cmd/
COPY internal/ ./internal/

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/helmsidecar ./cmd/helmsidecar

# distroless/static-debian12 ships CA certificates (needed for TLS to the
# Kubernetes API / OCI registries) and a nonroot (uid 65532) user, but no
# shell and no package manager.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/helmsidecar /helmsidecar

ENV HELM_SIDECAR_PORT=8080
EXPOSE 8080

USER nonroot:nonroot
ENTRYPOINT ["/helmsidecar"]
