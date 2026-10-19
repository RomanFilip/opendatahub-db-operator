ARG BUILDER_IMAGE=registry.access.redhat.com/ubi9/go-toolset:1.26@sha256:60fd6fb2998c40ad0d6561c34e83588b986a0cf7771f64e5e5c295ba87101f9b
ARG RUNTIME_IMAGE=registry.redhat.io/ubi9/ubi-minimal-pqc@sha256:b3a0b012de0c578082e1bc5b01c8bc27461b4eeb571a0b1a2b21a07dbd845a62

FROM ${BUILDER_IMAGE} AS builder
ARG TARGETOS=linux
ARG TARGETARCH
ARG HELM_VERSION=4.1.1

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download

COPY api/ api/
COPY assets/ assets/
COPY config/chart/ config/chart/
COPY internal/ internal/
COPY pkg/ pkg/
COPY test/ test/

RUN mkdir -p /tmp/e2e
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go test -c -o /tmp/e2e/e2e-tests ./test/e2e/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /tmp/e2e/gotestsum gotest.tools/gotestsum
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /tmp/e2e/test2json "$(go env GOROOT)/src/cmd/test2json"
RUN GOBIN=/tmp/e2e CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go install helm.sh/helm/v4/cmd/helm@v${HELM_VERSION}
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /tmp/e2e/e2e-cleanup ./test/e2e/cmd/cleanup

FROM ${RUNTIME_IMAGE} AS runner
ARG USER_ID=65532
ARG ODH_E2E_OPERATOR_IMAGE=
ARG ODH_E2E_PLATFORM_TYPE=
ARG ODH_E2E_PLATFORM_VERSION=

ENV ODH_E2E_OPERATOR_IMAGE=${ODH_E2E_OPERATOR_IMAGE} \
    ODH_E2E_PLATFORM_TYPE=${ODH_E2E_PLATFORM_TYPE} \
    ODH_E2E_PLATFORM_VERSION=${ODH_E2E_PLATFORM_VERSION} \
    GOVERSION=go1.26.7

WORKDIR /e2e
COPY --from=builder /tmp/e2e/e2e-tests .
COPY --from=builder /tmp/e2e/gotestsum .
COPY --from=builder /tmp/e2e/test2json .
COPY --from=builder /tmp/e2e/helm .
COPY --from=builder /tmp/e2e/e2e-cleanup .
COPY --from=builder /workspace/config/chart ./chart
COPY test/e2e/runner.sh ./runner.sh
RUN chmod 0755 ./runner.sh && \
    mkdir -p /artifacts/e2e-results && \
    chown -R ${USER_ID}:0 /artifacts && \
    chmod -R g=u /artifacts

USER ${USER_ID}:0

ENTRYPOINT ["/e2e/runner.sh"]
CMD []
