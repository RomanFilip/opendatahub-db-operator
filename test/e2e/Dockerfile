ARG BUILDER_IMAGE=registry.access.redhat.com/ubi9/go-toolset:latest
ARG BASE_IMAGE=registry.access.redhat.com/ubi9/ubi-minimal:latest

FROM ${BUILDER_IMAGE} AS builder
ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG HELM_VERSION=3.22.1

USER root
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY assets/ assets/
COPY internal/ internal/
COPY pkg/ pkg/
COPY test/ test/

ENV GOEXPERIMENT=strictfipsruntime

# A single package maps to one binary; add a separate go test -c invocation
# here if tests are ever split into sub-packages.
RUN CGO_ENABLED=1 GOEXPERIMENT=strictfipsruntime \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go test -c -race -o /e2e-tests ./test/e2e/

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /gotestsum gotest.tools/gotestsum

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /test2json $(go env GOROOT)/src/cmd/test2json

RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -o /e2e-run ./test/e2e/cmd/run/

RUN curl -fsSL "https://get.helm.sh/helm-v${HELM_VERSION}-${TARGETOS}-${TARGETARCH}.tar.gz" -o /tmp/helm.tar.gz && \
    curl -fsSL "https://get.helm.sh/helm-v${HELM_VERSION}-${TARGETOS}-${TARGETARCH}.tar.gz.sha256sum" -o /tmp/helm.tar.gz.sha256sum && \
    awk '{print $1 "  /tmp/helm.tar.gz"}' /tmp/helm.tar.gz.sha256sum | sha256sum -c && \
    tar -xzf /tmp/helm.tar.gz "${TARGETOS}-${TARGETARCH}/helm" && \
    mv "${TARGETOS}-${TARGETARCH}/helm" /helm && \
    rm -rf /tmp/helm.tar.gz /tmp/helm.tar.gz.sha256sum "${TARGETOS}-${TARGETARCH}"

FROM --platform=$TARGETPLATFORM ${BASE_IMAGE}
ARG USER_ID=65532
WORKDIR /e2e

COPY --from=builder /e2e-tests .
COPY --from=builder /gotestsum /usr/local/bin/gotestsum
COPY --from=builder /test2json /usr/local/bin/test2json
COPY --from=builder /e2e-run .
COPY --from=builder /helm /usr/local/bin/helm
COPY config/chart/ chart/

RUN mkdir -p /artifacts && chown ${USER_ID}:${USER_ID} /artifacts

USER ${USER_ID}:${USER_ID}

LABEL com.redhat.component=odh-db-operator
LABEL description="RHOAI database operator e2e tests"
LABEL io.k8s.description="RHOAI database operator e2e tests"
LABEL io.k8s.display-name="RHOAI database operator e2e tests"
LABEL io.openshift.tags="openshift,operator,e2e,database"
LABEL name="rhoai/odh-db-operator-e2e-rhel9"
LABEL summary="RHOAI database operator e2e tests"

# Default: run all tests. Pass -test.run=<pattern> to select specific tests,
# or pass "setup" / "teardown" to manage the operator Helm release.
ENTRYPOINT ["/e2e/e2e-run"]
CMD []
