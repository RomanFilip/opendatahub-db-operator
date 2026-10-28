---
description: Install and operate the Open Data Hub database operator.
audience: [human]
load: When installing, configuring, or operating the database service
---

# Installation and usage

The database operator runs as an OpenShift AI module and provides PostgreSQL providers and
namespaced database claims.

## Prerequisites

- An OpenShift cluster with the OpenShift AI Operator release that includes the `databaseservice` module.
- An OpenShift version supported by that OpenShift AI release; use its version-specific installation guide.
- Cluster administrator access to enable the module or install the chart and its CRDs.
- A working storage class for the default Internal PostgreSQL provider.
- Helm when installing the standalone chart.
- cert-manager installed on the cluster when installing the standalone chart.

Follow the [installation instructions for your OpenShift AI release](https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed/).
The database module's `DataScienceCluster` setting is available in operator releases that include
the [database service component](https://github.com/opendatahub-io/opendatahub-operator/blob/main/api/components/v1alpha1/databaseservice_types.go).

## Enable the module

After installing the OpenShift AI Operator, set the database module to `Managed` on the
`DataScienceCluster`:

```sh
oc patch datasciencecluster default-dsc --type=merge \
  -p '{"spec":{"components":{"databaseservice":{"managementState":"Managed"}}}}'
oc wait --for=condition=Ready databaseservice/default-db-operator --timeout=5m
```

For standalone evaluation or development, install the infrastructure CRDs before the chart.
The chart packages the `DatabaseService` CRD, but not the infrastructure CRDs. The operator
registers the provider and database claim controllers when it starts and finds their CRDs; the
provider controller also watches `SchemaClaim` resources:

```sh
kubectl apply -f config/crd/bases/infrastructure.opendatahub.io_databaseproviders.yaml
kubectl apply -f config/crd/bases/infrastructure.opendatahub.io_databaseclaims.yaml
kubectl apply -f config/crd/bases/infrastructure.opendatahub.io_schemaclaims.yaml
kubectl wait --for=condition=Established \
  crd/databaseproviders.infrastructure.opendatahub.io \
  crd/databaseclaims.infrastructure.opendatahub.io \
  crd/schemaclaims.infrastructure.opendatahub.io --timeout=2m
```

The chart does not set a namespace in `values.yaml`; install it in `odh-db-operator-system`,
the namespace used by the Kustomize bundle, or choose another namespace:

```sh
helm install opendatahub-db-operator config/chart --create-namespace --namespace odh-db-operator-system
```

Apply the `DatabaseService` singleton and wait for it to become Ready:

```sh
kubectl apply -f - <<'EOF'
apiVersion: services.platform.opendatahub.io/v1alpha1
kind: DatabaseService
metadata:
  name: default-db-operator
EOF
kubectl wait --for=condition=Ready databaseservice/default-db-operator --timeout=5m
```

Helm installs the `DatabaseService` CRD on first install but skips CRD changes on upgrade. Apply an
updated schema separately with `kubectl apply -f config/chart/crds/` alongside `helm upgrade`.

## Configure a provider

`DatabaseService` creates an Internal `DatabaseProvider` named `rhai-db` and its single-replica
PostgreSQL workload in the operator namespace. The workload uses the cluster's default storage
class and requests 20Gi. Set `config.default-provider-name` during Helm installation to choose
another name; changing it after the service has initialized is rejected. Check
`oc get databaseproviders` and the service's `DefaultProviderReady` condition while the
workload starts.

Create additional `DatabaseProvider` resources when a claim needs a different backend. This
example provisions an Internal PostgreSQL instance using the cluster's default storage class;
set `storageClassName` when a different class is required:

```yaml
apiVersion: infrastructure.opendatahub.io/v1alpha1
kind: DatabaseProvider
metadata:
  name: app-postgres
spec:
  type: Internal
  internal:
    storage:
      size: 20Gi
```

Apply the provider manifest with `oc apply -f databaseprovider.yaml`.

Internal providers are single-instance PostgreSQL services. On OpenShift, the Helm chart
installs a shared SecurityContextConstraints and use ClusterRole limited to UID/GID 26;
the operator creates a dedicated ServiceAccount per provider and binds it to that SCC.
The SCC template is included when the cluster advertises `security.openshift.io/v1`
(standalone `helm install`/`upgrade`) or when
`openshift.securityContextConstraints=true` (required for the ODH operator's offline
chart render, which does not populate Helm Capabilities). On Kind or vanilla Kubernetes
the SCC is omitted. For a Kustomize OpenShift deploy, also apply `config/openshift`.

For an administrator-managed database, configure an External provider with a connection Secret
instead. See the [provider API](../api/infrastructure/v1alpha1/databaseprovider_types.go)
for the supported fields.

## Request a database

Create a namespaced `DatabaseClaim` for a database on the default provider. The provider must
already contain the requested database or permit the claim to create it:

```yaml
apiVersion: infrastructure.opendatahub.io/v1alpha1
kind: DatabaseClaim
metadata:
  name: app-db
  namespace: my-app
spec:
  database: app
  access: ReadWrite
```

Apply the claim and check its `Provisioned` condition. The operator writes connection credentials to
a Secret in the claim's namespace; by default, the Secret has the same name as the claim:

```sh
oc apply -f databaseclaim.yaml
oc get databaseclaim app-db -n my-app -o yaml
oc get secret app-db -n my-app
```

The Secret contains discrete PostgreSQL connection fields. See the [credential contract](../ARCHITECTURE.md#credential-contract)
for field names and TLS behavior.

To use another provider, set `spec.provider.name` or `spec.provider.selector` on the claim.
The same selection applies to `SchemaClaim`. Remove claims using the default provider before
deleting `DatabaseService`; deletion reports `BlockingClaims` while any bound claim remains.
The internal PVC and admin Secret remain after removal so the data can be recovered or deleted
explicitly by an administrator.

## Status and limitations

Check provider and claim conditions with `oc get databaseproviders` and
`oc get databaseclaims -A`. The claim's `Provisioned` condition reports whether credentials are
ready for the application.

The Internal provider is a single PostgreSQL instance without high availability or automated backup
and restore. External providers remain the administrator's responsibility. Claims retain database
data when deleted by default, and the operator does not provide automated credential rotation. See
[architecture](../ARCHITECTURE.md#provider-backends) for the operational boundaries.

## Support and contributing

- Report bugs and request help in [GitHub Issues](https://github.com/opendatahub-io/opendatahub-db-operator/issues).
- Contact the maintainers at [managed-open-data-hub@redhat.com](mailto:managed-open-data-hub@redhat.com).
- See [CONTRIBUTING.md](../CONTRIBUTING.md) for development workflows.
