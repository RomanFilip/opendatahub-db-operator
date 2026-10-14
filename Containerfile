# ODH Konflux CI and local/Kind builds use this Containerfile. RHOAI uses Dockerfile.konflux.

# Build the manager binary
FROM registry.access.redhat.com/ubi9/go-toolset:1.26@sha256:f9e171ed4a2ecfd284b6d977d60f6a0ed33eac8628ad806a80cdf3521cb8bede AS builder
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
COPY internal/ internal/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-$(go env GOARCH)} \
    go build -o /workspace/bin/manager ./cmd/main.go

# Use UBI micro as a minimal runtime image
FROM registry.access.redhat.com/ubi10/ubi-micro:latest
WORKDIR /
COPY --from=builder /workspace/bin/manager .
USER 65532:65532

ENTRYPOINT ["/manager", "operator"]
