/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package providernetworkpolicy runs in a separate test process because
// controller-runtime rejects duplicate controller names across managers in
// one process. Only the DatabaseProvider reconciler is registered so the
// claim controllers cannot overwrite the synthetic Provisioned status.
package providernetworkpolicy

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	"github.com/opendatahub-io/opendatahub-db-operator/internal/controller/databaseprovider"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	modulemanager "github.com/opendatahub-io/opendatahub-db-operator/pkg/manager"
	"github.com/opendatahub-io/opendatahub-db-operator/test/support"
)

// unrelatedClaimWindow bounds the Consistently assertion that an unrelated
// provider stays untouched: the periodic requeue is slower than this window,
// so only an erroneous watch enqueue can violate it.
const unrelatedClaimWindow = 10 * time.Second

// TestClaimWatchesDriveProviderAdmission verifies claim watches refresh
// Internal NetworkPolicy ingress on claim status changes without waiting for
// the periodic retry. Subtests share one manager with distinct resource names.
func TestClaimWatchesDriveProviderAdmission(t *testing.T) {
	ctrl.SetLogger(logr.Discard())

	gomegaCfg := support.LoadGomegaConfig()
	SetDefaultEventuallyTimeout(gomegaCfg.EventuallyTimeout)
	SetDefaultEventuallyPollingInterval(gomegaCfg.EventuallyPollingInterval)
	SetDefaultConsistentlyPollingInterval(gomegaCfg.ConsistentlyPollingInterval)
	g := NewWithT(t)

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	restCfg, err := env.Start()
	g.Expect(err).NotTo(HaveOccurred(),
		"starting envtest -- KUBEBUILDER_ASSETS must point at a directory with etcd/kube-apiserver "+
			"(see `setup-envtest use -p path`); `make test-envtest` sets this up automatically")
	t.Cleanup(func() {
		g.Expect(env.Stop()).To(Succeed())
	})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	cfg, err := moduleconfig.Load()
	g.Expect(err).NotTo(HaveOccurred())
	// Keep periodic requeue longer than Eventually so a pass proves the watch fired.
	cfg.DatabaseProvider.RetryInterval = gomegaCfg.EventuallyTimeout * 2

	scheme, err := modulemanager.NewScheme()
	g.Expect(err).NotTo(HaveOccurred())

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: "0",
		},
		HealthProbeBindAddress: "0",
		LeaderElection:         false,
	})
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(databaseprovider.NewReconciler(ctx, mgr, cfg, databaseprovider.Options{
		Recorder: mgr.GetEventRecorder(infraApi.DatabaseProviderResource),
	})).To(Succeed())

	managerDone := make(chan struct{})
	var managerErr error
	go func() {
		managerErr = mgr.Start(ctx)
		close(managerDone)
	}()
	t.Cleanup(func() {
		cancel()
		<-managerDone
		if managerErr != nil {
			t.Errorf("manager failed while stopping: %v", managerErr)
		}
	})
	g.Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue())

	cli := mgr.GetClient()

	ensureNamespace := func(g Gomega, name string) {
		err := cli.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
		if err != nil {
			g.Expect(apierrors.IsAlreadyExists(err)).To(BeTrue(), "creating namespace %s: %v", name, err)
		}
	}

	// expectPolicyAdmits asserts the exact ingress peer set of the provider's
	// NetworkPolicy: the operator pod plus one namespace-selector peer whose
	// values are exactly the given namespaces.
	expectPolicyAdmits := func(g Gomega, key client.ObjectKey, namespaces ...string) {
		policy := &networkingv1.NetworkPolicy{}
		g.Expect(cli.Get(ctx, key, policy)).To(Succeed())
		g.Expect(policy.Spec.Ingress).To(HaveLen(1))
		from := policy.Spec.Ingress[0].From
		g.Expect(from).To(ContainElement(networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"kubernetes.io/metadata.name": cfg.OperatorNamespace,
			}},
			PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				"app.kubernetes.io/name": "opendatahub-db-operator",
			}},
		}))
		if len(namespaces) == 0 {
			g.Expect(from).To(HaveLen(1))
			return
		}
		sorted := slices.Clone(namespaces)
		slices.Sort(sorted)
		g.Expect(from).To(HaveLen(2))
		g.Expect(from).To(ContainElement(networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      "kubernetes.io/metadata.name",
				Operator: metav1.LabelSelectorOpIn,
				Values:   sorted,
			}}},
		}))
	}

	// newInternalProvider creates an Internal provider and waits until its
	// instance and NetworkPolicy render with operator-only admission.
	newInternalProvider := func(t *testing.T, name, namespace string) client.ObjectKey {
		g := NewWithT(t)
		ensureNamespace(g, namespace)
		provider := &infraApi.DatabaseProvider{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: infraApi.DatabaseProviderSpec{
				Type: infraApi.ProviderTypeInternal,
				Internal: &infraApi.InternalProviderSpec{
					Namespace: namespace,
					Storage:   infraApi.StorageSpec{Size: resource.MustParse("1Gi")},
				},
			},
		}
		g.Expect(cli.Create(ctx, provider)).To(Succeed())
		key := client.ObjectKey{Namespace: namespace, Name: name}
		g.Eventually(func(g Gomega) {
			sts := &appsv1.StatefulSet{}
			g.Expect(cli.Get(ctx, key, sts)).To(Succeed())
			expectPolicyAdmits(g, key)
		}).Should(Succeed())
		return key
	}

	provisionedCondition := func(provisioned bool) []fwapi.Condition {
		conditionStatus, reason := metav1.ConditionFalse, "UserDeprovisioned"
		if provisioned {
			conditionStatus, reason = metav1.ConditionTrue, "UserProvisioned"
		}
		return []fwapi.Condition{{
			Type:               dbcontroller.ConditionProvisioned,
			Status:             conditionStatus,
			Reason:             reason,
			LastTransitionTime: metav1.Now(),
		}}
	}

	setDatabaseClaimStatus := func(g Gomega, key client.ObjectKey, provider string, provisioned bool) {
		g.Eventually(func(g Gomega) {
			current := &infraApi.DatabaseClaim{}
			g.Expect(cli.Get(ctx, key, current)).To(Succeed())
			current.Status.Provider = provider
			current.Status.Conditions = provisionedCondition(provisioned)
			g.Expect(cli.Status().Update(ctx, current)).To(Succeed())
		}).Should(Succeed())
	}

	setSchemaClaimStatus := func(g Gomega, key client.ObjectKey, provider string, provisioned bool) {
		g.Eventually(func(g Gomega) {
			current := &infraApi.SchemaClaim{}
			g.Expect(cli.Get(ctx, key, current)).To(Succeed())
			current.Status.Provider = provider
			current.Status.Conditions = provisionedCondition(provisioned)
			g.Expect(cli.Status().Update(ctx, current)).To(Succeed())
		}).Should(Succeed())
	}

	selectorRef := infraApi.ProviderRef{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"db.infrastructure.opendatahub.io/tier": "shared"}},
	}

	t.Run("database claim bind admits the claim namespace", func(t *testing.T) {
		g := NewWithT(t)
		providerKey := newInternalProvider(t, "internal-bind", "database-system")
		ensureNamespace(g, "bind-workload")
		claim := &infraApi.DatabaseClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "analytics", Namespace: "bind-workload"},
			Spec: infraApi.DatabaseClaimSpec{
				Provider: infraApi.ProviderRef{Name: providerKey.Name},
				Database: "analytics",
			},
		}
		g.Expect(cli.Create(ctx, claim)).To(Succeed())
		setDatabaseClaimStatus(g, client.ObjectKeyFromObject(claim), providerKey.Name, true)

		g.Eventually(func(g Gomega) {
			expectPolicyAdmits(g, providerKey, "bind-workload")
		}).Should(Succeed())
	})

	t.Run("schema claim bind admits the claim namespace", func(t *testing.T) {
		g := NewWithT(t)
		providerKey := newInternalProvider(t, "internal-schema", "database-system")
		ensureNamespace(g, "schema-workload")
		claim := &infraApi.SchemaClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "feature-store", Namespace: "schema-workload"},
			Spec:       infraApi.SchemaClaimSpec{Provider: infraApi.ProviderRef{Name: providerKey.Name}},
		}
		g.Expect(cli.Create(ctx, claim)).To(Succeed())
		setSchemaClaimStatus(g, client.ObjectKeyFromObject(claim), providerKey.Name, true)

		g.Eventually(func(g Gomega) {
			expectPolicyAdmits(g, providerKey, "schema-workload")
		}).Should(Succeed())
	})

	t.Run("ceasing one claim keeps admission while another holds the namespace", func(t *testing.T) {
		g := NewWithT(t)
		providerKey := newInternalProvider(t, "internal-cease", "database-system")
		ensureNamespace(g, "cease-workload")
		holder := &infraApi.DatabaseClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "holder", Namespace: "cease-workload"},
			Spec:       infraApi.DatabaseClaimSpec{Provider: infraApi.ProviderRef{Name: providerKey.Name}},
		}
		leaver := &infraApi.DatabaseClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "leaver", Namespace: "cease-workload"},
			Spec:       infraApi.DatabaseClaimSpec{Provider: infraApi.ProviderRef{Name: providerKey.Name}},
		}
		g.Expect(cli.Create(ctx, holder)).To(Succeed())
		g.Expect(cli.Create(ctx, leaver)).To(Succeed())
		setDatabaseClaimStatus(g, client.ObjectKeyFromObject(holder), providerKey.Name, true)
		setDatabaseClaimStatus(g, client.ObjectKeyFromObject(leaver), providerKey.Name, true)

		g.Eventually(func(g Gomega) {
			expectPolicyAdmits(g, providerKey, "cease-workload")
		}).Should(Succeed())

		setDatabaseClaimStatus(g, client.ObjectKeyFromObject(leaver), providerKey.Name, false)
		g.Consistently(func(g Gomega) {
			expectPolicyAdmits(g, providerKey, "cease-workload")
		}).WithTimeout(unrelatedClaimWindow).Should(Succeed())

		setDatabaseClaimStatus(g, client.ObjectKeyFromObject(holder), providerKey.Name, false)
		g.Eventually(func(g Gomega) {
			expectPolicyAdmits(g, providerKey)
		}).Should(Succeed())
	})

	t.Run("deleting the last claim removes admission", func(t *testing.T) {
		g := NewWithT(t)
		providerKey := newInternalProvider(t, "internal-delete", "database-system")
		ensureNamespace(g, "delete-workload")
		claim := &infraApi.DatabaseClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "short-lived", Namespace: "delete-workload"},
			Spec:       infraApi.DatabaseClaimSpec{Provider: infraApi.ProviderRef{Name: providerKey.Name}},
		}
		g.Expect(cli.Create(ctx, claim)).To(Succeed())
		setDatabaseClaimStatus(g, client.ObjectKeyFromObject(claim), providerKey.Name, true)

		g.Eventually(func(g Gomega) {
			expectPolicyAdmits(g, providerKey, "delete-workload")
		}).Should(Succeed())

		g.Expect(cli.Delete(ctx, claim)).To(Succeed())
		g.Eventually(func(g Gomega) {
			expectPolicyAdmits(g, providerKey)
		}).Should(Succeed())
	})

	t.Run("selector claim rebind refreshes both providers", func(t *testing.T) {
		g := NewWithT(t)
		keyA := newInternalProvider(t, "internal-rebind-a", "database-system")
		keyB := newInternalProvider(t, "internal-rebind-b", "database-system")
		ensureNamespace(g, "rebind-workload")
		claim := &infraApi.DatabaseClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "rebinding", Namespace: "rebind-workload"},
			Spec:       infraApi.DatabaseClaimSpec{Provider: selectorRef},
		}
		g.Expect(cli.Create(ctx, claim)).To(Succeed())
		setDatabaseClaimStatus(g, client.ObjectKeyFromObject(claim), keyA.Name, true)

		g.Eventually(func(g Gomega) {
			expectPolicyAdmits(g, keyA, "rebind-workload")
		}).Should(Succeed())

		setDatabaseClaimStatus(g, client.ObjectKeyFromObject(claim), keyB.Name, true)
		g.Eventually(func(g Gomega) {
			expectPolicyAdmits(g, keyB, "rebind-workload")
		}).Should(Succeed())
		g.Eventually(func(g Gomega) {
			expectPolicyAdmits(g, keyA)
		}).Should(Succeed())
	})

	t.Run("selector claim unbind removes admission", func(t *testing.T) {
		g := NewWithT(t)
		providerKey := newInternalProvider(t, "internal-unbind", "database-system")
		ensureNamespace(g, "unbind-workload")
		claim := &infraApi.DatabaseClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "unbound", Namespace: "unbind-workload"},
			Spec:       infraApi.DatabaseClaimSpec{Provider: selectorRef},
		}
		g.Expect(cli.Create(ctx, claim)).To(Succeed())
		setDatabaseClaimStatus(g, client.ObjectKeyFromObject(claim), providerKey.Name, true)

		g.Eventually(func(g Gomega) {
			expectPolicyAdmits(g, providerKey, "unbind-workload")
		}).Should(Succeed())

		setDatabaseClaimStatus(g, client.ObjectKeyFromObject(claim), "", false)
		g.Eventually(func(g Gomega) {
			expectPolicyAdmits(g, providerKey)
		}).Should(Succeed())
	})

	t.Run("unrelated claim leaves admission unchanged", func(t *testing.T) {
		g := NewWithT(t)
		watched := newInternalProvider(t, "internal-negative", "database-system")
		other := newInternalProvider(t, "internal-other", "database-system")
		ensureNamespace(g, "negative-workload")
		claim := &infraApi.DatabaseClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "elsewhere", Namespace: "negative-workload"},
			Spec:       infraApi.DatabaseClaimSpec{Provider: infraApi.ProviderRef{Name: other.Name}},
		}
		g.Expect(cli.Create(ctx, claim)).To(Succeed())
		setDatabaseClaimStatus(g, client.ObjectKeyFromObject(claim), other.Name, true)

		// The referenced provider admitting the namespace proves the claim
		// transitions actually reached the reconciler.
		g.Eventually(func(g Gomega) {
			expectPolicyAdmits(g, other, "negative-workload")
		}).Should(Succeed())
		g.Consistently(func(g Gomega) {
			expectPolicyAdmits(g, watched)
		}).WithTimeout(unrelatedClaimWindow).Should(Succeed())
	})
}
