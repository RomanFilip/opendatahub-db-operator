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

package databaseclaim

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/gomega"
	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	odherrors "github.com/opendatahub-io/odh-platform-utilities/framework/controller/actions/errors"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	odhtypes "github.com/opendatahub-io/odh-platform-utilities/framework/controller/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/controller/claimerrors"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
	secretvalue "github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

func TestProvisionAction_PersistsProviderBindingBeforeEnsure(t *testing.T) {
	g := NewWithT(t)

	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "rhai-db"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeInternal,
			Internal: &infraApi.InternalProviderSpec{
				Namespace: "database-system",
			},
		},
		Status: infraApi.DatabaseProviderStatus{
			Status: fwapi.Status{
				Conditions: []fwapi.Condition{{
					Type:   "Reachable",
					Status: metav1.ConditionTrue,
				}},
			},
		},
	}
	claim := &infraApi.DatabaseClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "analytics", Namespace: "app"},
		Spec: infraApi.DatabaseClaimSpec{
			Provider: infraApi.ProviderRef{Name: "rhai-db"},
			Database: "analytics",
		},
	}

	cli := fake.NewClientBuilder().WithScheme(claimTestScheme(t)).WithObjects(provider, claim).Build()
	rr := claimTestRequest(t, cli, claim)
	m := &Controller{Options: Options{cfg: &moduleconfig.Config{}}}

	err := m.provisionAction(context.Background(), rr)
	g.Expect(err).To(HaveOccurred())
	var stop odherrors.StopError
	g.Expect(errors.As(err, &stop)).To(BeTrue())
	g.Expect(stop.RequeueAfter()).To(Equal(providerBindingRequeueAfter))
	g.Expect(claim.Status.Provider).To(Equal("rhai-db"))
	provisioned := conditions.FindStatusCondition(claim, ConditionProvisioned)
	g.Expect(provisioned).NotTo(BeNil())
	g.Expect(provisioned.Reason).To(Equal("ProviderBound"))
}

// TestProvisionAction_ClientFailureStaysSafeEverywhere covers the shapes a
// failed admin connection reaches: the condition messages and the value the
// reconciler returns (which the framework logs and records as an Event). The
// client projects the driver error; no caller re-renders or scrubs it.
func TestProvisionAction_ClientFailureStaysSafeEverywhere(t *testing.T) {
	g := NewWithT(t)

	const sentinel = "admin-pw-sentinel"

	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "sample-external"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeExternal,
			External: &infraApi.ExternalProviderSpec{
				ConnectionSecretRef: corev1.SecretReference{Namespace: "db-admin", Name: "external-admin"},
			},
		},
		Status: infraApi.DatabaseProviderStatus{
			Status: fwapi.Status{Conditions: []fwapi.Condition{{Type: "Reachable", Status: metav1.ConditionTrue}}},
		},
	}
	adminSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-admin", Namespace: "db-admin"},
		Data: map[string][]byte{
			postgres.SecretKeyHost:     []byte("postgres.db-admin.svc"),
			postgres.SecretKeyPort:     []byte("5432"),
			postgres.SecretKeyUser:     []byte("postgres"),
			postgres.SecretKeyPassword: []byte(sentinel),
			postgres.SecretKeyDatabase: []byte("postgres"),
			postgres.SecretKeySSLMode:  []byte(postgres.SSLModeDisable),
		},
	}
	claim := &infraApi.DatabaseClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "analytics", Namespace: "app"},
		Spec: infraApi.DatabaseClaimSpec{
			Provider: infraApi.ProviderRef{Name: "sample-external"},
			Database: "analytics",
		},
		Status: infraApi.DatabaseClaimStatus{Provider: "sample-external"},
	}

	cli := fake.NewClientBuilder().WithScheme(claimTestScheme(t)).WithObjects(provider, adminSecret, claim).Build()
	m := &Controller{Options: Options{
		cfg: &moduleconfig.Config{OperatorNamespace: "odh-db-operator-system"},
		PostgresClientFactory: func(_ context.Context, _ postgres.Config) (postgres.Client, error) {
			return nil, postgres.ProjectError(
				"ping",
				fmt.Errorf("authentication failed for password %s", sentinel),
				secretvalue.FromString(sentinel),
			)
		},
	}}

	err := m.provisionAction(context.Background(), claimTestRequest(t, cli, claim))
	g.Expect(err).To(HaveOccurred())
	g.Expect(err.Error()).To(ContainSubstring("[REDACTED]"))
	g.Expect(err.Error()).NotTo(ContainSubstring(sentinel))
	g.Expect(postgres.RetryClassOf(err)).To(Equal(postgres.RetryNone))

	for _, conditionType := range []string{ConditionProvisioned, ConditionTLSConfiguration} {
		condition := conditions.FindStatusCondition(claim, conditionType)
		g.Expect(condition).NotTo(BeNil(), conditionType)
		g.Expect(condition.Message).NotTo(ContainSubstring(sentinel), conditionType)
	}
}

func TestProvisionAction_ExtensionNotAvailable(t *testing.T) {
	g := NewWithT(t)

	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "sample-external"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeExternal,
			External: &infraApi.ExternalProviderSpec{
				ConnectionSecretRef: corev1.SecretReference{
					Namespace: "db-admin",
					Name:      "external-admin",
				},
			},
		},
		Status: infraApi.DatabaseProviderStatus{
			Status: fwapi.Status{
				Conditions: []fwapi.Condition{{
					Type:   "Reachable",
					Status: metav1.ConditionTrue,
				}},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-admin", Namespace: "db-admin"},
		Data: map[string][]byte{
			postgres.SecretKeyHost:     []byte("postgres.db-admin.svc"),
			postgres.SecretKeyPort:     []byte("5432"),
			postgres.SecretKeyUser:     []byte("postgres"),
			postgres.SecretKeyPassword: []byte("secret"),
			postgres.SecretKeyDatabase: []byte("postgres"),
		},
	}
	claim := &infraApi.DatabaseClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "analytics", Namespace: "app"},
		Spec: infraApi.DatabaseClaimSpec{
			Provider:   infraApi.ProviderRef{Name: "sample-external"},
			Database:   "analytics",
			Extensions: []string{"vector"},
		},
		Status: infraApi.DatabaseClaimStatus{Provider: "sample-external"},
	}
	pg := &extensionProvisionPG{
		databases:          map[string]bool{"analytics": true},
		extensionAvailable: map[string]bool{"vector": false},
	}
	cli := fake.NewClientBuilder().WithScheme(claimTestScheme(t)).WithObjects(provider, secret, claim).Build()
	m := &Controller{Options: Options{
		cfg: &moduleconfig.Config{OperatorNamespace: "odh-db-operator-system"},
		PostgresClientFactory: func(_ context.Context, _ postgres.Config) (postgres.Client, error) {
			return pg, nil
		},
	}}

	err := m.provisionAction(context.Background(), claimTestRequest(t, cli, claim))
	g.Expect(err).To(HaveOccurred())
	var stop odherrors.StopError
	g.Expect(errors.As(err, &stop)).To(BeTrue())
	g.Expect(stop.RequeueAfter()).To(Equal(moduleconfig.DefaultRetryInterval))
	var missingExt claimerrors.ExtensionNotAvailable
	g.Expect(errors.As(err, &missingExt)).To(BeTrue())

	provisioned := conditions.FindStatusCondition(claim, ConditionProvisioned)
	g.Expect(provisioned).NotTo(BeNil())
	g.Expect(provisioned.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(provisioned.Reason).To(Equal("ExtensionNotAvailable"))
	g.Expect(provisioned.Message).To(ContainSubstring("vector"))
	g.Expect(pg.execSQLsContainingCreateExtension()).To(BeFalse())
}

func TestProvisionAction_UnsupportedExtensionNeverOpensPostgres(t *testing.T) {
	g := NewWithT(t)

	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "sample-external"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeExternal,
			External: &infraApi.ExternalProviderSpec{
				ConnectionSecretRef: corev1.SecretReference{
					Namespace: "db-admin",
					Name:      "external-admin",
				},
			},
		},
		Status: infraApi.DatabaseProviderStatus{
			Status: fwapi.Status{
				Conditions: []fwapi.Condition{{
					Type:   "Reachable",
					Status: metav1.ConditionTrue,
				}},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "external-admin", Namespace: "db-admin"},
		Data: map[string][]byte{
			postgres.SecretKeyHost:     []byte("postgres.db-admin.svc"),
			postgres.SecretKeyPort:     []byte("5432"),
			postgres.SecretKeyUser:     []byte("postgres"),
			postgres.SecretKeyPassword: []byte("secret"),
			postgres.SecretKeyDatabase: []byte("postgres"),
		},
	}
	claim := &infraApi.DatabaseClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "analytics", Namespace: "app"},
		Spec: infraApi.DatabaseClaimSpec{
			Provider:   infraApi.ProviderRef{Name: "sample-external"},
			Database:   "analytics",
			Extensions: []string{"postgis"},
		},
		Status: infraApi.DatabaseClaimStatus{Provider: "sample-external"},
	}
	cli := fake.NewClientBuilder().WithScheme(claimTestScheme(t)).WithObjects(provider, secret, claim).Build()
	factoryCalls := 0
	m := &Controller{Options: Options{
		cfg: &moduleconfig.Config{OperatorNamespace: "odh-db-operator-system"},
		PostgresClientFactory: func(_ context.Context, _ postgres.Config) (postgres.Client, error) {
			factoryCalls++
			return nil, fmt.Errorf("factory must not be called for unsupported extensions")
		},
	}}

	err := m.provisionAction(context.Background(), claimTestRequest(t, cli, claim))
	g.Expect(err).To(HaveOccurred())
	var stop odherrors.StopError
	g.Expect(errors.As(err, &stop)).To(BeTrue())
	var unsupported claimerrors.ExtensionNotSupported
	g.Expect(errors.As(err, &unsupported)).To(BeTrue())
	g.Expect(unsupported.Extensions).To(Equal([]string{"postgis"}))
	g.Expect(factoryCalls).To(Equal(0))

	provisioned := conditions.FindStatusCondition(claim, ConditionProvisioned)
	g.Expect(provisioned).NotTo(BeNil())
	g.Expect(provisioned.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(provisioned.Reason).To(Equal("ExtensionNotSupported"))
	g.Expect(provisioned.Message).To(ContainSubstring(`extension "postgis" is not supported`))
}

func TestSchemaClaimAPIUnavailable(t *testing.T) {
	g := NewWithT(t)
	schemaclaimGVK := schema.GroupVersionKind{
		Group:   infraApi.GroupName,
		Version: "v1alpha1",
		Kind:    "SchemaClaim",
	}

	g.Expect(schemaClaimAPIUnavailable(&meta.NoKindMatchError{GroupKind: schemaclaimGVK.GroupKind()})).
		To(BeTrue(), "NoKindMatchError means the SchemaClaim CRD is absent")
	g.Expect(schemaClaimAPIUnavailable(apierrors.NewNotFound(
		schema.GroupResource{Group: infraApi.GroupName, Resource: "schemaclaims"},
		"",
	))).To(BeTrue(), "NotFound is returned by some discovery paths when the API is missing")
	g.Expect(schemaClaimAPIUnavailable(fmt.Errorf(
		"listing schemaclaims: %w",
		apierrors.NewNotFound(schema.GroupResource{Group: infraApi.GroupName, Resource: "schemaclaims"}, ""),
	))).To(BeTrue(), "wrapped NotFound must unwrap so cleanup is not stuck")
	g.Expect(schemaClaimAPIUnavailable(errors.New("failed to get API group resources"))).
		To(BeFalse(), "English discovery wording alone is not a typed unavailable signal")
	g.Expect(schemaClaimAPIUnavailable(errors.New("Unauthorized"))).To(BeFalse())
	g.Expect(schemaClaimAPIUnavailable(apierrors.NewForbidden(
		schema.GroupResource{Group: infraApi.GroupName, Resource: "schemaclaims"},
		"",
		errors.New("no RBAC"),
	))).To(BeFalse(), "Forbidden is a real misconfiguration and should retry")
}

func TestCleanupAction_DeletesCredentialsSecret(t *testing.T) {
	g := NewWithT(t)
	claim, provider, adminSecret := cleanupFixtures()
	credSecret := ownedCredentialsSecret(t, claim)
	cli := fakeCleanupClient(t, provider, adminSecret, claim, credSecret)

	err := newCleanupController(&cleanupPG{databases: map[string]bool{"analytics": true}}).cleanupAction(
		context.Background(),
		claimTestRequest(t, cli, claim),
	)
	g.Expect(err).NotTo(HaveOccurred())

	got := &corev1.Secret{}
	err = cli.Get(context.Background(), client.ObjectKeyFromObject(credSecret), got)
	g.Expect(err).To(HaveOccurred())
	g.Expect(client.IgnoreNotFound(err)).To(Succeed())
}

func TestCleanupAction_EmptyProviderStillDeletesCredentialsSecret(t *testing.T) {
	g := NewWithT(t)
	claim, _, _ := cleanupFixtures()
	claim.Status.Provider = ""
	credSecret := ownedCredentialsSecret(t, claim)
	cli := fakeCleanupClient(t, claim, credSecret)

	err := newCleanupController(&cleanupPG{}).cleanupAction(
		context.Background(),
		claimTestRequest(t, cli, claim),
	)
	g.Expect(err).NotTo(HaveOccurred())

	got := &corev1.Secret{}
	err = cli.Get(context.Background(), client.ObjectKeyFromObject(credSecret), got)
	g.Expect(client.IgnoreNotFound(err)).To(Succeed())
}

func TestCleanupAction_DeletesStampedSecretWithStrippedOwnerRef(t *testing.T) {
	g := NewWithT(t)
	claim, provider, adminSecret := cleanupFixtures()
	role := dbcontroller.RoleNameFor(claim.Namespace, claim.Name)
	credSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      dbcontroller.SecretNameForDatabaseClaim(claim),
		Namespace: claim.Namespace,
	}}
	dbcontroller.ApplyClaimCredentials(credSecret, postgres.Config{}, role, secretvalue.FromString("pw"), "analytics", claim.UID)
	cli := fakeCleanupClient(t, provider, adminSecret, claim, credSecret)

	err := newCleanupController(&cleanupPG{databases: map[string]bool{"analytics": true}}).cleanupAction(
		context.Background(),
		claimTestRequest(t, cli, claim),
	)
	g.Expect(err).NotTo(HaveOccurred())

	got := &corev1.Secret{}
	err = cli.Get(context.Background(), client.ObjectKeyFromObject(credSecret), got)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(),
		"a stamp-verified ownerless Secret must be cleaned up, got: %v", err)
}

func TestCleanupAction_SkipsForeignOwnedSecretWithForgedStamp(t *testing.T) {
	g := NewWithT(t)
	claim, provider, adminSecret := cleanupFixtures()
	// A Secret controller-owned by a different claim, carrying a stamp forged
	// for this claim: the claim UID is public, the role derivable, and the
	// password readable, so an actor with Secret-write can produce it. The
	// ownership rule must reject it regardless of the stamp.
	role := dbcontroller.RoleNameFor(claim.Namespace, claim.Name)
	controller := true
	credSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      dbcontroller.SecretNameForDatabaseClaim(claim),
		Namespace: claim.Namespace,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: infraApi.GroupVersion.String(),
			Kind:       infraApi.SchemaClaimKind,
			Name:       "sc-claim-a",
			UID:        types.UID("schemaclaim-uid"),
			Controller: &controller,
		}},
	}}
	dbcontroller.ApplyClaimCredentials(credSecret, postgres.Config{}, role, secretvalue.FromString("forged-pw"), "analytics", claim.UID)
	cli := fakeCleanupClient(t, provider, adminSecret, claim, credSecret)

	err := newCleanupController(&cleanupPG{databases: map[string]bool{"analytics": true}}).cleanupAction(
		context.Background(),
		claimTestRequest(t, cli, claim),
	)
	g.Expect(err).NotTo(HaveOccurred())

	got := &corev1.Secret{}
	g.Expect(cli.Get(context.Background(), client.ObjectKeyFromObject(credSecret), got)).To(Succeed(),
		"a Secret owned by another controller must never be deleted, forged stamp or not")
}

func TestCleanupAction_SkipsCredentialsSecretOwnedByOtherClaim(t *testing.T) {
	g := NewWithT(t)
	claim, provider, adminSecret := cleanupFixtures()
	controller := true
	credSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dbcontroller.SecretNameForDatabaseClaim(claim),
			Namespace: claim.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: infraApi.GroupVersion.String(),
				Kind:       infraApi.SchemaClaimKind,
				Name:       "foo",
				UID:        types.UID("other-claim-uid"),
				Controller: &controller,
			}},
		},
	}
	cli := fakeCleanupClient(t, provider, adminSecret, claim, credSecret)

	err := newCleanupController(&cleanupPG{databases: map[string]bool{"analytics": true}}).cleanupAction(
		context.Background(),
		claimTestRequest(t, cli, claim),
	)
	g.Expect(err).NotTo(HaveOccurred())

	got := &corev1.Secret{}
	g.Expect(cli.Get(context.Background(), client.ObjectKeyFromObject(credSecret), got)).To(Succeed())
}

type extensionProvisionPG struct {
	databases          map[string]bool
	extensionAvailable map[string]bool
	execSQLs           []string
}

func (p *extensionProvisionPG) Config() postgres.Config { return postgres.Config{} }
func (p *extensionProvisionPG) Close()                  {}
func (p *extensionProvisionPG) Ping(context.Context) error {
	return nil
}

func (p *extensionProvisionPG) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	p.execSQLs = append(p.execSQLs, sql)
	return pgconn.CommandTag{}, nil
}

func (p *extensionProvisionPG) Query(context.Context, string, ...any) (postgres.Rows, error) {
	return nil, fmt.Errorf("unexpected Query")
}

func (p *extensionProvisionPG) QueryRow(_ context.Context, query string, args ...any) (pgx.Row, error) {
	if p.databases == nil {
		p.databases = map[string]bool{}
	}
	switch {
	case strings.Contains(query, "pg_database"):
		name, _ := args[0].(string)
		return extensionBoolRow{p.databases[name]}, nil
	case strings.Contains(query, "pg_extension"):
		if names, ok := args[0].([]string); ok {
			var installed []string
			for _, name := range names {
				if p.extensionAvailable[name] {
					installed = append(installed, name)
				}
			}
			return extensionNamesRow{names: installed}, nil
		}
		name, _ := args[0].(string)
		installed := false
		if p.extensionAvailable != nil {
			installed = p.extensionAvailable[name]
		}
		return extensionBoolRow{installed}, nil
	default:
		return extensionBoolRow{false}, nil
	}
}

type extensionNamesRow struct{ names []string }

func (r extensionNamesRow) Scan(dest ...any) error {
	ptr, ok := dest[0].(*[]string)
	if !ok {
		return fmt.Errorf("expected *[]string")
	}
	*ptr = append([]string(nil), r.names...)
	return nil
}

func (p *extensionProvisionPG) execSQLsContainingCreateExtension() bool {
	for _, sql := range p.execSQLs {
		if strings.Contains(strings.ToUpper(sql), "CREATE EXTENSION") {
			return true
		}
	}
	return false
}

type extensionBoolRow struct{ v bool }

func (r extensionBoolRow) Scan(dest ...any) error {
	ptr, ok := dest[0].(*bool)
	if !ok {
		return fmt.Errorf("expected *bool")
	}
	*ptr = r.v
	return nil
}

func claimTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	g := NewWithT(t)
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(infraApi.AddToScheme(scheme)).To(Succeed())
	return scheme
}

func claimTestRequest(t *testing.T, cli client.Client, claim *infraApi.DatabaseClaim) *odhtypes.ReconciliationRequest {
	t.Helper()
	aggregator, err := conditions.NewAggregator(
		fwapi.ConditionTypeReady,
		conditions.Dependent(fwapi.ConditionType(ConditionProvisioned), conditions.HealthyWhenTrue),
		conditions.Dependent(fwapi.ConditionType(ConditionTLSConfiguration), conditions.HealthyWhenTrue),
	)
	if err != nil {
		t.Fatal(err)
	}
	return &odhtypes.ReconciliationRequest{
		Client:     cli,
		Conditions: conditions.NewManager(claim, aggregator),
		Instance:   claim,
	}
}

func newCleanupController(pg *cleanupPG) *Controller {
	return &Controller{Options: Options{
		cfg: &moduleconfig.Config{OperatorNamespace: "odh-db-operator-system"},
		PostgresClientFactory: func(_ context.Context, _ postgres.Config) (postgres.Client, error) {
			return pg, nil
		},
	}}
}

func cleanupFixtures() (*infraApi.DatabaseClaim, *infraApi.DatabaseProvider, *corev1.Secret) {
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "rhai-db"},
		Spec: infraApi.DatabaseProviderSpec{
			Type:     infraApi.ProviderTypeInternal,
			Internal: &infraApi.InternalProviderSpec{Namespace: "database-system"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dbcontroller.InternalAdminSecretName(provider.Name),
			Namespace: "database-system",
		},
		Data: map[string][]byte{
			postgres.SecretKeyHost:     []byte("rhai-db.database-system.svc"),
			postgres.SecretKeyPort:     []byte("5432"),
			postgres.SecretKeyUser:     []byte("postgres"),
			postgres.SecretKeyPassword: []byte("postgres"),
			postgres.SecretKeyDatabase: []byte("postgres"),
		},
	}
	claim := &infraApi.DatabaseClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "analytics",
			Namespace: "app",
			UID:       types.UID("database-claim-uid"),
		},
		Spec: infraApi.DatabaseClaimSpec{
			Provider:       infraApi.ProviderRef{Name: "rhai-db"},
			Database:       "analytics",
			DeletionPolicy: infraApi.DeletionPolicyRetain,
		},
		Status: infraApi.DatabaseClaimStatus{
			Provider: "rhai-db",
			Database: "analytics",
			Connection: infraApi.DatabaseConnectionStatus{
				Database: "analytics",
			},
		},
	}
	return claim, provider, secret
}

func ownedCredentialsSecret(t *testing.T, claim *infraApi.DatabaseClaim) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dbcontroller.SecretNameForDatabaseClaim(claim),
			Namespace: claim.Namespace,
		},
	}
	g := NewWithT(t)
	g.Expect(controllerutil.SetControllerReference(claim, secret, claimTestScheme(t))).To(Succeed())
	return secret
}

func fakeCleanupClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(claimTestScheme(t)).WithObjects(objs...).Build()
}

// cleanupPG is a minimal postgres.Client fake for cleanupAction paths.
type cleanupPG struct {
	databases    map[string]bool
	roles        map[string]bool
	droppedRoles []string
}

func (t *cleanupPG) Config() postgres.Config { return postgres.Config{} }
func (t *cleanupPG) Close()                  {}
func (t *cleanupPG) Ping(context.Context) error {
	return nil
}

func (t *cleanupPG) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	if strings.Contains(query, "DROP ROLE") {
		start := strings.Index(query, `"`)
		end := strings.LastIndex(query, `"`)
		if start >= 0 && end > start {
			name := query[start+1 : end]
			delete(t.roles, name)
			t.droppedRoles = append(t.droppedRoles, name)
		}
	}
	return pgconn.CommandTag{}, nil
}

func (t *cleanupPG) Query(context.Context, string, ...any) (postgres.Rows, error) {
	return nil, fmt.Errorf("unexpected Query")
}

func (t *cleanupPG) QueryRow(_ context.Context, query string, args ...any) (pgx.Row, error) {
	if t.databases == nil {
		t.databases = map[string]bool{}
	}
	if t.roles == nil {
		t.roles = map[string]bool{}
	}
	name, _ := args[0].(string)
	switch {
	case strings.Contains(query, "pg_database"):
		return cleanupBoolRow{t.databases[name]}, nil
	case strings.Contains(query, "pg_roles"):
		return cleanupBoolRow{t.roles[name]}, nil
	default:
		return cleanupBoolRow{false}, nil
	}
}

type cleanupBoolRow struct{ v bool }

func (r cleanupBoolRow) Scan(dest ...any) error {
	ptr, ok := dest[0].(*bool)
	if !ok {
		return fmt.Errorf("expected *bool")
	}
	*ptr = r.v
	return nil
}
