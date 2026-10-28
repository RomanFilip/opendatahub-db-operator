package databaseservice

import (
	"context"
	"errors"
	"strings"
	"testing"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	odherrors "github.com/opendatahub-io/odh-platform-utilities/framework/controller/actions/errors"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	odhtypes "github.com/opendatahub-io/odh-platform-utilities/framework/controller/types"
	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	servicesApi "github.com/opendatahub-io/opendatahub-db-operator/api/services/v1alpha1"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func serviceTestClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, infraApi.AddToScheme, servicesApi.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&servicesApi.DatabaseService{}, &infraApi.DatabaseProvider{}).WithObjects(objects...).Build()
}

func serviceTestRequest(t *testing.T, cli client.Client, service *servicesApi.DatabaseService) *odhtypes.ReconciliationRequest {
	t.Helper()
	aggregator, err := conditions.NewAggregator(fwapi.ConditionTypeReady,
		conditions.Dependent(conditionDefaultProviderReady, conditions.HealthyWhenTrue))
	if err != nil {
		t.Fatal(err)
	}
	return &odhtypes.ReconciliationRequest{Client: cli, Instance: service, Conditions: conditions.NewManager(service, aggregator)}
}

func TestDefaultProviderCreationAndImmutableName(t *testing.T) {
	ctx := context.Background()
	service := &servicesApi.DatabaseService{ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName}}
	cli := serviceTestClient(t, service)
	m := NewModule(&moduleconfig.Config{DefaultProviderName: "custom-db"})
	rr := serviceTestRequest(t, cli, service)

	err := m.ensureDefaultProvider(ctx, rr)
	var stop odherrors.StopError
	if !errors.As(err, &stop) || service.Status.DefaultProviderName != "custom-db" {
		t.Fatalf("initialization should persist the configured name before creation: status=%q err=%v", service.Status.DefaultProviderName, err)
	}
	if err := m.ensureDefaultProvider(ctx, rr); err != nil {
		t.Fatal(err)
	}
	provider := &infraApi.DatabaseProvider{}
	if err := cli.Get(ctx, client.ObjectKey{Name: "custom-db"}, provider); err != nil {
		t.Fatal(err)
	}
	if provider.Spec.Type != infraApi.ProviderTypeInternal || provider.Spec.Internal.Storage.Size.String() != "20Gi" {
		t.Fatalf("wrong default provider spec: %+v", provider.Spec)
	}

	m.cfg.DefaultProviderName = "changed-db"
	err = m.ensureDefaultProvider(ctx, rr)
	if !errors.As(err, &stop) || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("name change should be blocked: %v", err)
	}
	if err := cli.Get(ctx, client.ObjectKey{Name: "changed-db"}, &infraApi.DatabaseProvider{}); !apierrors.IsNotFound(err) {
		t.Fatalf("name change created a second provider: %v", err)
	}
}

func TestDefaultProviderNameCollision(t *testing.T) {
	ctx := context.Background()
	service := &servicesApi.DatabaseService{ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName}, Status: servicesApi.DatabaseServiceStatus{DefaultProviderName: "rhai-db"}}
	other := &infraApi.DatabaseProvider{ObjectMeta: metav1.ObjectMeta{Name: "rhai-db"}}
	cli := serviceTestClient(t, service, other)
	err := NewModule(&moduleconfig.Config{}).ensureDefaultProvider(ctx, serviceTestRequest(t, cli, service))
	if err == nil || !strings.Contains(err.Error(), "not this service") {
		t.Fatalf("existing provider collision should be reported: %v", err)
	}
}

func TestDefaultProviderDeletionBlockedByClaims(t *testing.T) {
	ctx := context.Background()
	service := &servicesApi.DatabaseService{ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName}, Status: servicesApi.DatabaseServiceStatus{DefaultProviderName: "rhai-db"}}
	provider := &infraApi.DatabaseProvider{ObjectMeta: metav1.ObjectMeta{Name: "rhai-db", Annotations: map[string]string{defaultProviderOwnerAnnotation: service.Name}}, Spec: infraApi.DatabaseProviderSpec{Type: infraApi.ProviderTypeInternal}}
	claim := &infraApi.DatabaseClaim{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team"}, Status: infraApi.DatabaseClaimStatus{Provider: "rhai-db"}}
	cli := serviceTestClient(t, service, provider, claim)
	err := NewModule(&moduleconfig.Config{}).cleanupDefaultProvider(ctx, serviceTestRequest(t, cli, service))
	if err == nil || !strings.Contains(err.Error(), "DatabaseClaim team/app") {
		t.Fatalf("bound claim should block deletion: %v", err)
	}
	if err := cli.Get(ctx, client.ObjectKey{Name: provider.Name}, &infraApi.DatabaseProvider{}); err != nil {
		t.Fatalf("blocked provider was deleted: %v", err)
	}
	got := &servicesApi.DatabaseService{}
	if err := cli.Get(ctx, client.ObjectKey{Name: service.Name}, got); err != nil {
		t.Fatal(err)
	}
	condition := conditions.FindStatusCondition(got, conditionDefaultProviderReady)
	if condition == nil || condition.Reason != "BlockingClaims" {
		t.Fatalf("blocking condition not reported: %+v", condition)
	}
}

func TestDefaultProviderDeletionBlockedByUnresolvedClaims(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  string
		claim client.Object
	}{
		{name: "DatabaseClaim without provider", kind: "DatabaseClaim", claim: &infraApi.DatabaseClaim{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team"}}},
		{name: "SchemaClaim without provider", kind: "SchemaClaim", claim: &infraApi.SchemaClaim{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team"}}},
		{name: "DatabaseClaim with matching selector", kind: "DatabaseClaim", claim: &infraApi.DatabaseClaim{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team"}, Spec: infraApi.DatabaseClaimSpec{Provider: infraApi.ProviderRef{Selector: &metav1.LabelSelector{}}}}},
		{name: "SchemaClaim with matching selector", kind: "SchemaClaim", claim: &infraApi.SchemaClaim{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team"}, Spec: infraApi.SchemaClaimSpec{Provider: infraApi.ProviderRef{Selector: &metav1.LabelSelector{}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service := &servicesApi.DatabaseService{ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName}, Status: servicesApi.DatabaseServiceStatus{DefaultProviderName: "rhai-db"}}
			provider := &infraApi.DatabaseProvider{ObjectMeta: metav1.ObjectMeta{Name: "rhai-db", Annotations: map[string]string{defaultProviderOwnerAnnotation: service.Name}}, Spec: infraApi.DatabaseProviderSpec{Type: infraApi.ProviderTypeInternal}}
			cli := serviceTestClient(t, service, provider, tc.claim)
			err := NewModule(&moduleconfig.Config{}).cleanupDefaultProvider(ctx, serviceTestRequest(t, cli, service))
			if err == nil || !strings.Contains(err.Error(), tc.kind+" team/app") {
				t.Fatalf("unresolved claim should block deletion: %v", err)
			}
			if err := cli.Get(ctx, client.ObjectKey{Name: provider.Name}, &infraApi.DatabaseProvider{}); err != nil {
				t.Fatalf("blocked provider was deleted: %v", err)
			}
		})
	}
}

func TestClaimUsesProviderWithSelector(t *testing.T) {
	provider := &infraApi.DatabaseProvider{ObjectMeta: metav1.ObjectMeta{Name: "rhai-db", Labels: map[string]string{"team": "platform"}}}
	for _, tc := range []struct {
		name     string
		selector *metav1.LabelSelector
		want     bool
	}{
		{name: "matching labels", selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "platform"}}, want: true},
		{name: "nonmatching labels", selector: &metav1.LabelSelector{MatchLabels: map[string]string{"team": "other"}}},
		{name: "matching expression", selector: &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "team", Operator: metav1.LabelSelectorOpIn, Values: []string{"platform"}}}}, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usesProvider, err := claimUsesProvider(infraApi.ProviderRef{Selector: tc.selector}, "", provider.Name, provider)
			if err != nil || usesProvider != tc.want {
				t.Fatalf("selector uses default provider: got %t, want %t, err %v", usesProvider, tc.want, err)
			}
		})
	}
}

func TestDefaultProviderDeletionLeavesForeignProvider(t *testing.T) {
	ctx := context.Background()
	service := &servicesApi.DatabaseService{ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName}, Status: servicesApi.DatabaseServiceStatus{DefaultProviderName: "rhai-db"}}
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "rhai-db"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeExternal,
			External: &infraApi.ExternalProviderSpec{
				ConnectionSecretRef: corev1.SecretReference{Name: "external-admin", Namespace: "team"},
			},
		},
	}
	claim := &infraApi.DatabaseClaim{ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "team"}, Spec: infraApi.DatabaseClaimSpec{Provider: infraApi.ProviderRef{Name: provider.Name}}, Status: infraApi.DatabaseClaimStatus{Provider: provider.Name}}
	cli := serviceTestClient(t, service, provider, claim)
	if err := NewModule(&moduleconfig.Config{}).cleanupDefaultProvider(ctx, serviceTestRequest(t, cli, service)); err != nil {
		t.Fatalf("foreign provider should not block DatabaseService deletion: %v", err)
	}
	if err := cli.Get(ctx, client.ObjectKey{Name: provider.Name}, &infraApi.DatabaseProvider{}); err != nil {
		t.Fatalf("foreign provider was deleted: %v", err)
	}
}

func TestDefaultProviderDeletionRejectsOwnedExternalProvider(t *testing.T) {
	ctx := context.Background()
	service := &servicesApi.DatabaseService{ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName}, Status: servicesApi.DatabaseServiceStatus{DefaultProviderName: "rhai-db"}}
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{
			Name: "rhai-db", Annotations: map[string]string{defaultProviderOwnerAnnotation: service.Name},
		},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeExternal,
			External: &infraApi.ExternalProviderSpec{
				ConnectionSecretRef: corev1.SecretReference{Name: "external-admin", Namespace: "team"},
			},
		},
	}
	cli := serviceTestClient(t, service, provider)
	err := NewModule(&moduleconfig.Config{}).cleanupDefaultProvider(ctx, serviceTestRequest(t, cli, service))
	if err == nil || !strings.Contains(err.Error(), "owned by this DatabaseService but is not Internal") {
		t.Fatalf("owned External provider should block DatabaseService deletion: %v", err)
	}
	if err := cli.Get(ctx, client.ObjectKey{Name: provider.Name}, &infraApi.DatabaseProvider{}); err != nil {
		t.Fatalf("owned External provider was deleted: %v", err)
	}
}

func TestDefaultProviderDeletionRetainsDataAndCredentials(t *testing.T) {
	ctx := context.Background()
	uid := types.UID("provider-uid")
	service := &servicesApi.DatabaseService{ObjectMeta: metav1.ObjectMeta{Name: servicesApi.DatabaseServiceInstanceName}, Status: servicesApi.DatabaseServiceStatus{DefaultProviderName: "rhai-db"}}
	provider := &infraApi.DatabaseProvider{ObjectMeta: metav1.ObjectMeta{Name: "rhai-db", UID: uid, Annotations: map[string]string{defaultProviderOwnerAnnotation: service.Name}}, Spec: infraApi.DatabaseProviderSpec{Type: infraApi.ProviderTypeInternal}}
	owner := metav1.OwnerReference{APIVersion: infraApi.SchemeGroupVersion.String(), Kind: "DatabaseProvider", Name: provider.Name, UID: uid}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "rhai-db", Namespace: "operator", OwnerReferences: []metav1.OwnerReference{owner}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "rhai-db-admin", Namespace: "operator", OwnerReferences: []metav1.OwnerReference{owner}}}
	cli := serviceTestClient(t, service, provider, pvc, secret)
	m := NewModule(&moduleconfig.Config{OperatorNamespace: "operator"})
	rr := serviceTestRequest(t, cli, service)
	if err := m.cleanupDefaultProvider(ctx, rr); err == nil || !strings.Contains(err.Error(), "waiting") {
		t.Fatalf("deletion should wait for the provider deletion timestamp: %v", err)
	}
	if err := m.cleanupDefaultProvider(ctx, rr); err == nil || !strings.Contains(err.Error(), "waiting") {
		t.Fatalf("cleanup should wait until the provider is absent: %v", err)
	}
	if err := m.cleanupDefaultProvider(ctx, rr); err != nil {
		t.Fatal(err)
	}
	for _, obj := range []client.Object{&corev1.PersistentVolumeClaim{}, &corev1.Secret{}} {
		key := client.ObjectKey{Namespace: "operator", Name: "rhai-db"}
		if _, ok := obj.(*corev1.Secret); ok {
			key.Name += "-admin"
		}
		if err := cli.Get(ctx, key, obj); err != nil || len(obj.GetOwnerReferences()) != 0 {
			t.Fatalf("data resource %s was not retained: err=%v refs=%v", key, err, obj.GetOwnerReferences())
		}
	}
}
