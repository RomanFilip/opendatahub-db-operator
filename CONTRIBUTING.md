---
description: Non-obvious setup and verification requirements for contributors.
audience: [human, ai]
load: When building, testing, debugging, or reviewing a change
---

# Contributing

Run Make targets from the repository root. Use `make help` for the target list. See [README.md](README.md) for user installation and [ARCHITECTURE.md](ARCHITECTURE.md) for design context.

## Prerequisites

See `go.mod` for the Go version and `.github/workflows/ci.yaml` for CI's Go and helper-tool installation steps.

## Test environments

`make test`, `make test-envtest`, and `make test-e2e-runner` run without an external cluster. The runner target tests cleanup and shell behavior and compiles the real smoke suite; it does not build or execute a container image. Envtest needs API server and etcd assets selected by `setup-envtest`. The [CI workflow](.github/workflows/ci.yaml) is the source of truth for current coverage; it does not exercise the cluster-backed integration or e2e lifecycle suites.

`make test-integration` applies CRDs to the kubeconfig cluster only when its current context matches `TEST_INTEGRATION_CONTEXT` (default: the configured Kind cluster). Set that variable explicitly to use another test cluster, such as OpenShift. The suite requires exclusive use of the cluster-scoped `DatabaseService/default-db-operator` and will not reuse or delete an existing instance.

`make test-e2e` requires an existing Kind cluster and a current context matching `kind-$(KIND_CLUSTER)`; see the [Makefile](Makefile) for the default value. It does not create the cluster. Setup builds and loads the image with Podman, and teardown deletes the configured e2e namespace and `DatabaseService/default-db-operator`, so use an isolated cluster. Uncached `Containerfile` base images may require registry access.

The shift-left runner image is built separately with `make container-build-e2e E2E_TEST_IMG=localhost/opendatahub-db-operator-e2e:dev`. Its `setup`, `test`, and `teardown` modes install the chart, run the compiled smoke suite, and remove test resources. Set `ODH_E2E_OPERATOR_IMAGE`, `ODH_E2E_PLATFORM_TYPE`, and `ODH_E2E_PLATFORM_VERSION` in the Jenkins test container for the target operator image and platform; defaults target the upstream ODH image and an empty platform version. Image lifecycle validation requires an exclusive disposable cluster with cert-manager; the runner does not create a cluster or install cert-manager. Keep the cluster reserved through repeated teardown because the chart uses a cluster-scoped `DatabaseService` singleton and cluster-wide RBAC names. The container writes JUnit, JSON test events, and bounded diagnostics under `/artifacts/e2e-results`.
