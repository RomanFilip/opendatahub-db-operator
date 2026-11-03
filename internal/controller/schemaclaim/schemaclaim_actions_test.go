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

package schemaclaim

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/gomega"
	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	odherrors "github.com/opendatahub-io/odh-platform-utilities/framework/controller/actions/errors"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	odhtypes "github.com/opendatahub-io/odh-platform-utilities/framework/controller/types"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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
	claim := &infraApi.SchemaClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "analytics", Namespace: "app"},
		Spec: infraApi.SchemaClaimSpec{
			Provider: infraApi.ProviderRef{Name: "rhai-db"},
			Schema:   "app_schema",
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
	claim := &infraApi.SchemaClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "analytics", Namespace: "app"},
		Spec: infraApi.SchemaClaimSpec{
			Provider:   infraApi.ProviderRef{Name: "sample-external"},
			Schema:     "app_schema",
			Database:   "analytics",
			Extensions: []string{"vector"},
		},
		Status: infraApi.SchemaClaimStatus{Provider: "sample-external"},
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
	claim := &infraApi.SchemaClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "analytics", Namespace: "app"},
		Spec: infraApi.SchemaClaimSpec{
			Provider:   infraApi.ProviderRef{Name: "sample-external"},
			Schema:     "app_schema",
			Database:   "analytics",
			Extensions: []string{"postgis"},
		},
		Status: infraApi.SchemaClaimStatus{Provider: "sample-external"},
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

func TestCleanupAction_MissingRoleSkipsRoleDrop(t *testing.T) {
	g := NewWithT(t)
	claim, provider, secret := cleanupFixtures()
	pg := &cleanupPG{databases: map[string]bool{"analytics": true}}

	err := newCleanupController(pg).cleanupAction(
		context.Background(),
		claimTestRequest(t, fakeCleanupClient(t, provider, secret, claim), claim),
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.droppedRoles).To(BeEmpty())
}

func TestCleanupAction_MissingDatabaseStillDropsRole(t *testing.T) {
	g := NewWithT(t)
	claim, provider, secret := cleanupFixtures()
	role := dbcontroller.RoleNameForSchemaClaim(claim.Namespace, claim.Name)
	pg := &cleanupPG{roles: map[string]bool{role: true}}

	err := newCleanupController(pg).cleanupAction(
		context.Background(),
		claimTestRequest(t, fakeCleanupClient(t, provider, secret, claim), claim),
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.droppedRoles).To(ContainElement(role))
}

// TestCleanupAction_DatabaseDroppedAtOpenStillDropsRole covers the race the
// invalid_catalog_name fallback exists for: DatabaseExists saw the database, but
// the client for it fails to open because the database is gone. Cleanup must
// read that as "nothing left inside it" — the SQLSTATE has to survive the
// boundary, which is why Client errors are projected rather than re-rendered.
func TestCleanupAction_DatabaseDroppedAtOpenStillDropsRole(t *testing.T) {
	g := NewWithT(t)
	claim, provider, adminSecret := cleanupFixtures()
	role := dbcontroller.RoleNameForSchemaClaim(claim.Namespace, claim.Name)
	pg := &cleanupPG{databases: map[string]bool{"analytics": true}, roles: map[string]bool{role: true}}

	ctrl := &Controller{Options: Options{
		cfg: &moduleconfig.Config{OperatorNamespace: "odh-db-operator-system"},
		PostgresClientFactory: func(_ context.Context, cfg postgres.Config) (postgres.Client, error) {
			if cfg.DBName == "analytics" {
				return nil, postgres.ProjectError("ping", &pgconn.PgError{
					Code:    pgerrcode.InvalidCatalogName,
					Message: `database "analytics" does not exist`,
				})
			}
			return pg, nil
		},
	}}

	err := ctrl.cleanupAction(
		context.Background(),
		claimTestRequest(t, fakeCleanupClient(t, provider, adminSecret, claim), claim),
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.droppedRoles).To(ContainElement(role))
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

func TestCleanupAction_DeletesStampedSecretWithStrippedOwnerRef(t *testing.T) {
	g := NewWithT(t)
	claim, provider, adminSecret := cleanupFixtures()
	role := dbcontroller.RoleNameForSchemaClaim(claim.Namespace, claim.Name)
	credSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      dbcontroller.SecretNameForSchemaClaim(claim),
		Namespace: claim.Namespace,
	}}
	// No controller ownerRef: the credential stamp is the only proof this
	// Secret is the claim's, and it must be enough for cleanup to delete it.
	dbcontroller.ApplySchemaClaimCredentials(credSecret, postgres.Config{}, role, secretvalue.FromString("pw"), "analytics", "app_claim", claim.UID)
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
	role := dbcontroller.RoleNameForSchemaClaim(claim.Namespace, claim.Name)
	controller := true
	credSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      dbcontroller.SecretNameForSchemaClaim(claim),
		Namespace: claim.Namespace,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: infraApi.GroupVersion.String(),
			Kind:       infraApi.DatabaseClaimKind,
			Name:       "sc-claim-a",
			UID:        types.UID("databaseclaim-uid"),
			Controller: &controller,
		}},
	}}
	dbcontroller.ApplySchemaClaimCredentials(credSecret, postgres.Config{}, role, secretvalue.FromString("forged-pw"), "analytics", "app_claim", claim.UID)
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
	credSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dbcontroller.SecretNameForSchemaClaim(claim),
			Namespace: claim.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: infraApi.GroupVersion.String(),
				Kind:       infraApi.DatabaseClaimKind,
				Name:       "sc-analytics",
				UID:        types.UID("other-claim-uid"),
				Controller: ptrTrue(),
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

func TestCleanupAction_MissingDatabaseStillCompletes(t *testing.T) {
	g := NewWithT(t)
	claim, provider, adminSecret := cleanupFixtures()
	credSecret := ownedCredentialsSecret(t, claim)
	role := dbcontroller.RoleNameForSchemaClaim(claim.Namespace, claim.Name)
	pg := &cleanupPG{roles: map[string]bool{role: true}}
	cli := fakeCleanupClient(t, provider, adminSecret, claim, credSecret)

	err := newCleanupController(pg).cleanupAction(
		context.Background(),
		claimTestRequest(t, cli, claim),
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.droppedRoles).To(ContainElement(role))

	got := &corev1.Secret{}
	err = cli.Get(context.Background(), client.ObjectKeyFromObject(credSecret), got)
	g.Expect(client.IgnoreNotFound(err)).To(Succeed())
}

func TestCleanupAction_MissingSchemaStillCompletes(t *testing.T) {
	g := NewWithT(t)
	claim, provider, adminSecret := cleanupFixtures()
	credSecret := ownedCredentialsSecret(t, claim)
	role := dbcontroller.RoleNameForSchemaClaim(claim.Namespace, claim.Name)
	// Database remains, but the schema was removed out of band. Revoke would
	// fail if attempted; cleanup must skip it and still drop role + Secret.
	pg := &cleanupPG{
		databases: map[string]bool{"analytics": true},
		roles:     map[string]bool{role: true},
		schemas:   map[string]bool{},
	}
	cli := fakeCleanupClient(t, provider, adminSecret, claim, credSecret)

	err := newCleanupController(pg).cleanupAction(
		context.Background(),
		claimTestRequest(t, cli, claim),
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.schemaRevokes).To(Equal(0))
	g.Expect(pg.droppedRoles).To(ContainElement(role))

	got := &corev1.Secret{}
	err = cli.Get(context.Background(), client.ObjectKeyFromObject(credSecret), got)
	g.Expect(err).To(HaveOccurred())
	g.Expect(client.IgnoreNotFound(err)).To(Succeed())
}

func TestCleanupAction_SkipsDropWhenSchemaMarkerDoesNotMatch(t *testing.T) {
	g := NewWithT(t)
	claim, provider, adminSecret := cleanupFixtures()
	claim.Spec.DeletionPolicy = infraApi.DeletionPolicyDelete
	claim.Status.SchemaCreated = true
	credSecret := ownedCredentialsSecret(t, claim)
	pg := &cleanupPG{
		databases: map[string]bool{"analytics": true},
		schemas:   map[string]bool{"app_schema": true},
		schemaOwners: map[string]string{
			"app_schema": "replacement-claim-uid",
		},
	}
	cli := fakeCleanupClient(t, provider, adminSecret, claim, credSecret)

	err := newCleanupController(pg).cleanupAction(
		context.Background(),
		claimTestRequest(t, cli, claim),
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.droppedSchemas).To(BeEmpty())
}

func newCleanupController(pg *cleanupPG) *Controller {
	return &Controller{Options: Options{
		cfg: &moduleconfig.Config{OperatorNamespace: "odh-db-operator-system"},
		PostgresClientFactory: func(_ context.Context, _ postgres.Config) (postgres.Client, error) {
			return pg, nil
		},
	}}
}

func cleanupFixtures() (*infraApi.SchemaClaim, *infraApi.DatabaseProvider, *corev1.Secret) {
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
	claim := &infraApi.SchemaClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "analytics",
			Namespace: "app",
			UID:       types.UID("schema-claim-uid"),
		},
		Spec: infraApi.SchemaClaimSpec{
			Provider:       infraApi.ProviderRef{Name: "rhai-db"},
			Schema:         "app_schema",
			Database:       "analytics",
			DeletionPolicy: infraApi.DeletionPolicyRetain,
		},
		Status: infraApi.SchemaClaimStatus{
			Provider: "rhai-db",
			Schema:   "app_schema",
			Connection: infraApi.SchemaConnectionStatus{
				Database: "analytics",
			},
		},
	}
	return claim, provider, secret
}

func ownedCredentialsSecret(t *testing.T, claim *infraApi.SchemaClaim) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dbcontroller.SecretNameForSchemaClaim(claim),
			Namespace: claim.Namespace,
		},
	}
	g := NewWithT(t)
	g.Expect(controllerutil.SetControllerReference(claim, secret, claimTestScheme(t))).To(Succeed())
	return secret
}

func ptrTrue() *bool {
	v := true
	return &v
}

func fakeCleanupClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithScheme(claimTestScheme(t)).WithObjects(objs...).Build()
}

// cleanupPG is a minimal postgres.Client fake for cleanupAction paths.
type cleanupPG struct {
	databases      map[string]bool
	schemas        map[string]bool
	schemaOwners   map[string]string
	roles          map[string]bool
	droppedRoles   []string
	droppedSchemas []string
	schemaRevokes  int
}

func (t *cleanupPG) Config() postgres.Config { return postgres.Config{} }
func (t *cleanupPG) Close()                  {}
func (t *cleanupPG) Ping(context.Context) error {
	return nil
}

func (t *cleanupPG) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	if t.schemas == nil {
		t.schemas = map[string]bool{}
	}
	switch {
	case strings.Contains(query, "DROP ROLE"):
		start := strings.Index(query, `"`)
		end := strings.LastIndex(query, `"`)
		if start >= 0 && end > start {
			name := query[start+1 : end]
			delete(t.roles, name)
			t.droppedRoles = append(t.droppedRoles, name)
		}
	case strings.Contains(query, "DROP SCHEMA"):
		name := cleanupQuotedAfter(query, "DROP SCHEMA IF EXISTS ")
		if name == "" {
			name = cleanupQuotedAfter(query, "DROP SCHEMA ")
		}
		if name != "" {
			delete(t.schemas, name)
			t.droppedSchemas = append(t.droppedSchemas, name)
		}
	case strings.Contains(query, "REVOKE") || strings.Contains(query, "ALTER DEFAULT PRIVILEGES"):
		if strings.Contains(query, "SCHEMA ") {
			t.schemaRevokes++
			name := cleanupQuotedAfter(query, "SCHEMA ")
			if name != "" && !t.schemas[name] {
				return pgconn.CommandTag{}, &pgconn.PgError{
					Code:    "3F000",
					Message: "schema \"" + name + "\" does not exist",
				}
			}
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
	if t.schemas == nil {
		t.schemas = map[string]bool{}
	}
	if t.schemaOwners == nil {
		t.schemaOwners = map[string]string{}
	}
	if t.roles == nil {
		t.roles = map[string]bool{}
	}
	name, _ := args[0].(string)
	switch {
	case strings.Contains(query, "pg_database"):
		return cleanupBoolRow{t.databases[name]}, nil
	case strings.Contains(query, "obj_description"):
		uid := t.schemaOwners[name]
		if uid == "" {
			return cleanupNullStringRow{}, nil
		}
		return cleanupNullStringRow{
			valid: true,
			s:     "opendatahub.io/schemaclaim-uid=" + uid,
		}, nil
	case strings.Contains(query, "pg_namespace"):
		return cleanupBoolRow{t.schemas[name]}, nil
	case strings.Contains(query, "pg_roles"):
		return cleanupBoolRow{t.roles[name]}, nil
	default:
		return cleanupBoolRow{false}, nil
	}
}

func cleanupQuotedAfter(query, marker string) string {
	idx := strings.Index(query, marker)
	if idx < 0 {
		return ""
	}
	rest := query[idx+len(marker):]
	start := strings.Index(rest, `"`)
	if start < 0 {
		return ""
	}
	rest = rest[start+1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
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

type cleanupNullStringRow struct {
	valid bool
	s     string
}

func (r cleanupNullStringRow) Scan(dest ...any) error {
	ns, ok := dest[0].(*sql.NullString)
	if !ok {
		return fmt.Errorf("expected *sql.NullString")
	}
	ns.Valid = r.valid
	ns.String = r.s
	return nil
}

// extensionProvisionPG supports provisionAction paths through extension verification.
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

func (p *extensionProvisionPG) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	p.execSQLs = append(p.execSQLs, query)
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
		return cleanupBoolRow{p.databases[name]}, nil
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
		return cleanupBoolRow{installed}, nil
	default:
		return cleanupBoolRow{false}, nil
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
	for _, stmt := range p.execSQLs {
		if strings.Contains(strings.ToUpper(stmt), "CREATE EXTENSION") {
			return true
		}
	}
	return false
}

func claimTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	g := NewWithT(t)
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	g.Expect(infraApi.AddToScheme(scheme)).To(Succeed())
	return scheme
}

func claimTestRequest(t *testing.T, cli client.Client, claim *infraApi.SchemaClaim) *odhtypes.ReconciliationRequest {
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
