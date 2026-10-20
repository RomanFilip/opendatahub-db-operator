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

`make test` is an alias for `make test-unit`, which selects unit-test packages through `UNIT_TEST_PACKAGES` in the [Makefile](Makefile). `make test-envtest` also runs without an external cluster; it needs API server and etcd assets selected by `setup-envtest`. The [CI workflow](.github/workflows/ci.yaml) runs the fast checks; the separate [E2E workflow](.github/workflows/e2e.yaml) runs the Internal and External provider claim lifecycles in Kind with cert-manager present and absent. Run the cluster-backed integration suite yourself when your change touches that surface.

Cluster scenarios live in `test/e2e/`, with shared functions in `helpers_test.go`. E2E files do not use build tags: normal build and lint checks compile them, while the unit-test targets exclude their package. Use `make test-e2e` to set up and run the suite. Raw `go test ./...` includes the e2e, integration, and envtest suites and requires their environments.

To exercise the packaged runner locally, build it with `make e2e-image-build`, then run:

```sh
make e2e-image-run E2E_CONTEXT=<test-cluster-context> IMG=<cluster-pullable-operator-image>
```

This runs image setup, the operator smoke test, and teardown, including after a test or setup failure. It saves `e2e-results/junit.xml` and `e2e-results/log.jsonl` under `bin/e2e-artifacts/`; override `E2E_ARTIFACTS` to change the directory. Set `E2E_IMG` to use an already published runner image, or `CONTAINER_TOOL=docker` to use Docker. `IMG` identifies the operator image installed in the cluster, separately from the runner image. Override `E2E_TEST_PATTERN` to select other tests after supplying their cluster prerequisites.

Use an exclusive disposable cluster with no existing `DatabaseService/default-db-operator` or E2E namespace. The runner installs cert-manager if missing; cert-manager and CRDs remain after teardown. The target reads the normal `KUBECONFIG` configuration and exports only the selected context with embedded certificates to a temporary file, which it removes afterwards. Credentials must use a token or client certificate rather than a host authentication plugin. The API endpoint must be reachable from the container runtime; on macOS, a remote cluster endpoint works, while a Kind endpoint at `127.0.0.1` needs additional networking configuration for the runtime VM.

`make test-integration` applies CRDs to the kubeconfig cluster only when its current context matches `TEST_INTEGRATION_CONTEXT` (default: the configured Kind cluster). Set that variable explicitly to use another test cluster, such as OpenShift. The suite requires exclusive use of the cluster-scoped `DatabaseService/default-db-operator` and will not reuse or delete an existing instance.

`make test-e2e-providers` runs the Internal and External provider claim lifecycles on an existing, isolated Kind cluster. The current context must match `kind-$(KIND_CLUSTER)`. Set `E2E_CERT_MANAGER_PROFILE` to `present` or `absent` to match the cluster: the present profile requires cert-manager, and the absent profile requires it to be missing. To create a disposable External PostgreSQL instance and its administrator Secret, run `bash test/e2e/scripts/setup-external-postgres.sh <kind-cluster-name>`, then run `make test-e2e-providers KIND_CLUSTER=<kind-cluster-name> E2E_CERT_MANAGER_PROFILE=present E2E_EXTERNAL_ADMIN_SECRET_NAMESPACE=external-postgres E2E_EXTERNAL_ADMIN_SECRET_NAME=external-postgres-admin`. Use `E2E_CERT_MANAGER_PROFILE=absent` on a cluster without cert-manager for the other profile. The [E2E workflow](.github/workflows/e2e.yaml) shows the full setup.
