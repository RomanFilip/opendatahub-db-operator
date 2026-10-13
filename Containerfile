# Dev-only build for local/Kind use. Production/Konflux builds use Dockerfile.konflux.

# Build the manager binary
FROM golang:1.26.5 AS builder
ARG TARGETOS
ARG TARGETARCH

USER 0
WORKDIR /workspace

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# Cache deps before building and copying source so that we don't need to
# re-download as much and so that source changes don't invalidate our
# downloaded layer.
RUN go mod download

# Copy only the source needed to build the manager binary.
COPY cmd/ cmd/
COPY pkg/ pkg/
COPY api/ api/
COPY assets/ assets/
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-$(go env GOARCH)} \
    go build -o /workspace/bin/manager ./cmd/main.go

# Use UBI micro as a minimal runtime image
FROM registry.access.redhat.com/ubi10/ubi-micro:latest
WORKDIR /
COPY --from=builder /workspace/bin/manager .
USER 65532:65532

ENTRYPOINT ["/manager", "operator"]
