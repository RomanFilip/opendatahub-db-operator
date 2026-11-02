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

Run the narrowest suite that exercises your change. Run these commands from the repository root;
the [Makefile](Makefile) defines their setup steps and defaults. E2E sources have no build tags:
unit-test targets exclude `test/e2e`, while `go test ./...` also runs the e2e, integration, and
envtest packages and needs their environments.

| Suite | Command | What it checks | Environment |
| --- | --- | --- | --- |
| Unit | `make test` | Go packages outside `test/integration`, `test/envtest`, and `test/e2e`. | No cluster. |
| Envtest | `make test-envtest` | Reconciliation against a local Kubernetes API server and etcd. | No external cluster; requires `controller-gen`, `setup-envtest`, and its selected API server/etcd assets. |
| Integration | `make test-integration` | Operator behavior against the current kubeconfig cluster. | Dedicated test cluster and matching `TEST_INTEGRATION_CONTEXT`. |
| End to end | `make test-e2e-providers` | Internal and External provider claim lifecycles on Kind. | Existing isolated Kind cluster, Kind, Helm, and Podman CLIs; the cert-manager profile and External provider administrator Secret described below. |

The [CI workflow](.github/workflows/ci.yaml) runs unit and envtest suites. The separate
[E2E workflow](.github/workflows/e2e.yaml) runs both cert-manager profiles.

The e2e suite includes a default-deny baseline scenario
(`TestOperatorRunsUnderDefaultDenyBaseline`) that isolates the operator
namespace with a deny-all NetworkPolicy and verifies the operator keeps
reaching the API server and PostgreSQL. Its chart-shape assertions run on
every cluster; the deny-all phase requires a CNI that enforces NetworkPolicy
(OpenShift/OVN-K, or Calico/Cilium on Kind) and is gated by a runtime
enforcement probe rather than a flag, so it runs automatically wherever it
can be meaningful and skips with a documented reason on clusters whose CNI
ignores policies (Kind's default kindnet). It is a required validation for
changes touching the operator NetworkPolicy or chart networking. Run it
directly against any cluster whose CNI enforces NetworkPolicy
(OpenShift/OVN-K, or Kind with Calico/Cilium) with the Helm-installed
operator -- the `make test-e2e-providers` target is bound to a
`kind-<cluster>` context and cannot express this:

```sh
ODH_E2E_NAMESPACE=<operator-namespace> \
ODH_E2E_CONSUMER_IMAGE=<bash-and-psql image, e.g. postgres:16> \
go test ./test/e2e/ -run TestOperatorRunsUnderDefaultDenyBaseline -v -timeout 25m
```

The E2E workflow's `Default-deny baseline (Calico)` job runs this scenario
continuously on every pull request, on a Kind cluster whose default CNI is
replaced with Calico so NetworkPolicy is enforced.

Run the integration suite yourself when a change touches its cluster boundary.

Before a cluster-backed suite, check `kubectl config current-context`. `make test-integration`
applies CRDs only when the current context matches `TEST_INTEGRATION_CONTEXT` (by default,
`kind-$(KIND_CLUSTER)`). Set that variable explicitly for another test cluster, such as OpenShift.
The suite needs exclusive use of the cluster-scoped `DatabaseService/default-db-operator` and will
not reuse or delete an existing instance.

`make test-e2e-providers` requires the context `kind-$(KIND_CLUSTER)` and does not create the Kind
cluster. Set `E2E_CERT_MANAGER_PROFILE=present` for a cluster with cert-manager, or `absent` for a
cluster without it. Set up a disposable External PostgreSQL instance and its administrator Secret,
then run the suite, for example:

```sh
bash test/e2e/scripts/setup-external-postgres.sh db-operator-dev
make test-e2e-providers KIND_CLUSTER=db-operator-dev E2E_CERT_MANAGER_PROFILE=present \
  E2E_EXTERNAL_ADMIN_SECRET_NAMESPACE=external-postgres \
  E2E_EXTERNAL_ADMIN_SECRET_NAME=external-postgres-admin
```

Use the `absent` profile on a cluster without cert-manager. The [E2E workflow](.github/workflows/e2e.yaml)
shows setup for both profiles. E2E setup builds and loads an image with Podman; teardown removes the
configured e2e namespace and `DatabaseService/default-db-operator`, so use an isolated cluster.
The `external-postgres` namespace created by the setup script remains after the suite. Uncached
`Containerfile` base images may require registry access.

To exercise the packaged runner, build it with `make e2e-image-build`, then run:

```sh
make e2e-image-run E2E_CONTEXT=<test-cluster-context> IMG=<cluster-pullable-operator-image>
```

This runs image setup, the operator smoke test, and teardown, including after a test or setup
failure. It writes `e2e-results/junit.xml` and `e2e-results/log.jsonl` under `bin/e2e-artifacts/`;
set `E2E_ARTIFACTS` to change that directory. `IMG` is the operator image installed in the cluster.
Set `E2E_IMG` to use an already published runner image, or `CONTAINER_TOOL=docker` to use Docker.
Set `E2E_TEST_PATTERN` to select other tests after their cluster prerequisites are in place.

Use an exclusive disposable cluster with no existing `DatabaseService/default-db-operator` or
configured e2e namespace. Setup installs cert-manager when it is missing; teardown leaves
cert-manager and CRDs in place. The target exports only the selected context, with embedded
certificates, to a temporary kubeconfig and removes that file afterwards. Credentials must be a
token or client certificate; a host authentication plugin is not available inside the image. The
API endpoint must be reachable from the container runtime. On macOS, a remote cluster endpoint
works; a Kind API at `127.0.0.1` needs extra networking so the runtime VM can reach it.
