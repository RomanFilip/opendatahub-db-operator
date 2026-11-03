// Package databaseserviceaggregation runs in a separate test process because
// controller-runtime rejects duplicate controller names across managers in one process.
package databaseserviceaggregation

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	. "github.com/onsi/gomega"
	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	servicesv1alpha1 "github.com/opendatahub-io/opendatahub-db-operator/api/services/v1alpha1"
	"github.com/opendatahub-io/opendatahub-db-operator/internal/controller/databaseservice"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	modulemanager "github.com/opendatahub-io/opendatahub-db-operator/pkg/manager"
	dbmetrics "github.com/opendatahub-io/opendatahub-db-operator/pkg/metrics"
	"github.com/opendatahub-io/opendatahub-db-operator/test/support"
)

func TestDatabaseServiceAggregatesProviderStatus(t *testing.T) {
	g := NewWithT(t)

	ctrl.SetLogger(logr.Discard())

	gomegaCfg := support.LoadGomegaConfig()
	SetDefaultEventuallyTimeout(gomegaCfg.EventuallyTimeout)
	SetDefaultEventuallyPollingInterval(gomegaCfg.EventuallyPollingInterval)

	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	restCfg, err := env.Start()
	g.Expect(err).NotTo(HaveOccurred())
	t.Cleanup(func() {
		g.Expect(env.Stop()).To(Succeed())
	})

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	cfg, err := moduleconfig.Load()
	g.Expect(err).NotTo(HaveOccurred())
	cfg.Controller.LeaderElection.Enabled = false
	cfg.Controller.Webhook.Enabled = false
	cfg.DefaultProviderName = "aggregation-default"

	scheme, err := modulemanager.NewScheme()
	g.Expect(err).NotTo(HaveOccurred())
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(databaseservice.NewReconciler(ctx, mgr, cfg)).To(Succeed())

	bootstrapClient, err := client.New(restCfg, client.Options{Scheme: scheme})
	g.Expect(err).NotTo(HaveOccurred())
	defaultProvider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: cfg.DefaultProviderName,
			Annotations: map[string]string{
				dbcontroller.AnnotationDefaultProviderOwner: servicesv1alpha1.DatabaseServiceInstanceName,
			},
		},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeInternal,
			Internal: &infraApi.InternalProviderSpec{
				Storage: infraApi.StorageSpec{Size: resource.MustParse("20Gi")},
			},
		},
	}
	g.Expect(bootstrapClient.Create(ctx, defaultProvider)).To(Succeed())
	defaultProvider.Status.Conditions = []fwapi.Condition{{
		Type:   "Reachable",
		Status: metav1.ConditionTrue,
		Reason: "ConnectionVerified",
	}}
	g.Expect(bootstrapClient.Status().Update(ctx, defaultProvider)).To(Succeed())
	service := &servicesv1alpha1.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{Name: servicesv1alpha1.DatabaseServiceInstanceName},
	}
	g.Expect(bootstrapClient.Create(ctx, service)).To(Succeed())
	service.Status.DefaultProviderName = cfg.DefaultProviderName
	g.Expect(bootstrapClient.Status().Update(ctx, service)).To(Succeed())
	storedService := &servicesv1alpha1.DatabaseService{}
	g.Expect(bootstrapClient.Get(ctx, client.ObjectKeyFromObject(service), storedService)).To(Succeed())
	g.Expect(storedService.Status.DefaultProviderName).To(Equal(cfg.DefaultProviderName))

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

	// The first reconcile must finish before the subtests because it registers
	// the dynamic DatabaseProvider watch they rely on.
	g.Eventually(func(g Gomega) {
		got := &servicesv1alpha1.DatabaseService{}
		g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(service), got)).To(Succeed())
		providerHealth := conditions.FindStatusCondition(got, "ProviderHealth")
		g.Expect(providerHealth).NotTo(BeNil())
		g.Expect(providerHealth.Reason).To(Equal("ProvidersReachable"))
	}).Should(Succeed())

	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "aggregation-provider"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeExternal,
			External: &infraApi.ExternalProviderSpec{
				ConnectionSecretRef: corev1.SecretReference{Namespace: "default", Name: "admin"},
			},
		},
	}

	t.Run("unreachable provider sets degraded", func(t *testing.T) {
		g := NewWithT(t)

		g.Expect(cli.Create(ctx, provider)).To(Succeed())

		// The dynamic provider watch attaches asynchronously once the first
		// reconcile registers it, so a single status write can be missed if it
		// races the informer startup. Rewrite the status (unique message per
		// attempt so each write bumps the resourceVersion) until the service
		// re-aggregates; passing proves the watch is live, which the in-range
		// subtest below also relies on.
		var probe int
		g.Eventually(func(g Gomega) {
			probe++
			g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(provider), provider)).To(Succeed())
			provider.Status.Conditions = []fwapi.Condition{{
				Type:   "Reachable",
				Status: metav1.ConditionFalse,
				Reason: "ConnectionFailed",
				Message: fmt.Sprintf(
					"PostgreSQL is unreachable from the operator namespace. Check the admin Secret and network access (probe %d)",
					probe,
				),
			}}
			g.Expect(cli.Status().Update(ctx, provider)).To(Succeed())

			got := &servicesv1alpha1.DatabaseService{}
			g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(service), got)).To(Succeed())
			providerHealth := conditions.FindStatusCondition(got, "ProviderHealth")
			g.Expect(providerHealth).NotTo(BeNil())
			g.Expect(providerHealth.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(providerHealth.Message).To(ContainSubstring(`DatabaseProvider "aggregation-provider" is unreachable`))
			g.Expect(providerHealth.Message).To(ContainSubstring("Check the admin Secret and network access"))

			degraded := conditions.FindStatusCondition(got, "Degraded")
			g.Expect(degraded).NotTo(BeNil())
			g.Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
			g.Expect(degraded.Message).To(ContainSubstring(`DatabaseProvider "aggregation-provider" is unreachable`))

			// The provider reports no server version at all: unknown is not
			// out-of-range, so the aggregated condition stays True.
			expectServerVersion(g, got, metav1.ConditionTrue, "ServerVersionsWithinTestedRange")
		}).Should(Succeed())
	})

	t.Run("out-of-range version is informational", func(t *testing.T) {
		g := NewWithT(t)

		outOfRange := []fwapi.Condition{
			{
				Type:    "Reachable",
				Status:  metav1.ConditionTrue,
				Reason:  "ConnectionVerified",
				Message: "Connection verified",
			},
			{
				Type:     "ServerVersion",
				Status:   metav1.ConditionFalse,
				Reason:   "ServerVersionOutsideTestedRange",
				Severity: fwapi.ConditionSeverityInfo,
				Message:  "PostgreSQL 12.0 is outside the tested range (13 through 17); connections and claims remain allowed",
			},
		}
		provider.Status.Conditions = outOfRange
		updateProviderStatus(g, ctx, cli, provider)
		triggerServiceReconcile(t, ctx, cli, service)

		eventuallyService(t, ctx, cli, service, func(g Gomega, got *servicesv1alpha1.DatabaseService) {
			providerHealth := conditions.FindStatusCondition(got, "ProviderHealth")
			g.Expect(providerHealth).NotTo(BeNil())
			g.Expect(providerHealth.Status).To(Equal(metav1.ConditionTrue))

			degraded := conditions.FindStatusCondition(got, "Degraded")
			g.Expect(degraded).NotTo(BeNil())
			g.Expect(degraded.Status).To(Equal(metav1.ConditionFalse))

			version := conditions.FindStatusCondition(got, "ServerVersion")
			g.Expect(version).NotTo(BeNil())
			g.Expect(version.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(version.Severity).To(Equal(fwapi.ConditionSeverityInfo))
			g.Expect(version.Message).To(ContainSubstring(
				`DatabaseProvider "aggregation-provider" reports PostgreSQL 12.0 is outside the tested range`,
			))
		})
	})

	t.Run("in-range version is marked true", func(t *testing.T) {
		g := NewWithT(t)

		g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(provider), provider)).To(Succeed())
		provider.Status.Conditions = []fwapi.Condition{
			{
				Type:    "Reachable",
				Status:  metav1.ConditionTrue,
				Reason:  "ConnectionVerified",
				Message: "Connection verified",
			},
			{
				Type:     "ServerVersion",
				Status:   metav1.ConditionTrue,
				Reason:   "ServerVersionSupported",
				Severity: fwapi.ConditionSeverityInfo,
				Message:  "PostgreSQL 16.4 is within the tested range",
			},
		}
		updateProviderStatus(g, ctx, cli, provider)

		eventuallyService(t, ctx, cli, service, func(g Gomega, got *servicesv1alpha1.DatabaseService) {
			expectServerVersion(g, got, metav1.ConditionTrue, "ServerVersionsWithinTestedRange")
		})
	})

	t.Run("zero providers is distinct from healthy", func(t *testing.T) {
		g := NewWithT(t)

		dbmetrics.SetProviderReachable(provider.Name, false)
		dbmetrics.SetProviderServerVersion(provider.Name, "12.0")
		// Stop default-provider management before deleting its provider so the
		// next reconcile observes an empty provider list.
		cfg.DefaultProviderName = "aggregation-configured"
		g.Expect(cli.Delete(ctx, provider)).To(Succeed())
		g.Expect(cli.Delete(ctx, defaultProvider)).To(Succeed())
		g.Eventually(func(g Gomega) {
			got := &infraApi.DatabaseProvider{}
			err := cli.Get(ctx, client.ObjectKeyFromObject(provider), got)
			g.Expect(err).To(HaveOccurred())
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
			err = cli.Get(ctx, client.ObjectKeyFromObject(defaultProvider), got)
			g.Expect(err).To(HaveOccurred())
			g.Expect(apierrors.IsNotFound(err)).To(BeTrue())
		}).Should(Succeed())
		triggerServiceReconcile(t, ctx, cli, service)

		eventuallyService(t, ctx, cli, service, func(g Gomega, got *servicesv1alpha1.DatabaseService) {
			providerHealth := conditions.FindStatusCondition(got, "ProviderHealth")
			g.Expect(providerHealth).NotTo(BeNil())
			g.Expect(providerHealth.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(providerHealth.Reason).To(Equal("NoProvidersConfigured"))
			g.Expect(providerHealth.Message).To(ContainSubstring("No DatabaseProvider resources exist"))

			degraded := conditions.FindStatusCondition(got, "Degraded")
			g.Expect(degraded).NotTo(BeNil())
			g.Expect(degraded.Status).To(Equal(metav1.ConditionFalse))
			g.Expect(degraded.Message).To(ContainSubstring("No DatabaseProvider resources exist"))

			expectServerVersion(g, got, metav1.ConditionUnknown, "NoProvidersConfigured")
		})
		g.Eventually(func(g Gomega) {
			g.Expect(testutil.GatherAndCount(ctrlmetrics.Registry, "dbprovider_reachable")).To(Equal(0))
			g.Expect(testutil.GatherAndCount(ctrlmetrics.Registry, "dbprovider_server_version_info")).To(Equal(0))
		}).Should(Succeed())
	})
}

func updateProviderStatus(
	g Gomega,
	ctx context.Context,
	cli client.Client,
	provider *infraApi.DatabaseProvider,
) {
	// The manager client is cached: the Get can return a stale resourceVersion
	// right after a previous status write (the informer lags the API server),
	// which makes Status().Update fail with a 409 conflict. Retry with a fresh
	// Get only on conflict; any other failure is a real error and fails fast.
	wantConditions := provider.Status.Conditions
	g.Eventually(func(g Gomega) {
		g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(provider), provider)).To(Succeed())
		provider.Status.Conditions = wantConditions
		err := cli.Status().Update(ctx, provider)
		g.Expect(apierrors.IsConflict(err)).To(BeFalse(), "informer cache lag should resolve within the retry window")
		g.Expect(err).To(Succeed())
	}).Should(Succeed())
}

func triggerServiceReconcile(
	t *testing.T,
	ctx context.Context,
	cli client.Client,
	service *servicesv1alpha1.DatabaseService,
) {
	t.Helper()
	g := NewWithT(t)

	// The service controller's own status writes race this annotation bump,
	// so re-read and retry on conflict instead of failing the test.
	g.Eventually(func(g Gomega) {
		g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(service), service)).To(Succeed())
		annotations := service.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations["test.opendatahub.io/reconcile"] = t.Name()
		service.SetAnnotations(annotations)
		g.Expect(cli.Update(ctx, service)).To(Succeed())
	}).WithTimeout(time.Minute).Should(Succeed())
}

func expectServerVersion(
	g Gomega,
	obj *servicesv1alpha1.DatabaseService,
	status metav1.ConditionStatus,
	reason string,
) {
	version := conditions.FindStatusCondition(obj, "ServerVersion")
	g.Expect(version).NotTo(BeNil())
	g.Expect(version.Status).To(Equal(status))
	g.Expect(version.Severity).To(Equal(fwapi.ConditionSeverityInfo))
	g.Expect(version.Reason).To(Equal(reason))
}

func eventuallyService(
	t *testing.T,
	ctx context.Context,
	cli client.Client,
	service *servicesv1alpha1.DatabaseService,
	assert func(Gomega, *servicesv1alpha1.DatabaseService),
) {
	t.Helper()
	g := NewWithT(t)

	g.Eventually(func(g Gomega) {
		got := &servicesv1alpha1.DatabaseService{}
		g.Expect(cli.Get(ctx, client.ObjectKeyFromObject(service), got)).To(Succeed())
		assert(g, got)
	}).Should(Succeed())
}
