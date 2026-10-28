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

// Package envtest tests DatabaseService reconciliation against envtest. Run it with make test-envtest.
package envtest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	servicesv1alpha1 "github.com/opendatahub-io/opendatahub-db-operator/api/services/v1alpha1"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	modulemanager "github.com/opendatahub-io/opendatahub-db-operator/pkg/manager"
	pginstance "github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres/instance"
	"github.com/opendatahub-io/opendatahub-db-operator/test/support"
)

// TestDatabaseServiceCreatesDefaultProvider verifies the service and provider against the local API server.
func TestDatabaseServiceCreatesDefaultProvider(t *testing.T) {
	ctrl.SetLogger(logr.Discard())

	// NewWithT snapshots duration defaults; set them before constructing g.
	gomegaCfg := support.LoadGomegaConfig()
	SetDefaultEventuallyTimeout(gomegaCfg.EventuallyTimeout)
	SetDefaultEventuallyPollingInterval(gomegaCfg.EventuallyPollingInterval)
	SetDefaultConsistentlyPollingInterval(gomegaCfg.ConsistentlyPollingInterval)
	g := NewWithT(t)

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
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

	// envtest is a single, short-lived process -- leader election (which
	// needs a Lease in an existing namespace) and the metrics/health/pprof
	// listeners aren't needed to exercise the reconciler.
	cfg.Controller.LeaderElection.Enabled = false
	cfg.Controller.Metrics.BindAddress = "0"
	cfg.Controller.Health.BindAddress = "0"
	cfg.Controller.Pprof.BindAddress = "0"
	cfg.Controller.Webhook.Enabled = false

	mgr, err := modulemanager.New(ctx, restCfg, cfg)
	g.Expect(err).NotTo(HaveOccurred())

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

	g.Expect(mgr.GetCache().WaitForCacheSync(ctx)).To(BeTrue(), "manager cache failed to sync")

	cli := mgr.GetClient()
	g.Expect(cli.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: cfg.OperatorNamespace}})).To(Succeed())

	instance := &servicesv1alpha1.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{
			Name: servicesv1alpha1.DatabaseServiceInstanceName,
		},
	}
	g.Expect(cli.Create(ctx, instance)).To(Succeed())

	g.Eventually(func(g Gomega) {
		select {
		case <-managerDone:
			g.Expect(managerDone).NotTo(BeClosed(), "manager stopped unexpectedly: %v", managerErr)
		default:
		}

		got := &servicesv1alpha1.DatabaseService{}
		g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(instance), got)).To(Succeed())
		g.Expect(got.Status.DefaultProviderName).To(Equal(cfg.DefaultProviderName))

		readyCond := conditions.FindStatusCondition(got, string(fwapi.ConditionTypeReady))
		g.Expect(readyCond).NotTo(BeNil(), "no Ready condition on status.conditions")
		g.Expect(readyCond.Status).To(Equal(metav1.ConditionFalse))
		release := got.Status.ComponentReleaseStatus.GetRelease(moduleconfig.ReleasePlatform)
		g.Expect(release).NotTo(BeNil(), "no platform release in status.releases")
		g.Expect(release.Version).To(Equal(cfg.ComponentRelease().Version))
	}).Should(Succeed())

	g.Eventually(func(g Gomega) {
		provider := &infraApi.DatabaseProvider{}
		g.Expect(cli.Get(ctx, client.ObjectKey{Name: cfg.DefaultProviderName}, provider)).To(Succeed())
		g.Expect(provider.Spec.Type).To(Equal(infraApi.ProviderTypeInternal))
		g.Expect(provider.Spec.Internal.Storage.Size.String()).To(Equal("20Gi"))
		statefulSet := &appsv1.StatefulSet{}
		key := client.ObjectKey{Namespace: cfg.OperatorNamespace, Name: cfg.DefaultProviderName}
		g.Expect(cli.Get(ctx, key, statefulSet)).To(Succeed())
		g.Expect(*statefulSet.Spec.Replicas).To(Equal(pginstance.InternalPostgresReplicaCount))
	}).Should(Succeed())

	for _, claim := range []client.Object{
		&infraApi.DatabaseClaim{ObjectMeta: metav1.ObjectMeta{Name: "default-database", Namespace: cfg.OperatorNamespace}},
		&infraApi.SchemaClaim{ObjectMeta: metav1.ObjectMeta{Name: "default-schema", Namespace: cfg.OperatorNamespace}},
	} {
		g.Expect(cli.Create(ctx, claim)).To(Succeed())
	}
	g.Eventually(func(g Gomega) {
		database := &infraApi.DatabaseClaim{}
		key := client.ObjectKey{Namespace: cfg.OperatorNamespace, Name: "default-database"}
		g.Expect(cli.Get(ctx, key, database)).To(Succeed())
		g.Expect(database.Status.Provider).To(Equal(cfg.DefaultProviderName))
		schema := &infraApi.SchemaClaim{}
		key = client.ObjectKey{Namespace: cfg.OperatorNamespace, Name: "default-schema"}
		g.Expect(cli.Get(ctx, key, schema)).To(Succeed())
		g.Expect(schema.Status.Provider).To(Equal(cfg.DefaultProviderName))
	}).Should(Succeed())
	databaseClaim := &infraApi.DatabaseClaim{}
	claimKey := client.ObjectKey{Namespace: cfg.OperatorNamespace, Name: "default-database"}
	g.Eventually(func() error {
		if err := mgr.GetAPIReader().Get(ctx, claimKey, databaseClaim); err != nil {
			return err
		}
		databaseClaim.Annotations = map[string]string{"test": "updated"}
		return cli.Update(ctx, databaseClaim)
	}).Should(Succeed())
	g.Expect(mgr.GetAPIReader().Get(ctx, claimKey, databaseClaim)).To(Succeed())
	databaseClaim.Spec.Provider.Name = "other-provider"
	err = cli.Update(ctx, databaseClaim)
	g.Expect(apierrors.IsInvalid(err)).To(BeTrue(), "an implicit binding must not be changed after creation: %v", err)

	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "external-missing-secret"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeExternal,
			External: &infraApi.ExternalProviderSpec{
				ConnectionSecretRef: corev1.SecretReference{Namespace: "default", Name: "missing-admin"},
			},
		},
	}
	g.Expect(cli.Create(ctx, provider)).To(Succeed())
	g.Eventually(func(g Gomega) {
		got := &infraApi.DatabaseProvider{}
		g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(provider), got)).To(Succeed())
		reachable := conditions.FindStatusCondition(got, "Reachable")
		g.Expect(reachable).NotTo(BeNil())
		g.Expect(reachable.Status).To(Equal(metav1.ConditionFalse))
		g.Expect(reachable.Reason).To(Equal("ConnectionSecretUnavailable"))
		g.Expect(reachable.Message).To(ContainSubstring("missing-admin"))
		g.Expect(got.Status.ServerVersion).To(BeEmpty())
	}).Should(Succeed())

	g.Expect(cli.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "database-system"}})).To(Succeed())
	internalProvider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "internal"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeInternal,
			Internal: &infraApi.InternalProviderSpec{
				Namespace: "database-system",
				Storage:   infraApi.StorageSpec{Size: resource.MustParse("1Gi")},
			},
		},
	}
	g.Expect(cli.Create(ctx, internalProvider)).To(Succeed())
	internalKey := client.ObjectKey{Namespace: "database-system", Name: "internal"}
	g.Eventually(func(g Gomega) {
		statefulSet := &appsv1.StatefulSet{}
		g.Expect(cli.Get(ctx, internalKey, statefulSet)).To(Succeed())
		g.Expect(statefulSet.Spec.Replicas).NotTo(BeNil())
		g.Expect(*statefulSet.Spec.Replicas).To(Equal(pginstance.InternalPostgresReplicaCount))
		pvc := &corev1.PersistentVolumeClaim{}
		g.Expect(cli.Get(ctx, internalKey, pvc)).To(Succeed())
		g.Expect(pvc.Spec.Resources.Requests.Storage().String()).To(Equal("1Gi"))
		policy := &networkingv1.NetworkPolicy{}
		g.Expect(cli.Get(ctx, internalKey, policy)).To(Succeed())
		g.Expect(policy.Spec.Ingress).To(HaveLen(1))
		g.Expect(policy.Spec.Ingress[0].From).To(HaveLen(1))
	}).Should(Succeed())

	g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(internalProvider), internalProvider)).To(Succeed())
	internalProvider.Spec.Internal.TLS = &infraApi.InternalProviderTLSSpec{}
	g.Expect(cli.Update(ctx, internalProvider)).To(Succeed())
	g.Eventually(func(g Gomega) {
		got := &infraApi.DatabaseProvider{}
		g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(internalProvider), got)).To(Succeed())
		tlsCondition := conditions.FindStatusCondition(got, "TLSConfiguration")
		g.Expect(tlsCondition).NotTo(BeNil())
		g.Expect(tlsCondition.Reason).To(Equal("CertManagerUnavailable"))
	}).Should(Succeed())

	g.Expect(cli.Delete(ctx, instance)).To(Succeed())
	g.Eventually(func(g Gomega) {
		got := &servicesv1alpha1.DatabaseService{}
		g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(instance), got)).To(Succeed())
		blocked := conditions.FindStatusCondition(got, "DefaultProviderReady")
		g.Expect(blocked).NotTo(BeNil())
		g.Expect(blocked.Reason).To(Equal("BlockingClaims"))
	}).Should(Succeed())
	g.Expect(cli.Get(ctx, client.ObjectKey{Name: cfg.DefaultProviderName}, &infraApi.DatabaseProvider{})).To(Succeed())

	for _, claim := range []client.Object{
		&infraApi.DatabaseClaim{},
		&infraApi.SchemaClaim{},
	} {
		key := client.ObjectKey{Namespace: cfg.OperatorNamespace, Name: "default-database"}
		if _, ok := claim.(*infraApi.SchemaClaim); ok {
			key.Name = "default-schema"
		}
		g.Expect(cli.Get(ctx, key, claim)).To(Succeed())
		g.Expect(cli.Delete(ctx, claim)).To(Succeed())
		g.Eventually(func() error {
			if err := cli.Get(ctx, key, claim); apierrors.IsNotFound(err) {
				return nil
			} else if err != nil {
				return err
			}
			claim.SetFinalizers(nil)
			return cli.Update(ctx, claim)
		}).Should(Succeed())
	}
	g.Eventually(func(g Gomega) {
		err := cli.Get(ctx, client.ObjectKey{Name: cfg.DefaultProviderName}, &infraApi.DatabaseProvider{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "default provider should be removed after claims")
		err = cli.Get(ctx, client.ObjectKeyFromObject(instance), &servicesv1alpha1.DatabaseService{})
		g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "DatabaseService should finish deleting")
		for _, obj := range []client.Object{&corev1.PersistentVolumeClaim{}, &corev1.Secret{}} {
			key := client.ObjectKey{Namespace: cfg.OperatorNamespace, Name: cfg.DefaultProviderName}
			if _, ok := obj.(*corev1.Secret); ok {
				key.Name += "-admin"
			}
			g.Expect(cli.Get(ctx, key, obj)).To(Succeed())
			g.Expect(obj.GetOwnerReferences()).To(BeEmpty())
		}
	}).Should(Succeed())
}
