package controller_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	. "github.com/onsi/gomega"
	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	odhtypes "github.com/opendatahub-io/odh-platform-utilities/framework/controller/types"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/yaml"

	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/controller/claimerrors"
	dbmetrics "github.com/opendatahub-io/opendatahub-db-operator/pkg/metrics"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
	secretvalue "github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

func TestNewClient_LoadsProviderConfigAndUsesFactory(t *testing.T) {
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
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "external-admin",
			Namespace: "db-admin",
		},
		Data: map[string][]byte{
			postgres.SecretKeyHost:     []byte("postgres.db-admin.svc"),
			postgres.SecretKeyPort:     []byte("5432"),
			postgres.SecretKeyUser:     []byte("postgres"),
			postgres.SecretKeyPassword: []byte("secret"),
			postgres.SecretKeyDatabase: []byte("postgres"),
		},
	}

	cli := fake.NewClientBuilder().
		WithScheme(providerConfigScheme()).
		WithObjects(secret).
		Build()

	var gotConfig postgres.Config
	client, cfg, err := controller.NewClient(
		context.Background(),
		cli,
		provider,
		&moduleconfig.Config{OperatorNamespace: "odh-db-operator-system"},
		func(_ context.Context, cfg postgres.Config) (postgres.Client, error) {
			gotConfig = cfg
			return stubClient{config: cfg}, nil
		},
	)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(cfg).To(Equal(postgres.Config{
		Host:     "postgres.db-admin.svc",
		Port:     postgres.DefaultPort,
		User:     "postgres",
		Password: secretvalue.FromString("secret"),
		DBName:   "postgres",
		SSLMode:  postgres.SSLModeRequire,
	}))
	g.Expect(gotConfig).To(Equal(cfg))
	g.Expect(client.Config()).To(Equal(cfg))
}

// TestOpenClient_KeepsProjectedFailureClassification covers the cleanup fallback
// for a database that disappears mid-flight: the SQLSTATE must survive this
// helper's wrapping, because that is what tells schemaclaim cleanup to treat the
// database as already gone instead of failing the deletion.
func TestOpenClient_KeepsProjectedFailureClassification(t *testing.T) {
	g := NewWithT(t)

	projected := postgres.ProjectError(
		"ping",
		&pgconn.PgError{Code: pgerrcode.InvalidCatalogName, Message: `database "analytics" does not exist`},
		secretvalue.FromString("admin-password"),
	)

	_, err := controller.OpenClient(
		context.Background(),
		postgres.Config{DBName: "analytics"},
		func(context.Context, postgres.Config) (postgres.Client, error) {
			return nil, projected
		},
	)

	g.Expect(err).To(HaveOccurred())
	g.Expect(postgres.IsInvalidCatalogName(err)).To(BeTrue(), "the cleanup fallback depends on this")
	g.Expect(err.Error()).To(ContainSubstring("analytics"))
	g.Expect(err.Error()).NotTo(ContainSubstring("admin-password"))
}

type stubClient struct {
	config postgres.Config
}

func (s stubClient) Config() postgres.Config  { return s.config }
func (stubClient) Close()                     {}
func (stubClient) Ping(context.Context) error { return nil }
func (stubClient) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (stubClient) Query(context.Context, string, ...any) (postgres.Rows, error) {
	return nil, nil
}
func (stubClient) QueryRow(context.Context, string, ...any) (pgx.Row, error) {
	return nil, nil
}

func TestMarkClaimProvisioned_UpdatesMetric(t *testing.T) {
	g := NewWithT(t)
	t.Cleanup(func() { dbmetrics.PruneClaimMetrics(nil) })
	dbmetrics.PruneClaimMetrics(nil)

	claim := &infraApi.SchemaClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "team-a"},
	}
	aggregator, err := conditions.NewAggregator(
		fwapi.ConditionTypeReady,
		conditions.Dependent(fwapi.ConditionType(controller.ConditionProvisioned), conditions.HealthyWhenTrue),
	)
	g.Expect(err).NotTo(HaveOccurred())
	rr := &odhtypes.ReconciliationRequest{
		Instance:   claim,
		Conditions: conditions.NewManager(claim, aggregator),
	}

	controller.MarkClaimProvisioned(rr, metav1.ConditionFalse,
		conditions.WithReason("ExtensionNotAvailable"),
		conditions.WithMessage(`required extension "vector" is not available`))
	g.Expect(testutil.GatherAndCompare(ctrlmetrics.Registry, strings.NewReader(`
# HELP dbclaim_provisioned Whether a DatabaseClaim or SchemaClaim is provisioned
# TYPE dbclaim_provisioned gauge
dbclaim_provisioned{kind="SchemaClaim",name="my-app",namespace="team-a"} 0
`), "dbclaim_provisioned")).To(Succeed())
	provisioned := conditions.FindStatusCondition(claim, controller.ConditionProvisioned)
	g.Expect(provisioned).NotTo(BeNil())
	g.Expect(provisioned.Status).To(Equal(metav1.ConditionFalse))
	g.Expect(provisioned.Reason).To(Equal("ExtensionNotAvailable"))

	controller.MarkClaimProvisioned(rr, metav1.ConditionTrue,
		conditions.WithReason("SchemaProvisioned"),
		conditions.WithMessage("provisioned"))
	g.Expect(conditions.FindStatusCondition(claim, controller.ConditionProvisioned).Status).To(Equal(metav1.ConditionTrue))
	g.Expect(testutil.GatherAndCompare(ctrlmetrics.Registry, strings.NewReader(`
# HELP dbclaim_provisioned Whether a DatabaseClaim or SchemaClaim is provisioned
# TYPE dbclaim_provisioned gauge
dbclaim_provisioned{kind="SchemaClaim",name="my-app",namespace="team-a"} 1
`), "dbclaim_provisioned")).To(Succeed())
}

func TestEnsureClaimExtensions_EmptyNoop(t *testing.T) {
	g := NewWithT(t)
	cli := &recordingClient{installedByName: map[string]bool{"vector": true}}
	provider := &infraApi.DatabaseProvider{
		Spec: infraApi.DatabaseProviderSpec{Type: infraApi.ProviderTypeInternal},
	}
	g.Expect(controller.EnsureClaimExtensions(context.Background(), cli, provider, nil)).To(Succeed())
	g.Expect(cli.execSQLs).To(BeEmpty())
}

func TestEnsureClaimExtensions_InternalInstalls(t *testing.T) {
	g := NewWithT(t)
	cli := &recordingClient{}
	provider := &infraApi.DatabaseProvider{
		Spec: infraApi.DatabaseProviderSpec{Type: infraApi.ProviderTypeInternal},
	}
	g.Expect(controller.EnsureClaimExtensions(context.Background(), cli, provider, []string{"vector"})).To(Succeed())
	g.Expect(cli.execSQLs).To(HaveLen(1))
	g.Expect(strings.ToUpper(cli.execSQLs[0])).To(ContainSubstring("CREATE EXTENSION"))
	g.Expect(cli.execSQLs[0]).To(ContainSubstring("vector"))
}

func TestEnsureClaimExtensions_ExternalInstalledNoCreate(t *testing.T) {
	g := NewWithT(t)
	cli := &recordingClient{installedByName: map[string]bool{"vector": true}}
	provider := &infraApi.DatabaseProvider{
		Spec: infraApi.DatabaseProviderSpec{Type: infraApi.ProviderTypeExternal},
	}
	g.Expect(controller.EnsureClaimExtensions(context.Background(), cli, provider, []string{"vector"})).To(Succeed())
	g.Expect(cli.execSQLs).To(BeEmpty())
	for _, sql := range cli.execSQLs {
		g.Expect(strings.ToUpper(sql)).NotTo(ContainSubstring("CREATE EXTENSION"))
	}
}

func TestEnsureClaimExtensions_ExternalMissingNamedError(t *testing.T) {
	g := NewWithT(t)
	cli := &recordingClient{installedByName: map[string]bool{"vector": false}}
	provider := &infraApi.DatabaseProvider{
		Spec: infraApi.DatabaseProviderSpec{Type: infraApi.ProviderTypeExternal},
	}
	err := controller.EnsureClaimExtensions(context.Background(), cli, provider, []string{"vector"})
	var missing claimerrors.ExtensionNotAvailable
	g.Expect(errors.As(err, &missing)).To(BeTrue())
	g.Expect(missing.Extensions).To(Equal([]string{"vector"}))
	g.Expect(cli.execSQLs).To(BeEmpty())
	for _, sql := range cli.execSQLs {
		g.Expect(strings.ToUpper(sql)).NotTo(ContainSubstring("CREATE EXTENSION"))
	}
}

func TestEnsureClaimExtensions_UnsupportedRejectedBeforePostgres(t *testing.T) {
	g := NewWithT(t)
	cli := &recordingClient{installedByName: map[string]bool{"vector": false, "postgis": false}}
	provider := &infraApi.DatabaseProvider{
		Spec: infraApi.DatabaseProviderSpec{Type: infraApi.ProviderTypeInternal},
	}
	err := controller.EnsureClaimExtensions(context.Background(), cli, provider, []string{"vector", "postgis"})
	var unsupported claimerrors.ExtensionNotSupported
	g.Expect(errors.As(err, &unsupported)).To(BeTrue())
	g.Expect(unsupported.Extensions).To(Equal([]string{"postgis"}))
	g.Expect(err).To(MatchError(ContainSubstring(`extension "postgis" is not supported`)))
	g.Expect(cli.execSQLs).To(BeEmpty())
	g.Expect(cli.queryRowArgs).To(BeNil())
}

func TestEnsureClaimExtensions_NilProviderRejected(t *testing.T) {
	g := NewWithT(t)
	cli := &recordingClient{}
	err := controller.EnsureClaimExtensions(context.Background(), cli, nil, []string{"vector"})
	g.Expect(err).To(MatchError("provider is required for extension verification"))
}

func TestValidateClaimExtensions_EmptyAndSupported(t *testing.T) {
	g := NewWithT(t)
	g.Expect(controller.ValidateClaimExtensions(nil)).To(Succeed())
	g.Expect(controller.ValidateClaimExtensions([]string{})).To(Succeed())
	g.Expect(controller.ValidateClaimExtensions([]string{"vector"})).To(Succeed())
}

func TestValidateClaimExtensions_UnsupportedNamed(t *testing.T) {
	g := NewWithT(t)
	err := controller.ValidateClaimExtensions([]string{"postgis"})
	var unsupported claimerrors.ExtensionNotSupported
	g.Expect(errors.As(err, &unsupported)).To(BeTrue())
	g.Expect(unsupported.Extensions).To(Equal([]string{"postgis"}))
	g.Expect(err).To(MatchError(`extension "postgis" is not supported`))
}

func TestValidateClaimExtensions_MixedReportsOnlyUnsupported(t *testing.T) {
	g := NewWithT(t)
	err := controller.ValidateClaimExtensions([]string{"vector", "postgis", "uuid-ossp"})
	var unsupported claimerrors.ExtensionNotSupported
	g.Expect(errors.As(err, &unsupported)).To(BeTrue())
	g.Expect(unsupported.Extensions).To(Equal([]string{"postgis", "uuid-ossp"}))
	g.Expect(err).To(MatchError(ContainSubstring("postgis, uuid-ossp")))
}

func TestSupportedClaimExtensions_MatchClaimCRDEnum(t *testing.T) {
	g := NewWithT(t)
	_, thisFile, _, ok := runtime.Caller(0)
	g.Expect(ok).To(BeTrue())
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "../.."))

	for _, crdFile := range []string{
		"infrastructure.opendatahub.io_databaseclaims.yaml",
		"infrastructure.opendatahub.io_schemaclaims.yaml",
	} {
		path := filepath.Join(repoRoot, "config/crd/bases", crdFile)
		raw, err := os.ReadFile(path)
		g.Expect(err).NotTo(HaveOccurred(), path)
		var doc map[string]any
		g.Expect(yaml.Unmarshal(raw, &doc)).To(Succeed(), path)
		enum := claimExtensionsEnum(g, doc)
		g.Expect(enum).To(Equal(controller.SupportedClaimExtensions), path)
	}
}

type recordingClient struct {
	installedByName map[string]bool
	execSQLs        []string
	queryRowArgs    []any
}

func (c *recordingClient) Config() postgres.Config { return postgres.Config{} }
func (c *recordingClient) Close()                  {}
func (c *recordingClient) Ping(context.Context) error {
	return nil
}
func (c *recordingClient) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	c.execSQLs = append(c.execSQLs, sql)
	return pgconn.CommandTag{}, nil
}
func (c *recordingClient) Query(context.Context, string, ...any) (postgres.Rows, error) {
	return nil, nil
}
func (c *recordingClient) QueryRow(_ context.Context, _ string, args ...any) (pgx.Row, error) {
	c.queryRowArgs = append(c.queryRowArgs, args...)
	if len(args) > 0 {
		if names, ok := args[0].([]string); ok {
			var installed []string
			for _, name := range names {
				if c.installedByName[name] {
					installed = append(installed, name)
				}
			}
			return recordingNamesRow{names: installed}, nil
		}
	}
	installed := false
	if c.installedByName != nil && len(args) > 0 {
		if name, ok := args[0].(string); ok {
			installed = c.installedByName[name]
		}
	}
	return recordingBoolRow{installed: installed}, nil
}

func claimExtensionsEnum(g Gomega, doc map[string]any) []string {
	g.Expect(doc).To(HaveKey("spec"))
	spec, ok := doc["spec"].(map[string]any)
	g.Expect(ok).To(BeTrue())
	versions, ok := spec["versions"].([]any)
	g.Expect(ok).To(BeTrue())
	g.Expect(versions).NotTo(BeEmpty())
	version, ok := versions[0].(map[string]any)
	g.Expect(ok).To(BeTrue())
	schema, ok := version["schema"].(map[string]any)
	g.Expect(ok).To(BeTrue())
	openAPI, ok := schema["openAPIV3Schema"].(map[string]any)
	g.Expect(ok).To(BeTrue())
	props, ok := openAPI["properties"].(map[string]any)
	g.Expect(ok).To(BeTrue())
	specProp, ok := props["spec"].(map[string]any)
	g.Expect(ok).To(BeTrue())
	specProps, ok := specProp["properties"].(map[string]any)
	g.Expect(ok).To(BeTrue())
	extensions, ok := specProps["extensions"].(map[string]any)
	g.Expect(ok).To(BeTrue())
	items, ok := extensions["items"].(map[string]any)
	g.Expect(ok).To(BeTrue())
	rawEnum, ok := items["enum"].([]any)
	g.Expect(ok).To(BeTrue())
	enum := make([]string, 0, len(rawEnum))
	for _, v := range rawEnum {
		s, ok := v.(string)
		g.Expect(ok).To(BeTrue())
		enum = append(enum, s)
	}
	return enum
}

type recordingBoolRow struct {
	installed bool
}

func (r recordingBoolRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("expected one scan destination")
	}
	ptr, ok := dest[0].(*bool)
	if !ok {
		return errors.New("expected *bool scan destination")
	}
	*ptr = r.installed
	return nil
}

type recordingNamesRow struct {
	names []string
}

func (r recordingNamesRow) Scan(dest ...any) error {
	if len(dest) != 1 {
		return errors.New("expected one scan destination")
	}
	ptr, ok := dest[0].(*[]string)
	if !ok {
		return errors.New("expected *[]string scan destination")
	}
	*ptr = append([]string(nil), r.names...)
	return nil
}
