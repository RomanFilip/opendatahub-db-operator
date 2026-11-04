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

package databaseprovider

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/gomega"
	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	odherrors "github.com/opendatahub-io/odh-platform-utilities/framework/controller/actions/errors"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	odhtypes "github.com/opendatahub-io/odh-platform-utilities/framework/controller/types"
	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
	pginstance "github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres/instance"
	secretvalue "github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
	configv1 "github.com/openshift/api/config/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const internalStatefulSetKind = "StatefulSet"

func TestReconcileExternalActionReportsUnavailableSecret(t *testing.T) {
	g := NewWithT(t)
	provider := externalProvider()
	provider.Status.ServerVersion = "16.4"
	cli := testClient(t)
	rr := testRequest(t, cli, provider)

	err := (&Controller{}).reconcileExternalAction(context.Background(), rr)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(provider.Status.ServerVersion).To(BeEmpty())
	reachable := conditions.FindStatusCondition(provider, ConditionReachable)
	g.Expect(reachable).NotTo(BeNil())
	g.Expect(reachable.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(reachable.Reason).To(Equal("ConnectionSecretUnavailable"))
	g.Expect(reachable.Message).To(ContainSubstring("external-admin"))
	g.Expect(reachable.Message).To(ContainSubstring("Verify"))
	ready := conditions.FindStatusCondition(provider, string(fwapi.ConditionTypeReady))
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).NotTo(Equal(metav1.ConditionTrue))
	g.Expect(conditions.FindStatusCondition(provider, infraApi.ConditionOperationalLimitations)).To(BeNil())
}

func TestReconcileInternalActionReportsOperationalLimitationsBeforeSetup(t *testing.T) {
	g := NewWithT(t)
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "internal"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeInternal,
			Internal: &infraApi.InternalProviderSpec{
				TLS: &infraApi.InternalProviderTLSSpec{},
			},
		},
	}
	rr := testRequest(t, testClient(t), provider)
	m := &Controller{Options: Options{cfg: &moduleconfig.Config{}}}

	g.Expect(m.reconcileInternalAction(context.Background(), rr)).To(Succeed())
	limit := conditions.FindStatusCondition(provider, infraApi.ConditionOperationalLimitations)
	g.Expect(limit).NotTo(BeNil())
	g.Expect(limit.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(limit.Severity).To(Equal(fwapi.ConditionSeverityInfo))
	g.Expect(limit.Reason).To(Equal("SingleInstanceWithoutBackup"))
	g.Expect(limit.Message).To(Equal("The Internal provider is a single PostgreSQL instance without high availability or automated backup and restore. Use an appropriately managed External provider when those capabilities are required."))
	g.Expect(conditions.FindStatusCondition(provider, string(fwapi.ConditionTypeReady)).Status).NotTo(Equal(metav1.ConditionTrue))

	transition := limit.LastTransitionTime
	g.Expect(m.reconcileInternalAction(context.Background(), rr)).To(Succeed())
	g.Expect(conditions.FindStatusCondition(provider, infraApi.ConditionOperationalLimitations).LastTransitionTime).To(Equal(transition))
}

func TestInternalOperationalLimitationsDoNotBlockReadiness(t *testing.T) {
	g := NewWithT(t)
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "internal"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeInternal,
			Internal: &infraApi.InternalProviderSpec{
				Storage: infraApi.StorageSpec{Size: resource.MustParse("1Gi")},
			},
		},
	}
	rr := testRequest(t, testClient(t), provider)
	m := &Controller{Options: Options{cfg: &moduleconfig.Config{PostgresImage: moduleconfig.DefaultInternalPostgresImage}}}

	g.Expect(m.reconcileInternalAction(context.Background(), rr)).To(Succeed())
	g.Expect(conditions.FindStatusCondition(provider, infraApi.ConditionOperationalLimitations).Status).To(Equal(metav1.ConditionTrue))
	markReachable(rr, provider, metav1.ConditionTrue, conditions.WithReason(reasonInstanceRunning))
	g.Expect(conditions.FindStatusCondition(provider, string(fwapi.ConditionTypeReady)).Status).To(Equal(metav1.ConditionTrue))
}

func TestReconcileExternalActionReportsInvalidSecretWithoutReturningError(t *testing.T) {
	g := NewWithT(t)
	provider := externalProvider()
	provider.Status.ServerVersion = "16.4"
	secret := externalAdminSecret()
	secret.Data[postgres.SecretKeyPort] = []byte("not-a-port")
	cli := testClient(t, secret)
	rr := testRequest(t, cli, provider)

	g.Expect((&Controller{}).reconcileExternalAction(context.Background(), rr)).To(Succeed())
	g.Expect(provider.Status.ServerVersion).To(BeEmpty())
	reachable := conditions.FindStatusCondition(provider, ConditionReachable)
	g.Expect(reachable).NotTo(BeNil())
	g.Expect(reachable.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(reachable.Reason).To(Equal("ConnectionSecretInvalid"))
	g.Expect(reachable.Message).To(ContainSubstring("Correct the admin Secret"))
	g.Expect(reachable.Message).NotTo(ContainSubstring("secret"))
	tlsCondition := conditions.FindStatusCondition(provider, ConditionTLSConfiguration)
	g.Expect(tlsCondition).NotTo(BeNil())
	g.Expect(tlsCondition.Status).To(Equal(metav1.ConditionUnknown))
	ready := conditions.FindStatusCondition(provider, string(fwapi.ConditionTypeReady))
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).NotTo(Equal(metav1.ConditionTrue))
}

func TestReconcileExternalActionFollowsChangedTLSProfileWithoutCertManager(t *testing.T) {
	g := NewWithT(t)
	cli := testClient(t,
		externalAdminSecret(),
		&configv1.APIServer{
			ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
			Spec: configv1.APIServerSpec{TLSSecurityProfile: &configv1.TLSSecurityProfile{
				Type: configv1.TLSProfileIntermediateType,
			}},
		},
	)
	var minimumVersions []uint16
	var cipherSuites [][]uint16
	factory := func(_ context.Context, cfg postgres.Config) (postgres.Client, error) {
		tlsConfig := &tls.Config{}
		cfg.TLSConfigMutator(tlsConfig)
		minimumVersions = append(minimumVersions, tlsConfig.MinVersion)
		cipherSuites = append(cipherSuites, append([]uint16(nil), tlsConfig.CipherSuites...))
		return &fakePostgresClient{version: 160004}, nil
	}
	m := &Controller{Options: Options{PostgresClientFactory: factory}}
	provider := externalProvider()

	profiles := []*configv1.TLSSecurityProfile{
		{Type: configv1.TLSProfileIntermediateType},
		{
			Type: configv1.TLSProfileCustomType,
			Custom: &configv1.CustomTLSProfile{TLSProfileSpec: configv1.TLSProfileSpec{
				MinTLSVersion: configv1.VersionTLS12,
				Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256"},
			}},
		},
	}
	for index, profile := range profiles {
		if index > 0 {
			apiServer := &configv1.APIServer{}
			g.Expect(cli.Get(context.Background(), client.ObjectKey{Name: "cluster"}, apiServer)).To(Succeed())
			apiServer.Spec.TLSSecurityProfile = profile
			g.Expect(cli.Update(context.Background(), apiServer)).To(Succeed())
		}
		rr := testRequest(t, cli, provider)
		g.Expect(m.reconcileExternalAction(context.Background(), rr)).To(Succeed())
	}

	g.Expect(minimumVersions).To(Equal([]uint16{tls.VersionTLS12, tls.VersionTLS12}))
	g.Expect(cipherSuites[0]).NotTo(Equal(cipherSuites[1]))
	g.Expect(cipherSuites[1]).To(Equal([]uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256}))
	g.Expect(provider.Status.ServerVersion).To(Equal("16.4"))
	g.Expect(provider.Status.Connection).To(Equal(infraApi.ProviderConnectionStatus{
		Host:     "postgres.example.test",
		Port:     5432,
		Database: "postgres",
	}))
	g.Expect(provider.Status.TLS).To(Equal(&infraApi.ProviderTLSStatus{Enabled: true, Ready: true}))
	reachable := conditions.FindStatusCondition(provider, ConditionReachable)
	g.Expect(reachable.Status).To(Equal(metav1.ConditionTrue))
	version := conditions.FindStatusCondition(provider, ConditionServerVersion)
	g.Expect(version.Status).To(Equal(metav1.ConditionTrue))
	tlsCondition := conditions.FindStatusCondition(provider, ConditionTLSConfiguration)
	g.Expect(tlsCondition.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(tlsCondition.Reason).To(Equal(reasonTLSConfigured))
}

func TestReconcileExternalActionReportsUnsupportedTLSProfileCiphers(t *testing.T) {
	g := NewWithT(t)
	cli := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			externalAdminSecret(),
			&configv1.APIServer{
				ObjectMeta: metav1.ObjectMeta{Name: "cluster"},
				Spec: configv1.APIServerSpec{TLSSecurityProfile: &configv1.TLSSecurityProfile{
					Type: configv1.TLSProfileCustomType,
					Custom: &configv1.CustomTLSProfile{TLSProfileSpec: configv1.TLSProfileSpec{
						MinTLSVersion: configv1.VersionTLS12,
						Ciphers:       []string{"ECDHE-RSA-AES128-GCM-SHA256", "UNKNOWN-CIPHER-SENTINEL"},
					}},
				}},
			},
		).
		Build()
	m := &Controller{Options: Options{PostgresClientFactory: func(_ context.Context, _ postgres.Config) (postgres.Client, error) {
		return &fakePostgresClient{version: 160004}, nil
	}}}
	provider := externalProvider()
	rr := testRequest(t, cli, provider)

	g.Expect(m.reconcileExternalAction(context.Background(), rr)).To(Succeed())
	tlsCondition := conditions.FindStatusCondition(provider, ConditionTLSConfiguration)
	g.Expect(tlsCondition).NotTo(BeNil())
	g.Expect(tlsCondition.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(tlsCondition.Message).To(ContainSubstring("UNKNOWN-CIPHER-SENTINEL"))
}

func TestReconcileExternalActionReportsUnreachableWithoutReturningError(t *testing.T) {
	g := NewWithT(t)
	provider := externalProvider()
	provider.Status.ServerVersion = "15.8"
	cli := testClient(t, externalAdminSecret())
	clientCalls := 0
	m := &Controller{Options: Options{PostgresClientFactory: func(_ context.Context, _ postgres.Config) (postgres.Client, error) {
		clientCalls++
		return &fakePostgresClient{pingErr: errors.New("network unreachable")}, nil
	}}}
	rr := testRequest(t, cli, provider)

	g.Expect(m.reconcileExternalAction(context.Background(), rr)).To(Succeed())
	g.Expect(clientCalls).To(Equal(1))
	g.Expect(provider.Status.ServerVersion).To(BeEmpty())
	reachable := conditions.FindStatusCondition(provider, ConditionReachable)
	g.Expect(reachable.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(reachable.Message).To(ContainSubstring("network unreachable"))
	g.Expect(reachable.Message).To(ContainSubstring("Verify"))
	version := conditions.FindStatusCondition(provider, ConditionServerVersion)
	g.Expect(version.Status).To(Equal(metav1.ConditionUnknown))
	ready := conditions.FindStatusCondition(provider, string(fwapi.ConditionTypeReady))
	g.Expect(ready.Status).NotTo(Equal(metav1.ConditionTrue))
}

// TestReconcileExternalActionConnectionRefusedRequeuesQuickly covers the retry
// classification this path acts on: a refused connection must still requeue
// quickly once the driver error is projected at the client boundary.
func TestReconcileExternalActionConnectionRefusedRequeuesQuickly(t *testing.T) {
	g := NewWithT(t)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	g.Expect(err).NotTo(HaveOccurred())

	port := listener.Addr().(*net.TCPAddr).Port
	g.Expect(listener.Close()).To(Succeed())

	secret := externalAdminSecret()
	secret.Data[postgres.SecretKeyHost] = []byte("127.0.0.1")
	secret.Data[postgres.SecretKeyPort] = []byte(strconv.Itoa(port))
	secret.Data[postgres.SecretKeySSLMode] = []byte(postgres.SSLModeDisable)

	provider := externalProvider()
	rr := testRequest(t, testClient(t, secret), provider)
	m := &Controller{}

	err = m.reconcileExternalAction(context.Background(), rr)

	var stop odherrors.StopError
	g.Expect(errors.As(err, &stop)).To(BeTrue(), "a refused connection requeues instead of failing reconciliation")
	g.Expect(stop.RequeueAfter()).To(Equal(time.Second))

	reachable := conditions.FindStatusCondition(provider, ConditionReachable)
	g.Expect(reachable.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(reachable.Message).To(ContainSubstring("connection refused"))
	g.Expect(reachable.Message).NotTo(ContainSubstring(string(secret.Data[postgres.SecretKeyPassword])))
}

func TestReconcileExternalActionRedactsPasswordFromConnectionFailure(t *testing.T) {
	g := NewWithT(t)
	secret := externalAdminSecret()
	cli := testClient(t, secret)
	provider := externalProvider()
	m := &Controller{Options: Options{PostgresClientFactory: func(_ context.Context, _ postgres.Config) (postgres.Client, error) {
		return &fakePostgresClient{pingErr: postgres.ProjectError(
			"ping",
			errors.New("authentication failed for password secret"),
			secretvalue.FromBytes(secret.Data[postgres.SecretKeyPassword]),
		)}, nil
	}}}
	rr := testRequest(t, cli, provider)

	g.Expect(m.reconcileExternalAction(context.Background(), rr)).To(Succeed())
	reachable := conditions.FindStatusCondition(provider, ConditionReachable)
	g.Expect(reachable).NotTo(BeNil())
	g.Expect(reachable.Message).To(ContainSubstring("[REDACTED]"))
	g.Expect(reachable.Message).NotTo(ContainSubstring(string(secret.Data[postgres.SecretKeyPassword])))
}

func TestReconcileExternalActionKeepsOutOfRangeServerUsable(t *testing.T) {
	g := NewWithT(t)
	secret := externalAdminSecret()
	secret.Data[postgres.SecretKeySSLMode] = []byte(postgres.SSLModeDisable)
	cli := testClient(t, secret)
	provider := externalProvider()
	m := &Controller{Options: Options{PostgresClientFactory: func(_ context.Context, _ postgres.Config) (postgres.Client, error) {
		return &fakePostgresClient{version: 180000}, nil
	}}}
	rr := testRequest(t, cli, provider)

	g.Expect(m.reconcileExternalAction(context.Background(), rr)).To(Succeed())
	g.Expect(provider.Status.ServerVersion).To(Equal("18.0"))
	reachable := conditions.FindStatusCondition(provider, ConditionReachable)
	g.Expect(reachable.Status).To(Equal(metav1.ConditionTrue))
	version := conditions.FindStatusCondition(provider, ConditionServerVersion)
	g.Expect(version.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(version.Message).To(ContainSubstring("outside the tested range"))
	ready := conditions.FindStatusCondition(provider, string(fwapi.ConditionTypeReady))
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
}

func TestReconcileInternalActionNetworkPolicyAllowsOperatorAndProvisionedClaimNamespaces(t *testing.T) {
	g := NewWithT(t)
	provider := &infraApi.DatabaseProvider{ObjectMeta: metav1.ObjectMeta{Name: "internal"}, Spec: infraApi.DatabaseProviderSpec{
		Type: infraApi.ProviderTypeInternal,
		Internal: &infraApi.InternalProviderSpec{
			Namespace: "database-system", Storage: infraApi.StorageSpec{Size: resource.MustParse("1Gi")},
		},
	}}
	activeSchema := &infraApi.SchemaClaim{ObjectMeta: metav1.ObjectMeta{Name: "schema-active", Namespace: "workload-z"}, Spec: infraApi.SchemaClaimSpec{Provider: infraApi.ProviderRef{Name: "internal"}}}
	activeSchema.Status.Conditions = []fwapi.Condition{{Type: dbcontroller.ConditionProvisioned, Status: metav1.ConditionTrue}}
	activeDatabase := &infraApi.DatabaseClaim{ObjectMeta: metav1.ObjectMeta{Name: "database-active", Namespace: "workload-a"}, Status: infraApi.DatabaseClaimStatus{Provider: "internal"}}
	activeDatabase.Status.Conditions = []fwapi.Condition{{Type: dbcontroller.ConditionProvisioned, Status: metav1.ConditionTrue}}
	duplicateNamespace := &infraApi.DatabaseClaim{ObjectMeta: metav1.ObjectMeta{Name: "database-duplicate", Namespace: "workload-z"}, Spec: infraApi.DatabaseClaimSpec{Provider: infraApi.ProviderRef{Name: "internal"}}}
	duplicateNamespace.Status.Conditions = []fwapi.Condition{{Type: dbcontroller.ConditionProvisioned, Status: metav1.ConditionTrue}}
	unprovisioned := &infraApi.SchemaClaim{ObjectMeta: metav1.ObjectMeta{Name: "schema-pending", Namespace: "workload-pending"}, Spec: infraApi.SchemaClaimSpec{Provider: infraApi.ProviderRef{Name: "internal"}}}
	otherProvider := &infraApi.DatabaseClaim{ObjectMeta: metav1.ObjectMeta{Name: "database-other", Namespace: "workload-other"}, Spec: infraApi.DatabaseClaimSpec{Provider: infraApi.ProviderRef{Name: "another-provider"}}}
	otherProvider.Status.Conditions = []fwapi.Condition{{Type: dbcontroller.ConditionProvisioned, Status: metav1.ConditionTrue}}
	rr := testRequest(t, testClient(t, activeSchema, activeDatabase, duplicateNamespace, unprovisioned, otherProvider), provider)
	m := &Controller{Options: Options{cfg: &moduleconfig.Config{
		OperatorNamespace: "operator-system",
		PostgresImage:     moduleconfig.DefaultInternalPostgresImage,
	}}}

	g.Expect(m.reconcileInternalAction(context.Background(), rr)).To(Succeed())
	var policy *unstructured.Unstructured
	for i := range rr.Resources {
		if rr.Resources[i].GetKind() == "NetworkPolicy" {
			policy = &rr.Resources[i]
			break
		}
	}
	g.Expect(policy).NotTo(BeNil())
	if policy == nil {
		return
	}
	rendered := &networkingv1.NetworkPolicy{}
	g.Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(policy.Object, rendered)).To(Succeed())
	g.Expect(rendered.Spec.Ingress).To(HaveLen(1))
	g.Expect(rendered.Spec.Ingress[0].From).To(ConsistOf(
		networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "operator-system"}}, PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "opendatahub-db-operator"}}},
		networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "kubernetes.io/metadata.name", Operator: metav1.LabelSelectorOpIn, Values: []string{"workload-a", "workload-z"}}}}},
	))
}

func TestReconcileInternalActionDefaultsPostgresResources(t *testing.T) {
	tests := []struct {
		name     string
		provided corev1.ResourceRequirements
		expected corev1.ResourceRequirements
	}{
		{name: "no provider resources", expected: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")}, Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("1Gi")}}},
		{name: "partial provider resources override defaults", provided: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("6Gi")}, Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("750m")}}, expected: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("6Gi")}, Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("750m"), corev1.ResourceMemory: resource.MustParse("1Gi")}}},
		{name: "explicit request above default limit", provided: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3")}}, expected: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("4Gi")}, Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("3"), corev1.ResourceMemory: resource.MustParse("1Gi")}}},
		{name: "explicit limit below default request", provided: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")}}, expected: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("512Mi")}, Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("512Mi")}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			provider := &infraApi.DatabaseProvider{ObjectMeta: metav1.ObjectMeta{Name: "internal"}, Spec: infraApi.DatabaseProviderSpec{Type: infraApi.ProviderTypeInternal, Internal: &infraApi.InternalProviderSpec{Namespace: "database-system", Storage: infraApi.StorageSpec{Size: resource.MustParse("1Gi")}, Resources: *tc.provided.DeepCopy()}}}
			rr := testRequest(t, testClient(t), provider)
			m := &Controller{Options: Options{cfg: &moduleconfig.Config{
				PostgresImage: moduleconfig.DefaultInternalPostgresImage,
			}}}
			g.Expect(m.reconcileInternalAction(context.Background(), rr)).To(Succeed())
			var container *corev1.Container
			for i := range rr.Resources {
				if rr.Resources[i].GetKind() == internalStatefulSetKind {
					statefulSet := &appsv1.StatefulSet{}
					g.Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(rr.Resources[i].Object, statefulSet)).To(Succeed())
					g.Expect(statefulSet.Spec.Replicas).NotTo(BeNil())
					g.Expect(*statefulSet.Spec.Replicas).To(Equal(pginstance.InternalPostgresReplicaCount))
					container = &statefulSet.Spec.Template.Spec.Containers[0]
				}
			}
			g.Expect(container).NotTo(BeNil())
			if container != nil {
				g.Expect(container.Resources).To(Equal(tc.expected))
			}
		})
	}
}

func TestReconcileInternalActionUsesConfiguredPostgresImage(t *testing.T) {
	const postgresImage = "registry.redhat.io/rhel9/postgresql-16"

	for _, extension := range []string{"vector", "pg_trgm", "uuid-ossp", "pgcrypto"} {
		t.Run(extension, func(t *testing.T) {
			g := NewWithT(t)
			provider := &infraApi.DatabaseProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "internal"},
				Spec: infraApi.DatabaseProviderSpec{
					Type: infraApi.ProviderTypeInternal,
					Internal: &infraApi.InternalProviderSpec{
						Namespace:  "database-system",
						Storage:    infraApi.StorageSpec{Size: resource.MustParse("1Gi")},
						Extensions: []string{extension},
					},
				},
			}
			rr := testRequest(t, testClient(t), provider)
			m := &Controller{Options: Options{cfg: &moduleconfig.Config{
				PostgresImage: postgresImage,
			}}}

			g.Expect(m.reconcileInternalAction(context.Background(), rr)).To(Succeed())

			for i := range rr.Resources {
				if rr.Resources[i].GetKind() != internalStatefulSetKind {
					continue
				}
				statefulSet := &appsv1.StatefulSet{}
				g.Expect(runtime.DefaultUnstructuredConverter.FromUnstructured(rr.Resources[i].Object, statefulSet)).To(Succeed())
				g.Expect(statefulSet.Spec.Template.Spec.Containers).To(HaveLen(1))
				g.Expect(statefulSet.Spec.Template.Spec.Containers[0].Image).To(Equal(postgresImage))
				return
			}

			t.Fatal("internal provider did not render a StatefulSet")
		})
	}
}

func TestInternalReadinessRequiresDesiredReplicaCount(t *testing.T) {
	for _, tc := range []struct {
		name          string
		readyReplicas int32
		ready         bool
	}{
		{name: "not ready", readyReplicas: 0},
		{name: "ready", readyReplicas: pginstance.InternalPostgresReplicaCount, ready: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			provider := &infraApi.DatabaseProvider{ObjectMeta: metav1.ObjectMeta{Name: "internal"}, Spec: infraApi.DatabaseProviderSpec{Type: infraApi.ProviderTypeInternal, Internal: &infraApi.InternalProviderSpec{Namespace: "database-system"}}}
			sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "internal", Namespace: "database-system"}, Status: appsv1.StatefulSetStatus{ReadyReplicas: tc.readyReplicas}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "internal-admin", Namespace: "database-system"}, Data: map[string][]byte{postgres.SecretKeyHost: []byte("internal.database-system.svc"), postgres.SecretKeyPort: []byte("5432"), postgres.SecretKeyUser: []byte("postgres"), postgres.SecretKeyPassword: []byte("original-password"), postgres.SecretKeyDatabase: []byte("postgres")}}
			m := &Controller{Options: Options{cfg: &moduleconfig.Config{}, PostgresClientFactory: func(_ context.Context, _ postgres.Config) (postgres.Client, error) { return &fakePostgresClient{}, nil }}}
			rr := testRequest(t, testClient(t, sts, secret), provider)
			rr.Conditions.Mark(ConditionTLSConfiguration, metav1.ConditionTrue)
			g.Expect(m.internalReadinessAction(context.Background(), rr)).To(Succeed())
			reachable := conditions.FindStatusCondition(provider, ConditionReachable)
			if tc.ready {
				g.Expect(reachable.Status).To(Equal(metav1.ConditionTrue))
			} else {
				g.Expect(reachable.Status).To(Equal(metav1.ConditionFalse))
			}
		})
	}
}

func TestMarkInternalTLSConfigurationPrefersDisabledState(t *testing.T) {
	g := NewWithT(t)
	provider := &infraApi.DatabaseProvider{}
	rr := testRequest(t, testClient(t), provider)

	markInternalTLSConfiguration(rr, TLSState{}, errors.New("admin Secret unavailable"))

	condition := conditions.FindStatusCondition(provider, ConditionTLSConfiguration)
	g.Expect(condition).NotTo(BeNil())
	g.Expect(condition.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(condition.Reason).To(Equal(reasonTLSNotEnabled))
}

func TestResolveInternalTLSStateRequiresCompleteCredentials(t *testing.T) {
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "internal"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeInternal,
			Internal: &infraApi.InternalProviderSpec{
				Namespace: "database-system",
				TLS:       &infraApi.InternalProviderTLSSpec{},
			},
		},
	}
	cfg := &moduleconfig.Config{OperatorNamespace: "operator-system"}

	for _, tc := range []struct {
		name  string
		data  map[string][]byte
		ready bool
	}{
		{
			name: "CA only",
			data: map[string][]byte{postgres.SecretKeyCA: []byte("ca")},
		},
		{
			name: "CA and certificate without key",
			data: map[string][]byte{
				postgres.SecretKeyCA: []byte("ca"),
				"tls.crt":            []byte("certificate"),
			},
		},
		{
			name: "complete credentials",
			data: map[string][]byte{
				postgres.SecretKeyCA: []byte("ca"),
				"tls.crt":            []byte("certificate"),
				"tls.key":            []byte("key"),
			},
			ready: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := NewWithT(t)
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      internalTLSSecretName(provider),
					Namespace: "database-system",
				},
				Data: tc.data,
			}

			state, err := resolveInternalTLSState(context.Background(), testClient(t, secret), provider, cfg)

			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(state.Ready).To(Equal(tc.ready))
		})
	}
}

func TestAddInternalResourcesDefersStatefulSetUntilTLSIsReady(t *testing.T) {
	g := NewWithT(t)
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "internal"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeInternal,
			Internal: &infraApi.InternalProviderSpec{
				Namespace: "database-system",
				Storage:   infraApi.StorageSpec{Size: resource.MustParse("1Gi")},
				TLS:       &infraApi.InternalProviderTLSSpec{},
			},
		},
	}
	rr := testRequest(t, testClient(t), provider)
	m := &Controller{Options: Options{cfg: &moduleconfig.Config{
		OperatorNamespace: "operator-system",
		PostgresImage:     moduleconfig.DefaultInternalPostgresImage,
	}}}

	g.Expect(m.addInternalResources(context.Background(), rr, provider, TLSState{Enabled: true}, false)).To(Succeed())

	kinds := make(map[string]struct{}, len(rr.Resources))
	for i := range rr.Resources {
		kinds[rr.Resources[i].GetKind()] = struct{}{}
	}
	g.Expect(kinds).NotTo(HaveKey(internalStatefulSetKind))
	g.Expect(kinds).To(HaveKey("Certificate"))
	g.Expect(kinds).To(HaveKey("Issuer"))
	g.Expect(kinds).To(HaveKey("ConfigMap"))
}

func TestInternalReadinessRejectsUnusableAdminSecret(t *testing.T) {
	g := NewWithT(t)
	provider := &infraApi.DatabaseProvider{ObjectMeta: metav1.ObjectMeta{Name: "internal"}, Spec: infraApi.DatabaseProviderSpec{Type: infraApi.ProviderTypeInternal, Internal: &infraApi.InternalProviderSpec{Namespace: "database-system"}}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "internal", Namespace: "database-system"}, Status: appsv1.StatefulSetStatus{ReadyReplicas: pginstance.InternalPostgresReplicaCount}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "internal-admin", Namespace: "database-system"}, Data: map[string][]byte{postgres.SecretKeyHost: []byte("internal.database-system.svc"), postgres.SecretKeyPort: []byte("5432"), postgres.SecretKeyUser: []byte("postgres"), postgres.SecretKeyPassword: []byte("new-password"), postgres.SecretKeyDatabase: []byte("postgres")}}
	m := &Controller{Options: Options{cfg: &moduleconfig.Config{}, PostgresClientFactory: func(_ context.Context, cfg postgres.Config) (postgres.Client, error) {
		return &fakePostgresClient{pingErr: postgres.ProjectError(
			"ping",
			errors.New("authentication failed for new-password"),
			cfg.Password,
		)}, nil
	}}}
	rr := testRequest(t, testClient(t, sts, secret), provider)
	rr.Conditions.Mark(ConditionTLSConfiguration, metav1.ConditionTrue)
	g.Expect(m.internalReadinessAction(context.Background(), rr)).To(Succeed())
	reachable := conditions.FindStatusCondition(provider, ConditionReachable)
	g.Expect(reachable.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(reachable.Reason).To(Equal("AdminConnectionFailed"))
	g.Expect(reachable.Message).To(ContainSubstring("[REDACTED]"))
	g.Expect(reachable.Message).NotTo(ContainSubstring("new-password"))
}

func TestReconcileInternalActionDoesNotReplaceLostAdminCredentials(t *testing.T) {
	g := NewWithT(t)
	provider := &infraApi.DatabaseProvider{ObjectMeta: metav1.ObjectMeta{Name: "internal"}, Spec: infraApi.DatabaseProviderSpec{Type: infraApi.ProviderTypeInternal, Internal: &infraApi.InternalProviderSpec{Namespace: "database-system"}}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "internal", Namespace: "database-system"}}
	rr := testRequest(t, testClient(t, pvc), provider)
	m := &Controller{Options: Options{cfg: &moduleconfig.Config{}}}
	err := m.reconcileInternalAction(context.Background(), rr)
	var stop odherrors.StopError
	g.Expect(errors.As(err, &stop)).To(BeTrue())
	g.Expect(stop.RequeueAfter()).To(Equal(moduleconfig.DefaultRetryInterval))
	g.Expect(rr.Resources).To(BeEmpty())
	reachable := conditions.FindStatusCondition(provider, ConditionReachable)
	g.Expect(reachable.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(reachable.Reason).To(Equal(reasonAdminSecretUnavailable))
	g.Expect(reachable.Message).To(ContainSubstring("restore Secret database-system/internal-admin"))
}

func TestReconcileInternalActionReportsMissingCertManager(t *testing.T) {
	g := NewWithT(t)
	provider := &infraApi.DatabaseProvider{ObjectMeta: metav1.ObjectMeta{Name: "internal"}, Spec: infraApi.DatabaseProviderSpec{Type: infraApi.ProviderTypeInternal, Internal: &infraApi.InternalProviderSpec{Namespace: "database-system", TLS: &infraApi.InternalProviderTLSSpec{}}}}
	rr := testRequest(t, testClient(t), provider)
	m := &Controller{Options: Options{cfg: &moduleconfig.Config{}}}
	g.Expect(m.reconcileInternalAction(context.Background(), rr)).To(Succeed())
	tlsCondition := conditions.FindStatusCondition(provider, ConditionTLSConfiguration)
	g.Expect(tlsCondition).NotTo(BeNil())
	g.Expect(tlsCondition.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(tlsCondition.Reason).To(Equal("CertManagerUnavailable"))
	g.Expect(tlsCondition.Message).To(ContainSubstring("cert-manager"))
	g.Expect(rr.Resources).To(BeEmpty())
}

func externalProvider() *infraApi.DatabaseProvider {
	return &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "external"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeExternal,
			External: &infraApi.ExternalProviderSpec{
				ConnectionSecretRef: corev1.SecretReference{Namespace: "database-system", Name: "external-admin"},
			},
		},
	}
}

func externalAdminSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-admin", Namespace: "database-system"},
		Data: map[string][]byte{
			postgres.SecretKeyHost:     []byte("postgres.example.test"),
			postgres.SecretKeyPort:     []byte("5432"),
			postgres.SecretKeyUser:     []byte("admin"),
			postgres.SecretKeyPassword: []byte("secret"),
			postgres.SecretKeyDatabase: []byte("postgres"),
			postgres.SecretKeySSLMode:  []byte(postgres.SSLModeRequire),
		},
	}
}

func testClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		appsv1.AddToScheme,
		networkingv1.AddToScheme,
		infraApi.AddToScheme,
		configv1.AddToScheme,
		apiextensionsv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func testRequest(t *testing.T, cli client.Client, provider *infraApi.DatabaseProvider) *odhtypes.ReconciliationRequest {
	t.Helper()
	aggregator, err := conditions.NewAggregator(
		fwapi.ConditionTypeReady,
		conditions.Dependent(fwapi.ConditionType(ConditionReachable), conditions.HealthyWhenTrue),
		conditions.Dependent(fwapi.ConditionType(ConditionTLSConfiguration), conditions.HealthyWhenTrue),
	)
	if err != nil {
		t.Fatal(err)
	}
	return &odhtypes.ReconciliationRequest{
		Client:     cli,
		Conditions: conditions.NewManager(provider, aggregator),
		Instance:   provider,
	}
}

type fakePostgresClient struct {
	version int
	pingErr error
}

func (c *fakePostgresClient) Config() postgres.Config { return postgres.Config{} }
func (c *fakePostgresClient) Close()                  {}
func (c *fakePostgresClient) Ping(context.Context) error {
	return c.pingErr
}
func (c *fakePostgresClient) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (c *fakePostgresClient) Query(context.Context, string, ...any) (postgres.Rows, error) {
	return nil, errors.New("unexpected query")
}
func (c *fakePostgresClient) QueryRow(context.Context, string, ...any) (pgx.Row, error) {
	return fakePostgresRow{version: c.version}, nil
}

type fakePostgresRow struct{ version int }

func (r fakePostgresRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("unexpected scan arguments")
	}
	version, ok := dest[0].(*int)
	if !ok {
		return errors.New("unexpected scan target")
	}
	*version = r.version
	return nil
}
