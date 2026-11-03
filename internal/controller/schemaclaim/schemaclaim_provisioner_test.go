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

package schemaclaim_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	"github.com/opendatahub-io/opendatahub-db-operator/internal/controller/schemaclaim"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/controller/claimerrors"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
	secretvalue "github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

func TestEnsure_CreatesSchemaRoleAndSecret(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
	}

	cli := fakeClient(t, claim)
	secret, err := schemaclaim.SchemaProvisioner{
		Client:         cli,
		Claim:          claim,
		Provider:       internalProvider(),
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory: func(_ context.Context, cfg postgres.Config) (postgres.Client, error) {
			return &trackingClient{
				cfg:       cfg,
				databases: pg.databases,
				schemas:   pg.schemas,
				roles:     pg.roles,
				parent:    pg,
			}, nil
		},
	}.Ensure(context.Background())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.createdSchemas).To(ContainElement("myschema"))
	g.Expect(claim.Status.SchemaCreated).To(BeTrue())
	stored := &infraApi.SchemaClaim{}
	g.Expect(cli.Get(context.Background(), client.ObjectKeyFromObject(claim), stored)).To(Succeed())
	g.Expect(stored.Status.SchemaCreated).To(BeTrue())
	g.Expect(pg.createdRoles).NotTo(BeEmpty())
	g.Expect(pg.createdDatabases).To(BeEmpty())
	g.Expect(string(secret.Data[postgres.SecretKeySchema])).To(Equal("myschema"))
	g.Expect(string(secret.Data[postgres.SecretKeyDatabase])).To(Equal("analytics"))
	g.Expect(secret.Annotations[postgres.AnnotationConnectionTypeProtocol]).
		To(Equal(postgres.ConnectionTypeProtocolPostgres))
	g.Expect(string(secret.Data[postgres.SecretKeySSLMode])).To(Equal(postgres.SSLModeDisable))
}

func TestEnsure_RetriesSchemaCreatedStatusConflict(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
	}
	base := fakeClient(t, claim)
	cli := &conflictOnceStatusClient{Client: base}

	_, err := schemaclaim.SchemaProvisioner{
		Client:         cli,
		Claim:          claim,
		Provider:       internalProvider(),
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory: func(_ context.Context, cfg postgres.Config) (postgres.Client, error) {
			return &trackingClient{
				cfg:       cfg,
				databases: pg.databases,
				schemas:   pg.schemas,
				roles:     pg.roles,
				parent:    pg,
			}, nil
		},
	}.Ensure(context.Background())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cli.updates.Load()).To(BeNumerically(">=", 2))
	g.Expect(claim.Status.SchemaCreated).To(BeTrue())
	stored := &infraApi.SchemaClaim{}
	g.Expect(base.Get(context.Background(), client.ObjectKeyFromObject(claim), stored)).To(Succeed())
	g.Expect(stored.Status.SchemaCreated).To(BeTrue())
}

func TestEnsure_RejectsCredentialsSecretOwnedByOther(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	foreignOwner := &infraApi.DatabaseClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sc-claim-a",
			Namespace: claim.Namespace,
			UID:       "databaseclaim-uid",
		},
	}
	secretName := dbcontroller.SecretNameForSchemaClaim(claim)
	g.Expect(secretName).To(Equal(foreignOwner.Name))
	foreignSecret := credentialsSecretOwnedBy(t, secretName, claim.Namespace, foreignOwner)
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
	}

	_, err := ensure(t, claim, internalProvider(), pg, foreignSecret)
	g.Expect(err).To(MatchError(claimerrors.SecretNotOwned{Secret: secretName}))
	g.Expect(pg.createdSchemas).To(BeEmpty())
	g.Expect(pg.createdRoles).To(BeEmpty())
}

func TestEnsure_RotatesTamperedPassword(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	claim.UID = "schema-claim-uid"
	role := dbcontroller.RoleNameForSchemaClaim(claim.Namespace, claim.Name)
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
		roles:     map[string]bool{role: true},
	}
	secretName := dbcontroller.SecretNameForSchemaClaim(claim)
	existing := credentialsSecretOwnedBy(t, secretName, claim.Namespace, claim)
	dbcontroller.ApplySchemaClaimCredentials(existing, providerConfig(), role, secretvalue.FromString("original-pw"), "analytics", "app_claim", claim.UID)
	existing.Data[postgres.SecretKeyPassword] = []byte("tampered-pw")

	secret, err := ensure(t, claim, internalProvider(), pg, existing)

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.execSQLsContainingPasswordSet()).To(Equal(1),
		"a password that fails stamp verification must be rotated")
	g.Expect(string(secret.Data[postgres.SecretKeyPassword])).NotTo(Equal("tampered-pw"))
}

func TestEnsure_RejectsPreexistingSchemaWithoutOwnerMarker(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
		schemas:   map[string]bool{"myschema": true},
	}

	_, err := ensure(t, claim, internalProvider(), pg)
	g.Expect(err).To(MatchError(claimerrors.SchemaNotOwned{Schema: "myschema"}))
	g.Expect(pg.createdSchemas).To(BeEmpty())
	g.Expect(pg.createdRoles).To(BeEmpty())
	g.Expect(claim.Status.SchemaCreated).To(BeFalse())
}

func TestEnsure_RejectsPreexistingSchemaWithForeignOwnerMarker(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
		schemas:   map[string]bool{"myschema": true},
		schemaOwners: map[string]string{
			"myschema": "deleted-peer-uid",
		},
	}

	_, err := ensure(t, claim, internalProvider(), pg)
	g.Expect(err).To(MatchError(claimerrors.SchemaNotOwned{Schema: "myschema"}))
	g.Expect(pg.createdSchemas).To(BeEmpty())
	g.Expect(pg.createdRoles).To(BeEmpty())
	g.Expect(claim.Status.SchemaCreated).To(BeFalse())
}

func TestEnsure_RecoversPreexistingSchemaWithMatchingOwnerMarker(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
		schemas:   map[string]bool{"myschema": true},
		schemaOwners: map[string]string{
			"myschema": string(claim.UID),
		},
	}

	secret, err := ensure(t, claim, internalProvider(), pg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.createdSchemas).To(BeEmpty())
	g.Expect(claim.Status.SchemaCreated).To(BeTrue())
	g.Expect(string(secret.Data[postgres.SecretKeySchema])).To(Equal("myschema"))
}

func TestEnsure_RejectsExclusiveCreateDuplicateWithoutOwnerMarker(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	pg := &trackingClient{
		cfg:                        providerConfig(),
		databases:                  map[string]bool{"analytics": true},
		schemas:                    map[string]bool{"myschema": true},
		forceCreateSchemaDuplicate: true,
	}

	_, err := ensure(t, claim, internalProvider(), pg)
	g.Expect(err).To(MatchError(claimerrors.SchemaNotOwned{Schema: "myschema"}))
	g.Expect(claim.Status.SchemaCreated).To(BeFalse())
}

func TestEnsure_RejectsExclusiveCreateDuplicateWithForeignOwnerMarker(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
		schemas:   map[string]bool{"myschema": true},
		schemaOwners: map[string]string{
			"myschema": "other-claim-uid",
		},
		forceCreateSchemaDuplicate: true,
	}

	_, err := ensure(t, claim, internalProvider(), pg)
	g.Expect(err).To(MatchError(claimerrors.SchemaNotOwned{Schema: "myschema"}))
	g.Expect(claim.Status.SchemaCreated).To(BeFalse())
}

func TestEnsure_AllowsPreexistingSchemaWhenOwned(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	claim.Status.SchemaCreated = true
	claim.Status.Schema = "myschema"
	claim.Status.Connection.Database = claim.Spec.Database
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
		schemas:   map[string]bool{"myschema": true},
		schemaOwners: map[string]string{
			"myschema": string(claim.UID),
		},
	}

	secret, err := ensure(t, claim, internalProvider(), pg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.createdSchemas).To(BeEmpty())
	g.Expect(claim.Status.SchemaCreated).To(BeTrue())
	g.Expect(string(secret.Data[postgres.SecretKeySchema])).To(Equal("myschema"))
}

func TestEnsure_RejectsSchemaCreatedWhenMarkerReplaced(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	claim.Status.SchemaCreated = true
	claim.Status.Schema = "myschema"
	claim.Status.Connection.Database = claim.Spec.Database
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
		schemas:   map[string]bool{"myschema": true},
		schemaOwners: map[string]string{
			"myschema": "replacement-owner-uid",
		},
	}

	_, err := ensure(t, claim, internalProvider(), pg)
	g.Expect(err).To(MatchError(claimerrors.SchemaNotOwned{Schema: "myschema"}))
	g.Expect(pg.createdRoles).To(BeEmpty())
}

func TestEnsure_RejectsSchemaCreatedWhenRetargeted(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	claim.Status.SchemaCreated = true
	claim.Status.Schema = "owned_schema"
	claim.Status.Connection.Database = claim.Spec.Database
	claim.Spec.Schema = "victim_schema"
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
		schemas:   map[string]bool{"victim_schema": true, "owned_schema": true},
	}

	_, err := ensure(t, claim, internalProvider(), pg)
	g.Expect(err).To(MatchError(claimerrors.SchemaNotOwned{Schema: "victim_schema"}))
	g.Expect(pg.createdSchemas).To(BeEmpty())
	g.Expect(pg.createdRoles).To(BeEmpty())
}

func TestEnsure_RejectsReservedSchema(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	claim.Spec.Schema = "public"
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
	}

	_, err := ensure(t, claim, internalProvider(), pg)
	g.Expect(err).To(MatchError(claimerrors.SchemaReserved{Schema: "public"}))
	g.Expect(pg.createdSchemas).To(BeEmpty())
	g.Expect(pg.createdRoles).To(BeEmpty())
}

func TestEnsure_MissingDatabaseIsNotCreated(t *testing.T) {
	g := NewWithT(t)
	pg := &trackingClient{cfg: providerConfig()}

	_, err := ensure(t, sampleClaim(), internalProvider(), pg)
	g.Expect(err).To(MatchError(claimerrors.DatabaseNotFound{Database: "analytics"}))
	g.Expect(pg.createdDatabases).To(BeEmpty())
	g.Expect(pg.createdSchemas).To(BeEmpty())
}

func TestEnsure_DeniesSchemaCreateOnExternalWithoutCapability(t *testing.T) {
	g := NewWithT(t)
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "external"},
		Spec: infraApi.DatabaseProviderSpec{
			Type:     infraApi.ProviderTypeExternal,
			External: &infraApi.ExternalProviderSpec{},
		},
	}
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
	}

	_, err := ensure(t, sampleClaim(), provider, pg)
	g.Expect(err).To(MatchError(claimerrors.SchemaCreateNotAllowed{Schema: "myschema"}))
	g.Expect(pg.createdSchemas).To(BeEmpty())
}

func TestEnsure_CreatesSchemaOnExternalWhenAllowed(t *testing.T) {
	g := NewWithT(t)
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "external"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeExternal,
			External: &infraApi.ExternalProviderSpec{
				Capabilities: []infraApi.ExternalCapability{
					infraApi.ExternalCapabilityCreateSchema,
				},
			},
		},
	}
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
	}

	claim := sampleClaim()
	secret, err := ensure(t, claim, provider, pg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.createdSchemas).To(ContainElement("myschema"))
	g.Expect(claim.Status.SchemaCreated).To(BeTrue())
	g.Expect(string(secret.Data[postgres.SecretKeySchema])).To(Equal("myschema"))
}

func TestEnsure_ReadOnlyGrantsConnectWithoutDatabaseCreate(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	claim.Spec.Access = infraApi.AccessModeReadOnly
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
	}

	_, err := ensure(t, claim, internalProvider(), pg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.grantedConnect).To(Equal(1))
	g.Expect(pg.grantedDBCreate).To(Equal(0))
}

// TestEnsure_RegrantsSchemaPrivilegesOnEveryReconcile guards the partial-grant
// self-heal path: GrantSchemaPrivileges is multi-statement, and a shallow
// USAGE/CREATE check must not skip re-grant on later reconciles.
func TestEnsure_RegrantsSchemaPrivilegesOnEveryReconcile(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
	}
	cli := fakeClient(t, claim)
	provisioner := schemaclaim.SchemaProvisioner{
		Client:         cli,
		Claim:          claim,
		Provider:       internalProvider(),
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory: func(_ context.Context, cfg postgres.Config) (postgres.Client, error) {
			return &trackingClient{
				cfg:          cfg,
				databases:    pg.databases,
				schemas:      pg.schemas,
				schemaOwners: pg.schemaOwners,
				roles:        pg.roles,
				parent:       pg,
			}, nil
		},
	}

	_, err := provisioner.Ensure(context.Background())
	g.Expect(err).NotTo(HaveOccurred())
	afterFirstTables := pg.countExecContaining("ON ALL TABLES IN SCHEMA")
	afterFirstDefaults := pg.countExecContaining("ALTER DEFAULT PRIVILEGES IN SCHEMA")
	g.Expect(afterFirstTables).To(BeNumerically(">", 0))
	g.Expect(afterFirstDefaults).To(BeNumerically(">", 0))

	_, err = provisioner.Ensure(context.Background())
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.countExecContaining("ON ALL TABLES IN SCHEMA")).To(Equal(afterFirstTables * 2))
	g.Expect(pg.countExecContaining("ALTER DEFAULT PRIVILEGES IN SCHEMA")).To(Equal(afterFirstDefaults * 2))
}

func TestEnsure_InternalInstallsExtensionsBeforeSchema(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	claim.Spec.Extensions = []string{"vector"}
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
	}

	_, err := ensure(t, claim, internalProvider(), pg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.createdSchemas).To(ContainElement("myschema"))
	g.Expect(pg.execSQLsContainingCreateExtension("vector")).To(BeTrue())
}

func TestEnsure_ExternalMissingExtensionReturnsNotAvailable(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	claim.Spec.Extensions = []string{"vector"}
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "external"},
		Spec: infraApi.DatabaseProviderSpec{
			Type:     infraApi.ProviderTypeExternal,
			External: &infraApi.ExternalProviderSpec{},
		},
	}
	pg := &trackingClient{
		cfg:                      providerConfig(),
		databases:                map[string]bool{"analytics": true},
		extensionAvailableByName: map[string]bool{"vector": false},
	}

	_, err := ensure(t, claim, provider, pg)
	var missing claimerrors.ExtensionNotAvailable
	g.Expect(errors.As(err, &missing)).To(BeTrue())
	g.Expect(missing.Extensions).To(Equal([]string{"vector"}))
	g.Expect(pg.execSQLsContainingCreateExtension("vector")).To(BeFalse())
}

func TestEnsure_ExternalAvailableExtensionNeverCreates(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim()
	claim.Spec.Extensions = []string{"vector"}
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "external"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeExternal,
			External: &infraApi.ExternalProviderSpec{
				Capabilities: []infraApi.ExternalCapability{
					infraApi.ExternalCapabilityCreateSchema,
				},
			},
		},
	}
	pg := &trackingClient{
		cfg:                      providerConfig(),
		databases:                map[string]bool{"analytics": true},
		extensionAvailableByName: map[string]bool{"vector": true},
	}

	_, err := ensure(t, claim, provider, pg)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.execSQLsContainingCreateExtension("vector")).To(BeFalse())
	g.Expect(pg.createdSchemas).To(ContainElement("myschema"))
}

func ensure(
	t *testing.T,
	claim *infraApi.SchemaClaim,
	provider *infraApi.DatabaseProvider,
	pg *trackingClient,
	extra ...client.Object,
) (*corev1.Secret, error) {
	t.Helper()
	objs := append([]client.Object{claim}, extra...)
	cli := fakeClient(t, objs...)
	return schemaclaim.SchemaProvisioner{
		Client:         cli,
		Claim:          claim,
		Provider:       provider,
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory: func(_ context.Context, cfg postgres.Config) (postgres.Client, error) {
			return &trackingClient{
				cfg:                        cfg,
				databases:                  pg.databases,
				schemas:                    pg.schemas,
				schemaOwners:               pg.schemaOwners,
				roles:                      pg.roles,
				forceCreateSchemaDuplicate: pg.forceCreateSchemaDuplicate,
				extensionAvailableByName:   pg.extensionAvailableByName,
				parent:                     pg,
			}, nil
		},
	}.Ensure(context.Background())
}

func sampleClaim() *infraApi.SchemaClaim {
	return &infraApi.SchemaClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claim-a",
			Namespace: "app-ns",
			UID:       "claim-a-uid",
		},
		Spec: infraApi.SchemaClaimSpec{
			Provider: infraApi.ProviderRef{Name: "rhai-db"},
			Schema:   "myschema",
			Database: "analytics",
			Access:   infraApi.AccessModeReadWrite,
		},
	}
}

func internalProvider() *infraApi.DatabaseProvider {
	return &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "rhai-db"},
		Spec:       infraApi.DatabaseProviderSpec{Type: infraApi.ProviderTypeInternal},
	}
}

func providerConfig() postgres.Config {
	return postgres.Config{
		Host:   "rhai-db.database-system.svc",
		Port:   5432,
		User:   "postgres",
		DBName: "postgres",
	}
}

func fakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	g := NewWithT(t)
	g.Expect(infraApi.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&infraApi.SchemaClaim{}).
		Build()
}

func credentialsSecretOwnedBy(t *testing.T, name, namespace string, owner metav1.Object) *corev1.Secret {
	t.Helper()
	g := NewWithT(t)
	scheme := runtime.NewScheme()
	g.Expect(infraApi.AddToScheme(scheme)).To(Succeed())
	g.Expect(corev1.AddToScheme(scheme)).To(Succeed())
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	ownerObj, ok := owner.(client.Object)
	g.Expect(ok).To(BeTrue())
	g.Expect(controllerutil.SetControllerReference(ownerObj, secret, scheme)).To(Succeed())
	return secret
}

// conflictOnceStatusClient fails the first Status().Update with Conflict so
// persistSchemaCreated must retry.
type conflictOnceStatusClient struct {
	client.Client
	updates atomic.Int32
}

func (c *conflictOnceStatusClient) Status() client.StatusWriter {
	return &conflictOnceStatusWriter{parent: c, inner: c.Client.Status()}
}

type conflictOnceStatusWriter struct {
	parent *conflictOnceStatusClient
	inner  client.StatusWriter
}

func (w *conflictOnceStatusWriter) Create(ctx context.Context, obj, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	return w.inner.Create(ctx, obj, subResource, opts...)
}

func (w *conflictOnceStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if w.parent.updates.Add(1) == 1 {
		return apierrors.NewConflict(
			schema.GroupResource{Group: infraApi.GroupVersion.Group, Resource: "schemaclaims"},
			obj.GetName(),
			fmt.Errorf("simulated conflict"),
		)
	}
	return w.inner.Update(ctx, obj, opts...)
}

func (w *conflictOnceStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	return w.inner.Patch(ctx, obj, patch, opts...)
}

func (w *conflictOnceStatusWriter) Apply(ctx context.Context, obj runtime.ApplyConfiguration, opts ...client.SubResourceApplyOption) error {
	return w.inner.Apply(ctx, obj, opts...)
}

type trackingClient struct {
	cfg                        postgres.Config
	databases                  map[string]bool
	schemas                    map[string]bool
	schemaOwners               map[string]string
	roles                      map[string]bool
	forceCreateSchemaDuplicate bool
	createdDatabases           []string
	createdSchemas             []string
	createdRoles               []string
	grantedConnect             int
	grantedDBCreate            int
	extensionAvailableByName   map[string]bool
	execSQLs                   []string
	parent                     *trackingClient
}

func (t *trackingClient) execSQLsContainingPasswordSet() int {
	root := t.root()
	count := 0
	for _, stmt := range root.execSQLs {
		if strings.Contains(stmt, "PASSWORD") && strings.Contains(stmt, "ALTER ROLE") {
			count++
		}
	}
	return count
}

func (t *trackingClient) execSQLsContainingCreateExtension(name string) bool {
	root := t.root()
	for _, stmt := range root.execSQLs {
		upper := strings.ToUpper(stmt)
		if strings.Contains(upper, "CREATE EXTENSION") && strings.Contains(stmt, name) {
			return true
		}
	}
	return false
}

func (t *trackingClient) countExecContaining(substr string) int {
	root := t.root()
	n := 0
	for _, stmt := range root.execSQLs {
		if strings.Contains(stmt, substr) {
			n++
		}
	}
	return n
}

func (t *trackingClient) root() *trackingClient {
	if t.parent != nil {
		return t.parent
	}
	return t
}

func (t *trackingClient) Config() postgres.Config { return t.cfg }
func (t *trackingClient) Close()                  {}
func (t *trackingClient) Ping(context.Context) error {
	return nil
}

func (t *trackingClient) Exec(_ context.Context, query string, _ ...any) (pgconn.CommandTag, error) {
	root := t.root()
	root.execSQLs = append(root.execSQLs, query)
	if root.databases == nil {
		root.databases = map[string]bool{}
	}
	if root.schemas == nil {
		root.schemas = map[string]bool{}
	}
	if root.schemaOwners == nil {
		root.schemaOwners = map[string]string{}
	}
	if root.roles == nil {
		root.roles = map[string]bool{}
	}
	switch {
	case strings.Contains(query, "CREATE SCHEMA"):
		name := createSchemaName(query)
		exclusive := !strings.Contains(query, "IF NOT EXISTS")
		if exclusive && (root.forceCreateSchemaDuplicate || root.schemas[name]) {
			return pgconn.CommandTag{}, &pgconn.PgError{Code: "42P06", Message: "duplicate_schema"}
		}
		root.schemas[name] = true
		root.createdSchemas = append(root.createdSchemas, name)
		if uid := ownerUIDFromSQL(query); uid != "" {
			root.schemaOwners[name] = uid
		}
	case strings.Contains(query, "CREATE ROLE"):
		name := firstQuotedIdent(query)
		root.roles[name] = true
		root.createdRoles = append(root.createdRoles, name)
	case strings.Contains(query, "GRANT CONNECT ON DATABASE"):
		root.grantedConnect++
	case strings.Contains(query, "GRANT CREATE ON DATABASE"):
		root.grantedDBCreate++
	}
	return pgconn.CommandTag{}, nil
}

func (t *trackingClient) Query(context.Context, string, ...any) (postgres.Rows, error) {
	return nil, fmt.Errorf("unexpected Query")
}

func (t *trackingClient) QueryRow(_ context.Context, query string, args ...any) (pgx.Row, error) {
	root := t.root()
	if root.databases == nil {
		root.databases = map[string]bool{}
	}
	if root.schemas == nil {
		root.schemas = map[string]bool{}
	}
	if root.schemaOwners == nil {
		root.schemaOwners = map[string]string{}
	}
	if root.roles == nil {
		root.roles = map[string]bool{}
	}
	switch {
	case strings.Contains(query, "pg_database"):
		name, _ := args[0].(string)
		return boolRow{root.databases[name]}, nil
	case strings.Contains(query, "obj_description"):
		name, _ := args[0].(string)
		uid := root.schemaOwners[name]
		if uid == "" {
			return nullStringRow{}, nil
		}
		return nullStringRow{
			valid: true,
			s:     "opendatahub.io/schemaclaim-uid=" + uid,
		}, nil
	case strings.Contains(query, "pg_namespace"):
		name, _ := args[0].(string)
		return boolRow{root.schemas[name]}, nil
	case strings.Contains(query, "pg_roles"):
		name, _ := args[0].(string)
		return boolRow{root.roles[name]}, nil
	case strings.Contains(query, "HAS_DATABASE_PRIVILEGE"), strings.Contains(query, "HAS_SCHEMA_PRIVILEGE"):
		return boolRow{false}, nil
	case strings.Contains(query, "pg_extension"):
		if names, ok := args[0].([]string); ok {
			var installed []string
			for _, name := range names {
				if root.extensionAvailableByName[name] {
					installed = append(installed, name)
				}
			}
			return namesRow{names: installed}, nil
		}
		name, _ := args[0].(string)
		installed := false
		if root.extensionAvailableByName != nil {
			installed = root.extensionAvailableByName[name]
		}
		return boolRow{installed}, nil
	default:
		return boolRow{false}, nil
	}
}

type boolRow struct{ v bool }

func (r boolRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return fmt.Errorf("expected 1 dest")
	}
	ptr, ok := dest[0].(*bool)
	if !ok {
		return fmt.Errorf("expected *bool")
	}
	*ptr = r.v
	return nil
}

type namesRow struct{ names []string }

func (r namesRow) Scan(dest ...any) error {
	ptr, ok := dest[0].(*[]string)
	if !ok {
		return fmt.Errorf("expected *[]string")
	}
	*ptr = append([]string(nil), r.names...)
	return nil
}

func firstQuotedIdent(query string) string {
	start := -1
	for i, r := range query {
		if r == '"' {
			if start < 0 {
				start = i + 1
			} else {
				return query[start:i]
			}
		}
	}
	return ""
}

func createSchemaName(query string) string {
	if name := firstQuotedIdent(query); name != "" {
		return name
	}
	const needle = "format('CREATE SCHEMA %I', '"
	if i := strings.Index(query, needle); i >= 0 {
		rest := query[i+len(needle):]
		if j := strings.Index(rest, "'"); j >= 0 {
			return rest[:j]
		}
	}
	return ""
}

func ownerUIDFromSQL(query string) string {
	const prefix = "opendatahub.io/schemaclaim-uid="
	i := strings.Index(query, prefix)
	if i < 0 {
		return ""
	}
	rest := query[i+len(prefix):]
	if j := strings.IndexAny(rest, `"'`); j >= 0 {
		return rest[:j]
	}
	return rest
}

type nullStringRow struct {
	valid bool
	s     string
}

func (r nullStringRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return fmt.Errorf("expected 1 dest")
	}
	ns, ok := dest[0].(*sql.NullString)
	if !ok {
		return fmt.Errorf("expected *sql.NullString")
	}
	ns.Valid = r.valid
	ns.String = r.s
	return nil
}
