package databaseservice

import (
	"context"
	"errors"
	"strings"
	"testing"

	. "github.com/onsi/gomega"
	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	odhtypes "github.com/opendatahub-io/odh-platform-utilities/framework/controller/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	servicesApi "github.com/opendatahub-io/opendatahub-db-operator/api/services/v1alpha1"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	dbmetrics "github.com/opendatahub-io/opendatahub-db-operator/pkg/metrics"
)

func TestAggregateInputsUsesAPIReaderForMetricPruning(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	service := &servicesApi.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName},
	}
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "authoritative-provider"},
	}

	m := NewModule(&moduleconfig.Config{})
	m.apiReader = serviceTestClient(t, service, provider)
	rr := serviceTestRequest(t, serviceTestClient(t, service), service)

	dbmetrics.SetProviderReachable(provider.Name, true)
	g.Expect(m.aggregateInputs(ctx, rr)).To(Succeed())
	g.Expect(testutil.GatherAndCount(ctrlmetrics.Registry, "dbprovider_reachable")).To(Equal(1))

	dbmetrics.PruneProviderMetrics(nil)
}

func TestAggregateInputsClearsMetricsWhenProviderCRDIsMissing(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	service := &servicesApi.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName},
	}
	noMatch := &meta.NoKindMatchError{
		GroupKind: schema.GroupKind{
			Group: infraApi.GroupVersion.Group,
			Kind:  "DatabaseProvider",
		},
	}
	cachedClient := serviceTestClientWithListError(t, noMatch, service)

	m := NewModule(&moduleconfig.Config{})
	m.apiReader = serviceTestClient(t, service)
	rr := serviceTestRequest(t, cachedClient, service)

	dbmetrics.SetProviderReachable("removed-provider", true)
	g.Expect(m.aggregateInputs(ctx, rr)).To(Succeed())
	g.Expect(testutil.GatherAndCount(ctrlmetrics.Registry, "dbprovider_reachable")).To(Equal(0))
}

func TestAggregateInputsPrunesOnlyProvidersReturnedByAPIReader(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	service := &servicesApi.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName},
	}
	deletedProvider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "deleted-provider"},
	}
	currentProvider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "current-provider"},
	}

	m := NewModule(&moduleconfig.Config{})
	m.apiReader = serviceTestClient(t, service, currentProvider)
	rr := serviceTestRequest(t, serviceTestClient(t, service, deletedProvider), service)

	dbmetrics.SetProviderReachable(deletedProvider.Name, true)
	dbmetrics.SetProviderReachable(currentProvider.Name, true)
	g.Expect(m.aggregateInputs(ctx, rr)).To(Succeed())
	g.Expect(testutil.GatherAndCount(ctrlmetrics.Registry, "dbprovider_reachable")).To(Equal(1))

	dbmetrics.PruneProviderMetrics(nil)
}

func TestReportStatusRefreshesReleasesBeforeReturningAggregationError(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	service := &servicesApi.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName},
	}
	listErr := errors.New("list unavailable")
	apiReader := serviceTestClientWithListError(t, listErr, service)

	m := NewModule(&moduleconfig.Config{})
	m.apiReader = apiReader
	rr := serviceTestRequest(t, serviceTestClient(t, service), service)

	err := m.reportStatus(ctx, rr)
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("list unavailable"))
	g.Expect(service.Status.Releases).To(HaveLen(1))
	g.Expect(service.Status.Releases[0].Name).To(Equal(moduleconfig.ReleasePlatform))
}

func TestAggregateInputsReportsFailingClaims(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	service := &servicesApi.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName},
	}
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "reachable-provider"},
		Status: infraApi.DatabaseProviderStatus{
			Status: fwapi.Status{
				Conditions: []fwapi.Condition{{
					Type:   "Reachable",
					Status: metav1.ConditionTrue,
					Reason: "ConnectionVerified",
				}},
			},
		},
	}
	claim := &infraApi.SchemaClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "team-a"},
		Status: infraApi.SchemaClaimStatus{
			Status: fwapi.Status{
				Conditions: []fwapi.Condition{{
					Type:    dbcontroller.ConditionProvisioned,
					Status:  metav1.ConditionFalse,
					Reason:  "ExtensionNotAvailable",
					Message: `extension "vector" is not installed in the target database`,
				}},
			},
		},
	}

	m := NewModule(&moduleconfig.Config{})
	m.apiReader = serviceTestClient(t, service, provider, claim)
	rr := serviceTestRequest(t, serviceTestClient(t, service), service)

	g.Expect(m.aggregateInputs(ctx, rr)).To(Succeed())

	claimHealth := conditions.FindStatusCondition(service, ConditionClaimHealth)
	g.Expect(claimHealth).NotTo(BeNil())
	g.Expect(claimHealth.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(claimHealth.Reason).To(Equal("ClaimsNotProvisioned"))
	g.Expect(claimHealth.Message).To(ContainSubstring(`SchemaClaim "team-a/my-app" is not provisioned`))
	g.Expect(claimHealth.Message).To(ContainSubstring("vector"))
	g.Expect(claimHealth.Message).To(ContainSubstring("Install the required extension"))

	degraded := conditions.FindStatusCondition(service, string(fwapi.ConditionTypeDegraded))
	g.Expect(degraded).NotTo(BeNil())
	g.Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(degraded.Reason).To(Equal("InputsUnhealthy"))
	g.Expect(degraded.Message).To(ContainSubstring(`SchemaClaim "team-a/my-app" is not provisioned`))
}

func TestAggregateInputsPreservesProviderUnreachableReason(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	service := &servicesApi.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName},
	}
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "down-provider"},
		Status: infraApi.DatabaseProviderStatus{
			Status: fwapi.Status{
				Conditions: []fwapi.Condition{{
					Type:    "Reachable",
					Status:  metav1.ConditionFalse,
					Reason:  "ConnectionFailed",
					Message: "connection refused",
				}},
			},
		},
	}

	m := NewModule(&moduleconfig.Config{})
	m.apiReader = serviceTestClient(t, service, provider)
	rr := serviceTestRequest(t, serviceTestClient(t, service), service)

	g.Expect(m.aggregateInputs(ctx, rr)).To(Succeed())

	degraded := conditions.FindStatusCondition(service, string(fwapi.ConditionTypeDegraded))
	g.Expect(degraded).NotTo(BeNil())
	g.Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(degraded.Reason).To(Equal("ProviderUnreachable"))
	g.Expect(degraded.Message).To(ContainSubstring(`DatabaseProvider "down-provider" is unreachable`))
}

func TestFailingClaimSetsDegradedWithoutClearingReady(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	service := &servicesApi.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName},
	}
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "reachable-provider"},
		Status: infraApi.DatabaseProviderStatus{
			Status: fwapi.Status{
				Conditions: []fwapi.Condition{{
					Type:   "Reachable",
					Status: metav1.ConditionTrue,
					Reason: "ConnectionVerified",
				}},
			},
		},
	}
	claim := &infraApi.SchemaClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "team-a"},
		Status: infraApi.SchemaClaimStatus{
			Status: fwapi.Status{
				Conditions: []fwapi.Condition{{
					Type:    dbcontroller.ConditionProvisioned,
					Status:  metav1.ConditionFalse,
					Reason:  "ExtensionNotAvailable",
					Message: `extension "vector" is not installed in the target database`,
				}},
			},
		},
	}

	// Mirror production Ready dependents: ProviderHealth + DefaultProviderReady
	// only. ClaimHealth must not clear Ready when the module infrastructure is OK.
	aggregator, err := conditions.NewAggregator(fwapi.ConditionTypeReady,
		conditions.Dependent(fwapi.ConditionType(ConditionProviderHealth), conditions.HealthyWhenTrue),
		conditions.Dependent(conditionDefaultProviderReady, conditions.HealthyWhenTrue),
	)
	g.Expect(err).NotTo(HaveOccurred())
	rr := &odhtypes.ReconciliationRequest{
		Client:     serviceTestClient(t, service),
		Instance:   service,
		Conditions: conditions.NewManager(service, aggregator),
	}

	m := NewModule(&moduleconfig.Config{})
	m.apiReader = serviceTestClient(t, service, provider, claim)
	g.Expect(m.aggregateInputs(ctx, rr)).To(Succeed())
	rr.Conditions.MarkTrue(conditionDefaultProviderReady,
		conditions.WithReason("ProviderReady"),
		conditions.WithMessage("default DatabaseProvider is ready"))
	rr.Conditions.RecomputeHappiness()

	claimHealth := conditions.FindStatusCondition(service, ConditionClaimHealth)
	g.Expect(claimHealth).NotTo(BeNil())
	g.Expect(claimHealth.Status).To(Equal(metav1.ConditionFalse))

	degraded := conditions.FindStatusCondition(service, string(fwapi.ConditionTypeDegraded))
	g.Expect(degraded).NotTo(BeNil())
	g.Expect(degraded.Status).To(Equal(metav1.ConditionTrue))
	g.Expect(degraded.Reason).To(Equal("InputsUnhealthy"))

	ready := conditions.FindStatusCondition(service, string(fwapi.ConditionTypeReady))
	g.Expect(ready).NotTo(BeNil())
	g.Expect(ready.Status).To(Equal(metav1.ConditionTrue))
}

func TestAggregateInputsTreatsClaimsWithoutProvisionedAsPending(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	service := &servicesApi.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName},
	}
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "reachable-provider"},
		Status: infraApi.DatabaseProviderStatus{
			Status: fwapi.Status{
				Conditions: []fwapi.Condition{{
					Type:   "Reachable",
					Status: metav1.ConditionTrue,
					Reason: "ConnectionVerified",
				}},
			},
		},
	}
	claim := &infraApi.SchemaClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "fresh", Namespace: "team-a"},
	}

	m := NewModule(&moduleconfig.Config{})
	m.apiReader = serviceTestClient(t, service, provider, claim)
	rr := serviceTestRequest(t, serviceTestClient(t, service), service)

	g.Expect(m.aggregateInputs(ctx, rr)).To(Succeed())

	claimHealth := conditions.FindStatusCondition(service, ConditionClaimHealth)
	g.Expect(claimHealth).NotTo(BeNil())
	g.Expect(claimHealth.Status).To(Equal(metav1.ConditionUnknown))
	g.Expect(claimHealth.Reason).To(Equal("ClaimsPending"))
	g.Expect(claimHealth.Message).To(ContainSubstring("Waiting for 1 claim"))
	g.Expect(claimHealth.Message).NotTo(ContainSubstring("All DatabaseClaim and SchemaClaim resources are provisioned"))

	degraded := conditions.FindStatusCondition(service, string(fwapi.ConditionTypeDegraded))
	g.Expect(degraded).NotTo(BeNil())
	g.Expect(degraded.Status).To(Equal(metav1.ConditionFalse))
}

func TestAggregateInputsTreatsProviderBoundClaimsAsPending(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	service := &servicesApi.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName},
	}
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "reachable-provider"},
		Status: infraApi.DatabaseProviderStatus{
			Status: fwapi.Status{
				Conditions: []fwapi.Condition{{
					Type:   "Reachable",
					Status: metav1.ConditionTrue,
					Reason: "ConnectionVerified",
				}},
			},
		},
	}
	claim := &infraApi.DatabaseClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "analytics", Namespace: "app"},
		Status: infraApi.DatabaseClaimStatus{
			Status: fwapi.Status{
				Conditions: []fwapi.Condition{{
					Type:    dbcontroller.ConditionProvisioned,
					Status:  metav1.ConditionFalse,
					Reason:  dbcontroller.ReasonProviderBound,
					Message: `bound to DatabaseProvider "reachable-provider"; provisioning on next reconcile`,
				}},
			},
		},
	}

	m := NewModule(&moduleconfig.Config{})
	m.apiReader = serviceTestClient(t, service, provider, claim)
	rr := serviceTestRequest(t, serviceTestClient(t, service), service)

	g.Expect(m.aggregateInputs(ctx, rr)).To(Succeed())

	claimHealth := conditions.FindStatusCondition(service, ConditionClaimHealth)
	g.Expect(claimHealth).NotTo(BeNil())
	g.Expect(claimHealth.Status).To(Equal(metav1.ConditionUnknown))
	g.Expect(claimHealth.Reason).To(Equal("ClaimsPending"))
	g.Expect(claimHealth.Message).To(ContainSubstring("Waiting for 1 claim"))
	g.Expect(claimHealth.Message).NotTo(ContainSubstring("All DatabaseClaim and SchemaClaim resources are provisioned"))

	degraded := conditions.FindStatusCondition(service, string(fwapi.ConditionTypeDegraded))
	g.Expect(degraded).NotTo(BeNil())
	g.Expect(degraded.Status).To(Equal(metav1.ConditionFalse))
}

func TestAggregateInputsDegradedMessageWhenNoProvidersConfigured(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	service := &servicesApi.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName},
	}

	m := NewModule(&moduleconfig.Config{})
	m.apiReader = serviceTestClient(t, service)
	rr := serviceTestRequest(t, serviceTestClient(t, service), service)

	g.Expect(m.aggregateInputs(ctx, rr)).To(Succeed())

	degraded := conditions.FindStatusCondition(service, string(fwapi.ConditionTypeDegraded))
	g.Expect(degraded).NotTo(BeNil())
	g.Expect(degraded.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(degraded.Reason).To(Equal("NoProvidersConfigured"))
	g.Expect(degraded.Severity).To(Equal(fwapi.ConditionSeverityInfo))
	g.Expect(degraded.Message).To(ContainSubstring("No DatabaseProvider resources exist"))
}

func TestAggregateInputsPrunesDeletedClaimMetrics(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	service := &servicesApi.DatabaseService{
		ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName},
	}
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "reachable-provider"},
		Status: infraApi.DatabaseProviderStatus{
			Status: fwapi.Status{
				Conditions: []fwapi.Condition{{
					Type:   "Reachable",
					Status: metav1.ConditionTrue,
					Reason: "ConnectionVerified",
				}},
			},
		},
	}
	current := &infraApi.DatabaseClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "current", Namespace: "app"},
		Status: infraApi.DatabaseClaimStatus{
			Status: fwapi.Status{
				Conditions: []fwapi.Condition{{
					Type:   dbcontroller.ConditionProvisioned,
					Status: metav1.ConditionTrue,
					Reason: "UserProvisioned",
				}},
			},
		},
	}

	m := NewModule(&moduleconfig.Config{})
	m.apiReader = serviceTestClient(t, service, provider, current)
	rr := serviceTestRequest(t, serviceTestClient(t, service), service)

	t.Cleanup(func() { dbmetrics.PruneClaimMetrics(nil) })
	dbmetrics.PruneClaimMetrics(nil)
	dbmetrics.SetClaimProvisioned(infraApi.DatabaseClaimKind, "app", "removed", false)
	dbmetrics.SetClaimProvisioned(infraApi.DatabaseClaimKind, "app", "current", true)
	g.Expect(m.aggregateInputs(ctx, rr)).To(Succeed())
	g.Expect(testutil.GatherAndCompare(ctrlmetrics.Registry, strings.NewReader(`
# HELP dbclaim_provisioned Whether a DatabaseClaim or SchemaClaim is provisioned
# TYPE dbclaim_provisioned gauge
dbclaim_provisioned{kind="DatabaseClaim",name="current",namespace="app"} 1
`), "dbclaim_provisioned")).To(Succeed())
}

func serviceTestClientWithListError(
	t *testing.T,
	listErr error,
	objects ...client.Object,
) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		infraApi.AddToScheme,
		servicesApi.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}

	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&servicesApi.DatabaseService{}, &infraApi.DatabaseProvider{}).
		WithObjects(objects...).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(context.Context, client.WithWatch, client.ObjectList, ...client.ListOption) error {
				return listErr
			},
		}).
		Build()
}
