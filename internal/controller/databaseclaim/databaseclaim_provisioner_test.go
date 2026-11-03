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

package databaseclaim_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	"github.com/opendatahub-io/opendatahub-db-operator/internal/controller/databaseclaim"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/controller/claimerrors"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
	secretvalue "github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

func TestEnsure_CreatesNamedDatabaseWhenAllowed(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
	provider := internalProvider()
	pg := &trackingClient{cfg: providerConfig()}
	cli := fakeClient(t)

	secret, err := databaseclaim.DatabaseProvisioner{
		Client:         cli,
		Claim:          claim,
		Provider:       provider,
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory:  testDBClientFactory(pg),
	}.Ensure(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(secret).NotTo(BeNil())
	g.Expect(pg.createdDatabases).To(ContainElement("analytics"))
	g.Expect(pg.createdRoles).NotTo(BeEmpty())
	g.Expect(string(secret.Data[postgres.SecretKeyDatabase])).To(Equal("analytics"))
	g.Expect(secret.Annotations[postgres.AnnotationConnectionTypeProtocol]).
		To(Equal(postgres.ConnectionTypeProtocolPostgres))
	g.Expect(string(secret.Data[postgres.SecretKeySSLMode])).To(Equal(postgres.SSLModeDisable))
	g.Expect(string(secret.Data[postgres.SecretKeyPassword])).To(MatchRegexp(`^[A-Za-z0-9]+$`))
}

func TestEnsure_DeniesCreateOnExternalWithoutCapability(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "external"},
		Spec: infraApi.DatabaseProviderSpec{
			Type:     infraApi.ProviderTypeExternal,
			External: &infraApi.ExternalProviderSpec{},
		},
	}
	pg := &trackingClient{cfg: providerConfig()}

	_, err := databaseclaim.DatabaseProvisioner{
		Client:         fakeClient(t),
		Claim:          claim,
		Provider:       provider,
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory:  testDBClientFactory(pg),
	}.Ensure(context.Background())

	g.Expect(err).To(MatchError(claimerrors.DatabaseCreateNotAllowed{Database: "analytics"}))
	g.Expect(pg.createdDatabases).To(BeEmpty())
}

func TestEnsure_DefaultDatabaseMissingIsNotCreated(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("")
	pg := &trackingClient{cfg: providerConfig()}

	_, err := databaseclaim.DatabaseProvisioner{
		Client:         fakeClient(t),
		Claim:          claim,
		Provider:       internalProvider(),
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory:  testDBClientFactory(pg),
	}.Ensure(context.Background())

	g.Expect(err).To(MatchError(claimerrors.DatabaseNotFound{Database: "postgres"}))
	g.Expect(pg.createdDatabases).To(BeEmpty())
}

func TestEnsure_DistinctClaimsGetDistinctRoles(t *testing.T) {
	g := NewWithT(t)
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
	}
	cli := fakeClient(t)

	a := sampleClaim("analytics")
	a.Name = "my-db"
	b := sampleClaim("analytics")
	b.Name = "my.db"

	secretA, err := databaseclaim.DatabaseProvisioner{
		Client: cli, Claim: a, Provider: internalProvider(), Postgres: pg, ProviderConfig: providerConfig(),
		ClientFactory: testDBClientFactory(pg),
	}.Ensure(context.Background())
	g.Expect(err).NotTo(HaveOccurred())

	secretB, err := databaseclaim.DatabaseProvisioner{
		Client: cli, Claim: b, Provider: internalProvider(), Postgres: pg, ProviderConfig: providerConfig(),
		ClientFactory: testDBClientFactory(pg),
	}.Ensure(context.Background())
	g.Expect(err).NotTo(HaveOccurred())

	g.Expect(string(secretA.Data[postgres.SecretKeyUser])).
		NotTo(Equal(string(secretB.Data[postgres.SecretKeyUser])))
	g.Expect(pg.createdRoles).To(HaveLen(2))
}

func TestEnsure_RotatesPasswordWhenSecretMissingButRoleExists(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
	role := dbcontroller.RoleNameFor(claim.Namespace, claim.Name)
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
		roles:     map[string]bool{role: true},
	}

	secret, err := databaseclaim.DatabaseProvisioner{
		Client:         fakeClient(t),
		Claim:          claim,
		Provider:       internalProvider(),
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory:  testDBClientFactory(pg),
	}.Ensure(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.createdRoles).To(BeEmpty())
	g.Expect(pg.passwordSets).To(Equal(1))
	g.Expect(string(secret.Data[postgres.SecretKeyPassword])).To(MatchRegexp(`^[A-Za-z0-9]+$`))
	g.Expect(string(secret.Data[postgres.SecretKeyUser])).To(Equal(role))
}

func TestEnsure_RotatesPasswordWhenSecretPasswordEmpty(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
	claim.UID = "claim-a-uid"
	role := dbcontroller.RoleNameFor(claim.Namespace, claim.Name)
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
		roles:     map[string]bool{role: true},
	}
	existing := credentialsSecretOwnedBy(t, claim.Name, claim.Namespace, claim)
	existing.Data = map[string][]byte{
		postgres.SecretKeyHost:     []byte("stale.example"),
		postgres.SecretKeyPassword: []byte(""),
	}

	secret, err := databaseclaim.DatabaseProvisioner{
		Client:         fakeClient(t, existing),
		Claim:          claim,
		Provider:       internalProvider(),
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory:  testDBClientFactory(pg),
	}.Ensure(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.createdRoles).To(BeEmpty())
	g.Expect(pg.passwordSets).To(Equal(1))
	g.Expect(string(secret.Data[postgres.SecretKeyPassword])).To(MatchRegexp(`^[A-Za-z0-9]+$`))
	g.Expect(string(secret.Data[postgres.SecretKeyHost])).To(Equal(providerConfig().Host))
}

func TestEnsure_RejectsUnownedCredentialsSecret(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
	claim.UID = types.UID("claim-uid")
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
	}
	unowned := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace},
		Data:       map[string][]byte{postgres.SecretKeyPassword: []byte("foreign-password")},
	}
	cli := fakeClient(t, unowned)

	_, err := databaseclaim.DatabaseProvisioner{
		Client:         cli,
		Claim:          claim,
		Provider:       internalProvider(),
		Postgres:       pg,
		ProviderConfig: providerConfig(),
	}.Ensure(context.Background())

	g.Expect(err).To(MatchError(claimerrors.SecretNotOwned{Secret: claim.Name}))
	g.Expect(pg.createdRoles).To(BeEmpty(), "no role work before the foreign Secret is rejected")
	g.Expect(pg.passwordSets).To(Equal(0), "the foreign Secret's password must never be reused")
}

// stampedCredentialsSecret returns the claim's credentials Secret with a
// valid credential stamp for password, built through the same writer the
// provisioner uses.
const testClaimUID = "claim-uid"

func stampedCredentialsSecret(t *testing.T, claim *infraApi.DatabaseClaim, password string) *corev1.Secret {
	t.Helper()
	role := dbcontroller.RoleNameFor(claim.Namespace, claim.Name)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: claim.Name, Namespace: claim.Namespace}}
	dbcontroller.ApplyClaimCredentials(secret, providerConfig(), role, secretvalue.FromString(password), "analytics", claim.UID)
	return secret
}

func TestEnsure_PreservesStampedPassword(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
	claim.UID = testClaimUID
	role := dbcontroller.RoleNameFor(claim.Namespace, claim.Name)
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
		roles:     map[string]bool{role: true},
	}
	existing := credentialsSecretOwnedBy(t, claim.Name, claim.Namespace, claim)
	dbcontroller.ApplyClaimCredentials(existing, providerConfig(), role, secretvalue.FromString("original-pw"), "analytics", claim.UID)
	cli := fakeClient(t, existing)

	secret, err := databaseclaim.DatabaseProvisioner{
		Client:         cli,
		Claim:          claim,
		Provider:       internalProvider(),
		Postgres:       pg,
		ProviderConfig: providerConfig(),
	}.Ensure(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.passwordSets).To(Equal(0), "a stamp-verified password must not be rotated")
	g.Expect(string(secret.Data[postgres.SecretKeyPassword])).To(Equal("original-pw"))
}

func TestEnsure_RotatesTamperedPassword(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
	claim.UID = testClaimUID
	role := dbcontroller.RoleNameFor(claim.Namespace, claim.Name)
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
		roles:     map[string]bool{role: true},
	}
	existing := credentialsSecretOwnedBy(t, claim.Name, claim.Namespace, claim)
	dbcontroller.ApplyClaimCredentials(existing, providerConfig(), role, secretvalue.FromString("original-pw"), "analytics", claim.UID)
	existing.Data[postgres.SecretKeyPassword] = []byte("tampered-pw")
	cli := fakeClient(t, existing)

	secret, err := databaseclaim.DatabaseProvisioner{
		Client:         cli,
		Claim:          claim,
		Provider:       internalProvider(),
		Postgres:       pg,
		ProviderConfig: providerConfig(),
	}.Ensure(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.passwordSets).To(Equal(1), "a password that fails stamp verification must be rotated")
	g.Expect(string(secret.Data[postgres.SecretKeyPassword])).NotTo(Equal("tampered-pw"))
}

func TestEnsure_MigratesUnstampedOwnedSecret(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
	claim.UID = testClaimUID
	role := dbcontroller.RoleNameFor(claim.Namespace, claim.Name)
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
		roles:     map[string]bool{role: true},
	}
	existing := credentialsSecretOwnedBy(t, claim.Name, claim.Namespace, claim)
	existing.Data = map[string][]byte{postgres.SecretKeyPassword: []byte("legacy-pw")}
	cli := fakeClient(t, existing)

	secret, err := databaseclaim.DatabaseProvisioner{
		Client:         cli,
		Claim:          claim,
		Provider:       internalProvider(),
		Postgres:       pg,
		ProviderConfig: providerConfig(),
	}.Ensure(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.passwordSets).To(Equal(0), "an owned Secret from before the stamp must keep its password")
	g.Expect(string(secret.Data[postgres.SecretKeyPassword])).To(Equal("legacy-pw"))
	g.Expect(secret.Annotations[dbcontroller.CredentialStampAnnotation]).NotTo(BeEmpty(),
		"the migration path must stamp the preserved password")
}

func TestEnsure_ReAdoptsStampedSecretWithStrippedOwnerRef(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
	claim.UID = testClaimUID
	role := dbcontroller.RoleNameFor(claim.Namespace, claim.Name)
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
		roles:     map[string]bool{role: true},
	}
	existing := stampedCredentialsSecret(t, claim, "original-pw")
	cli := fakeClient(t, existing)

	secret, err := databaseclaim.DatabaseProvisioner{
		Client:         cli,
		Claim:          claim,
		Provider:       internalProvider(),
		Postgres:       pg,
		ProviderConfig: providerConfig(),
	}.Ensure(context.Background())

	g.Expect(err).NotTo(HaveOccurred(), "a stamp-verified Secret must not be treated as foreign")
	g.Expect(pg.passwordSets).To(Equal(0))
	g.Expect(string(secret.Data[postgres.SecretKeyPassword])).To(Equal("original-pw"))
}

func TestEnsure_CreatesNamedDatabaseOnExternalWhenAllowed(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
	provider := &infraApi.DatabaseProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "external"},
		Spec: infraApi.DatabaseProviderSpec{
			Type: infraApi.ProviderTypeExternal,
			External: &infraApi.ExternalProviderSpec{
				Capabilities: []infraApi.ExternalCapability{
					infraApi.ExternalCapabilityCreateDatabase,
				},
			},
		},
	}
	pg := &trackingClient{cfg: providerConfig()}

	secret, err := databaseclaim.DatabaseProvisioner{
		Client:         fakeClient(t),
		Claim:          claim,
		Provider:       provider,
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory:  testDBClientFactory(pg),
	}.Ensure(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.createdDatabases).To(ContainElement("analytics"))
	g.Expect(claim.Status.DatabaseCreated).To(BeTrue())
	g.Expect(secret).NotTo(BeNil())
	g.Expect(string(secret.Data[postgres.SecretKeyDatabase])).To(Equal("analytics"))
}

func TestEnsure_DoesNotMarkDatabaseCreatedWhenPreexisting(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
	}

	_, err := databaseclaim.DatabaseProvisioner{
		Client:         fakeClient(t),
		Claim:          claim,
		Provider:       internalProvider(),
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory:  testDBClientFactory(pg),
	}.Ensure(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.createdDatabases).To(BeEmpty())
	g.Expect(claim.Status.DatabaseCreated).To(BeFalse())
}

func TestEnsure_ReadOnlyGrantsConnectWithoutCreate(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
	claim.Spec.Access = infraApi.AccessModeReadOnly
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
	}

	_, err := databaseclaim.DatabaseProvisioner{
		Client:         fakeClient(t),
		Claim:          claim,
		Provider:       internalProvider(),
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory:  testDBClientFactory(pg),
	}.Ensure(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.grantedConnect).To(Equal(1))
	g.Expect(pg.grantedCreate).To(Equal(0))
}

func TestEnsure_RejectsCredentialsSecretOwnedByOther(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
	claim.Name = "sc-foo"
	claim.UID = "databaseclaim-uid"
	foreignOwner := &infraApi.SchemaClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "foo",
			Namespace: claim.Namespace,
			UID:       "schemaclaim-uid",
		},
	}
	secretName := dbcontroller.SecretNameForDatabaseClaim(claim)
	g.Expect(secretName).To(Equal("sc-foo"))
	g.Expect(dbcontroller.SecretNameForSchemaClaim(foreignOwner)).To(Equal(secretName))
	foreignSecret := credentialsSecretOwnedBy(t, secretName, claim.Namespace, foreignOwner)
	originalUser := []byte("schema-role")
	foreignSecret.Data = map[string][]byte{postgres.SecretKeyUser: originalUser}
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
	}
	cli := fakeClient(t, foreignSecret)

	_, err := databaseclaim.DatabaseProvisioner{
		Client:         cli,
		Claim:          claim,
		Provider:       internalProvider(),
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory:  testDBClientFactory(pg),
	}.Ensure(context.Background())

	g.Expect(err).To(MatchError(claimerrors.SecretNotOwned{Secret: secretName}))
	g.Expect(pg.createdRoles).To(BeEmpty())
	stored := &corev1.Secret{}
	g.Expect(cli.Get(context.Background(), client.ObjectKeyFromObject(foreignSecret), stored)).To(Succeed())
	g.Expect(stored.Data[postgres.SecretKeyUser]).To(Equal(originalUser))
}

func TestEnsure_EmptyExtensionsSkipsTargetDatabaseClient(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
	}
	factoryCalls := 0
	factory := func(ctx context.Context, cfg postgres.Config) (postgres.Client, error) {
		factoryCalls++
		return testDBClientFactory(pg)(ctx, cfg)
	}

	_, err := databaseclaim.DatabaseProvisioner{
		Client:         fakeClient(t),
		Claim:          claim,
		Provider:       internalProvider(),
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory:  factory,
	}.Ensure(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(factoryCalls).To(Equal(0))
	g.Expect(pg.createdRoles).NotTo(BeEmpty())
}

func TestEnsure_InternalInstallsExtensionsBeforeRole(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
	claim.Spec.Extensions = []string{"vector"}
	pg := &trackingClient{
		cfg:       providerConfig(),
		databases: map[string]bool{"analytics": true},
	}

	_, err := databaseclaim.DatabaseProvisioner{
		Client:         fakeClient(t),
		Claim:          claim,
		Provider:       internalProvider(),
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory:  testDBClientFactory(pg),
	}.Ensure(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.execSQLsContainingCreateExtension("vector")).To(BeTrue())
	g.Expect(pg.createdRoles).NotTo(BeEmpty())
}

func TestEnsure_ExternalMissingExtensionReturnsNotAvailable(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
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

	_, err := databaseclaim.DatabaseProvisioner{
		Client:         fakeClient(t),
		Claim:          claim,
		Provider:       provider,
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory:  testDBClientFactory(pg),
	}.Ensure(context.Background())

	var missing claimerrors.ExtensionNotAvailable
	g.Expect(errors.As(err, &missing)).To(BeTrue())
	g.Expect(missing.Extensions).To(Equal([]string{"vector"}))
	g.Expect(pg.execSQLsContainingCreateExtension("vector")).To(BeFalse())
}

func TestEnsure_ExternalAvailableExtensionNeverCreates(t *testing.T) {
	g := NewWithT(t)
	claim := sampleClaim("analytics")
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
		extensionAvailableByName: map[string]bool{"vector": true},
	}

	_, err := databaseclaim.DatabaseProvisioner{
		Client:         fakeClient(t),
		Claim:          claim,
		Provider:       provider,
		Postgres:       pg,
		ProviderConfig: providerConfig(),
		ClientFactory:  testDBClientFactory(pg),
	}.Ensure(context.Background())

	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(pg.execSQLsContainingCreateExtension("vector")).To(BeFalse())
	g.Expect(pg.createdRoles).NotTo(BeEmpty())
}

func testDBClientFactory(pg *trackingClient) postgres.ClientFactory {
	return func(_ context.Context, cfg postgres.Config) (postgres.Client, error) {
		return &trackingClient{
			cfg:                      cfg,
			databases:                pg.databases,
			roles:                    pg.roles,
			extensionAvailableByName: pg.extensionAvailableByName,
			parent:                   pg,
		}, nil
	}
}

func sampleClaim(database string) *infraApi.DatabaseClaim {
	return &infraApi.DatabaseClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-a", Namespace: "app-ns"},
		Spec: infraApi.DatabaseClaimSpec{
			Provider: infraApi.ProviderRef{Name: "rhai-db"},
			Database: database,
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
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
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

type trackingClient struct {
	cfg                      postgres.Config
	databases                map[string]bool
	roles                    map[string]bool
	createdDatabases         []string
	createdRoles             []string
	passwordSets             int
	grantedConnect           int
	grantedCreate            int
	extensionAvailableByName map[string]bool
	execSQLs                 []string
	parent                   *trackingClient
}

func (t *trackingClient) root() *trackingClient {
	if t.parent != nil {
		return t.parent
	}
	return t
}

func (t *trackingClient) execSQLsContainingCreateExtension(name string) bool {
	root := t.root()
	for _, sql := range root.execSQLs {
		upper := strings.ToUpper(sql)
		if strings.Contains(upper, "CREATE EXTENSION") && strings.Contains(sql, name) {
			return true
		}
	}
	return false
}

func (t *trackingClient) Config() postgres.Config { return t.cfg }
func (t *trackingClient) Close()                  {}
func (t *trackingClient) Ping(context.Context) error {
	return nil
}

func (t *trackingClient) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	root := t.root()
	root.execSQLs = append(root.execSQLs, sql)
	if root.databases == nil {
		root.databases = map[string]bool{}
	}
	if root.roles == nil {
		root.roles = map[string]bool{}
	}
	switch {
	case strings.Contains(sql, "CREATE DATABASE"):
		name := lastQuotedIdent(sql)
		root.databases[name] = true
		root.createdDatabases = append(root.createdDatabases, name)
	case strings.Contains(sql, "CREATE ROLE"):
		name := firstQuotedIdent(sql)
		root.roles[name] = true
		root.createdRoles = append(root.createdRoles, name)
	case strings.Contains(sql, "ALTER ROLE") && strings.Contains(sql, "PASSWORD"):
		root.passwordSets++
	case strings.Contains(sql, "GRANT CONNECT ON DATABASE"):
		root.grantedConnect++
	case strings.Contains(sql, "GRANT CREATE ON DATABASE"):
		root.grantedCreate++
	}
	return pgconn.CommandTag{}, nil
}

func (t *trackingClient) Query(context.Context, string, ...any) (postgres.Rows, error) {
	return nil, fmt.Errorf("unexpected Query")
}

func (t *trackingClient) QueryRow(_ context.Context, sql string, args ...any) (pgx.Row, error) {
	root := t.root()
	if root.databases == nil {
		root.databases = map[string]bool{}
	}
	if root.roles == nil {
		root.roles = map[string]bool{}
	}
	switch {
	case strings.Contains(sql, "pg_database"):
		name, _ := args[0].(string)
		return boolRow{root.databases[name]}, nil
	case strings.Contains(sql, "pg_roles"):
		name, _ := args[0].(string)
		return boolRow{root.roles[name]}, nil
	case strings.Contains(sql, "HAS_DATABASE_PRIVILEGE"):
		return boolRow{false}, nil
	case strings.Contains(sql, "pg_extension"):
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

func lastQuotedIdent(sql string) string {
	start := -1
	end := -1
	for i, r := range sql {
		if r == '"' {
			if start < 0 {
				start = i + 1
			} else {
				end = i
			}
		}
	}
	if start >= 0 && end > start {
		return sql[start:end]
	}
	return ""
}

func firstQuotedIdent(sql string) string {
	start := -1
	for i, r := range sql {
		if r == '"' {
			if start < 0 {
				start = i + 1
			} else {
				return sql[start:i]
			}
		}
	}
	return ""
}
