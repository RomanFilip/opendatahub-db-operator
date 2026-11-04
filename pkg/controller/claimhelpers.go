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

package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"regexp"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	odherrors "github.com/opendatahub-io/odh-platform-utilities/framework/controller/actions/errors"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	odhtypes "github.com/opendatahub-io/odh-platform-utilities/framework/controller/types"
	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/controller/claimerrors"
	dbmetrics "github.com/opendatahub-io/opendatahub-db-operator/pkg/metrics"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
)

const (
	// maxRoleNameLen is PostgreSQL's identifier length limit.
	maxRoleNameLen = 63
	// maxSchemaLen is PostgreSQL's identifier length limit for schema names.
	maxSchemaLen = 63
)

var nonIdentRe = regexp.MustCompile(`[^a-z0-9_]`)

// RoleNameFor derives a deterministic, PostgreSQL-safe role name for a
// DatabaseClaim from namespace and name. A short hash of the unsanitized
// identity is always appended so distinct claim names that sanitize to the
// same string (e.g. my-db vs my.db) cannot collide onto one role.
//
// SchemaClaim must use RoleNameForSchemaClaim so the two kinds never share a
// role when namespace and name match.
func RoleNameFor(namespace, name string) string {
	return roleNameFromRaw(fmt.Sprintf("%s_%s", namespace, name))
}

// RoleNameForSchemaClaim derives a PostgreSQL role name for a SchemaClaim.
// The "sc_" kind prefix keeps it distinct from RoleNameFor for a DatabaseClaim
// with the same namespace and name.
func RoleNameForSchemaClaim(namespace, name string) string {
	return roleNameFromRaw(fmt.Sprintf("sc_%s_%s", namespace, name))
}

func roleNameFromRaw(raw string) string {
	safe := nonIdentRe.ReplaceAllString(raw, "_")
	h := fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))[:8]
	baseMax := maxRoleNameLen - 9 // "_" + 8 hex
	if len(safe) > baseMax {
		safe = safe[:baseMax]
	}
	return safe + "_" + h
}

// SchemaNameFor returns the effective schema for a SchemaClaim: spec.schema
// when set (CEL already enforces pattern/length), otherwise a default derived
// from namespace and name. A short hash of the unsanitized identity is always
// appended so distinct claims that sanitize to the same string (e.g. a-b/c vs
// a/b-c) cannot collide onto one schema.
func SchemaNameFor(claim *infraApi.SchemaClaim) string {
	if claim == nil {
		return ""
	}
	if claim.Spec.Schema != "" {
		return claim.Spec.Schema
	}

	raw := strings.ToLower(claim.Namespace + "_" + claim.Name)
	safe := nonIdentRe.ReplaceAllString(raw, "_")
	h := fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))[:8]
	baseMax := maxSchemaLen - 9 // "_" + 8 hex
	if len(safe) > baseMax {
		safe = safe[:baseMax]
	}
	return safe + "_" + h
}

// NewClient loads the provider's admin connection config and opens a postgres.Client.
// Extracted from the identical openPool helpers in schemaclaim and databaseclaim.
// See LoadProviderConfig for the Secret read path.
func NewClient(
	ctx context.Context,
	reader client.Reader,
	provider *infraApi.DatabaseProvider,
	cfg *moduleconfig.Config,
	factory postgres.ClientFactory,
) (postgres.Client, postgres.Config, error) {
	providerCfg, err := LoadProviderConfig(ctx, reader, provider, cfg.OperatorNamespace)
	if err != nil {
		return nil, postgres.Config{}, err
	}
	providerCfg = cfg.WithPostgresTracing(providerCfg)
	dbClient, err := OpenClient(ctx, providerCfg, factory)
	return dbClient, providerCfg, err
}

func OpenClient(
	ctx context.Context,
	providerCfg postgres.Config,
	factory postgres.ClientFactory,
) (postgres.Client, error) {
	if factory == nil {
		factory = postgres.DefaultClientFactory
	}
	dbClient, err := factory(ctx, providerCfg)
	if err != nil {
		return nil, fmt.Errorf("opening postgres client: %w", err)
	}
	return dbClient, nil
}

func ResolveDatabase(
	override string,
	providerCfg postgres.Config,
) string {
	if override != "" {
		return override
	}
	return providerCfg.DBName
}

func ExternalProviderAllows(
	provider *infraApi.DatabaseProvider,
	capability infraApi.ExternalCapability,
) bool {
	if provider == nil {
		return false
	}
	if provider.Spec.Type != infraApi.ProviderTypeExternal {
		return true
	}
	if provider.Spec.External == nil {
		return false
	}
	return slices.Contains(provider.Spec.External.Capabilities, capability)
}

// SupportedClaimExtensions is the platform allow-list for claim
// spec.extensions. EnsureClaimExtensions validates against this list; keep it
// aligned with claim CRD Enum=vector (see TestSupportedClaimExtensions_MatchClaimCRDEnum).
var SupportedClaimExtensions = []string{"vector"}

// ValidateClaimExtensions rejects extension names outside
// SupportedClaimExtensions. Empty or nil input is allowed. Unsupported names
// are reported in input order via claimerrors.ExtensionNotSupported.
func ValidateClaimExtensions(extensions []string) error {
	if len(extensions) == 0 {
		return nil
	}
	supported := make(map[string]struct{}, len(SupportedClaimExtensions))
	for _, name := range SupportedClaimExtensions {
		supported[name] = struct{}{}
	}
	var unsupported []string
	for _, name := range extensions {
		if _, ok := supported[name]; !ok {
			unsupported = append(unsupported, name)
		}
	}
	if len(unsupported) == 0 {
		return nil
	}
	return claimerrors.ExtensionNotSupported{Extensions: unsupported}
}

// MarkClaimProvisioned updates the Provisioned condition and the matching
// dbclaim_provisioned gauge for DatabaseClaim and SchemaClaim instances.
func MarkClaimProvisioned(
	rr *odhtypes.ReconciliationRequest,
	status metav1.ConditionStatus,
	opts ...conditions.Option,
) {
	rr.Conditions.Mark(ConditionProvisioned, status, opts...)
	switch obj := rr.Instance.(type) {
	case *infraApi.DatabaseClaim:
		dbmetrics.SetClaimProvisioned(infraApi.DatabaseClaimKind, obj.Namespace, obj.Name, status == metav1.ConditionTrue)
	case *infraApi.SchemaClaim:
		dbmetrics.SetClaimProvisioned(infraApi.SchemaClaimKind, obj.Namespace, obj.Name, status == metav1.ConditionTrue)
	}
}

// StopIfUnsupportedClaimExtensions marks Provisioned false when extensions
// are outside SupportedClaimExtensions and returns a StopError so claim
// actions never open Postgres. Nil/empty or supported lists return nil.
func StopIfUnsupportedClaimExtensions(rr *odhtypes.ReconciliationRequest, extensions []string) error {
	if err := ValidateClaimExtensions(extensions); err != nil {
		MarkClaimProvisioned(rr, metav1.ConditionFalse,
			conditions.WithReason(ReasonExtensionNotSupported),
			conditions.WithMessage(err.Error()))
		return odherrors.NewStopErrorW(err)
	}
	return nil
}

// EnsureClaimExtensions installs claim-required extensions on Internal
// providers and verifies them against pg_extension in the connected database
// on External (and any non-Internal) providers. External paths never issue
// CREATE EXTENSION. Names are validated against SupportedClaimExtensions
// before any Postgres work.
func EnsureClaimExtensions(
	ctx context.Context,
	cli postgres.Client,
	provider *infraApi.DatabaseProvider,
	extensions []string,
) error {
	if len(extensions) == 0 {
		return nil
	}
	if err := ValidateClaimExtensions(extensions); err != nil {
		return err
	}
	if provider == nil {
		return fmt.Errorf("provider is required for extension verification")
	}

	if provider.Spec.Type == infraApi.ProviderTypeInternal {
		if err := postgres.CreateExtensionIfNotExists(ctx, cli, extensions); err != nil {
			return fmt.Errorf("installing claim extensions: %w", err)
		}
		return nil
	}

	missing, err := postgres.MissingExtensions(ctx, cli, extensions)
	if err != nil {
		return fmt.Errorf("verifying claim extensions: %w", err)
	}
	if len(missing) == 0 {
		return nil
	}
	return claimerrors.ExtensionNotAvailable{Extensions: missing}
}

// WrapQuickRetry wraps err with an operation label and upgrades it to a
// quick-retry signal when it's a connection-refused error. Extracted from the
// identical wrapQuickRetry helpers in schemaclaim and databaseclaim.
func WrapQuickRetry(op string, err error) error {
	if err == nil {
		return nil
	}
	wrapped := fmt.Errorf("%s: %w", op, err)
	if retryErr := StopWithQuickRetryIfConnectionRefused(wrapped); retryErr != nil {
		return retryErr
	}
	return wrapped
}

// ClearRoleOwnershipInDatabase opens an admin connection to database and
// reassigns/drops objects owned by role so DropRole can succeed. No-op when
// database is missing.
func ClearRoleOwnershipInDatabase(
	ctx context.Context,
	adminClient postgres.Client,
	adminCfg postgres.Config,
	factory postgres.ClientFactory,
	database string,
	role string,
) error {
	if database == "" || role == "" {
		return nil
	}
	exists, err := postgres.DatabaseExists(ctx, adminClient, database)
	if err != nil {
		return fmt.Errorf("checking database %q before ownership cleanup: %w", database, err)
	}
	if !exists {
		return nil
	}

	dbCfg := adminCfg
	dbCfg.DBName = database
	dbClient, err := OpenClient(ctx, dbCfg, factory)
	if err != nil {
		if postgres.IsInvalidCatalogName(err) {
			// The database went away between the check above and this open.
			return nil
		}

		return fmt.Errorf("opening postgres client for database %q: %w", database, err)
	}
	defer dbClient.Close()

	newOwner := adminCfg.User
	if newOwner == "" {
		newOwner = dbClient.Config().User
	}
	if err := postgres.ClearRoleOwnedObjects(ctx, dbClient, role, newOwner); err != nil {
		return err
	}
	return nil
}

// ShouldDropClaimDatabase reports whether cleanup may DROP the claim database.
// Requires DeletionPolicy=Delete, status.databaseCreated, a named non-default
// database, no other DatabaseClaim still referencing it on the same provider,
// and no live SchemaClaim still targeting that database (SchemaClaims nest
// schemas inside it; dropping would destroy their data while they remain active).
// SchemaClaims with DeletionTimestamp set are ignored so concurrent deletes do
// not block each other.
func ShouldDropClaimDatabase(
	claim *infraApi.DatabaseClaim,
	providerDefaultDB string,
	others []infraApi.DatabaseClaim,
	schemaClaims []infraApi.SchemaClaim,
) bool {
	if claim == nil {
		return false
	}
	if claim.Spec.DeletionPolicy != infraApi.DeletionPolicyDelete {
		return false
	}
	if !claim.Status.DatabaseCreated {
		return false
	}
	if claim.Spec.Database == "" {
		return false
	}
	database := claim.Status.Database
	if database == "" {
		database = claim.Spec.Database
	}
	if database == "" || database == providerDefaultDB {
		return false
	}
	provider := claim.Status.Provider
	if provider == "" {
		provider = claim.Spec.Provider.Name
	}
	for i := range others {
		other := &others[i]
		if other.UID == claim.UID {
			continue
		}
		if other.Namespace == claim.Namespace && other.Name == claim.Name {
			continue
		}
		otherProvider := other.Status.Provider
		if otherProvider == "" {
			otherProvider = other.Spec.Provider.Name
		}
		if otherProvider != provider {
			continue
		}
		otherDB := other.Status.Database
		if otherDB == "" {
			otherDB = other.Spec.Database
		}
		if otherDB == database {
			return false
		}
	}
	for i := range schemaClaims {
		sc := &schemaClaims[i]
		if !sc.DeletionTimestamp.IsZero() {
			continue
		}
		scProvider := sc.Status.Provider
		if scProvider == "" {
			scProvider = sc.Spec.Provider.Name
		}
		if scProvider != provider {
			continue
		}
		if schemaClaimDatabase(sc, providerDefaultDB) == database {
			return false
		}
	}
	return true
}

// ListDatabaseClaims returns all DatabaseClaims visible to cli.
func ListDatabaseClaims(ctx context.Context, cli client.Client) ([]infraApi.DatabaseClaim, error) {
	list := &infraApi.DatabaseClaimList{}
	if err := cli.List(ctx, list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// IsReservedSchema reports PostgreSQL system / default schemas that a claim
// must never DROP SCHEMA CASCADE, even if Status.SchemaCreated is somehow set.
func IsReservedSchema(schema string) bool {
	switch strings.ToLower(schema) {
	case "public", "pg_catalog", "information_schema", "pg_toast":
		return true
	default:
		return strings.HasPrefix(strings.ToLower(schema), "pg_")
	}
}

// ShouldDropClaimSchema reports whether cleanup may DROP SCHEMA CASCADE.
// Requires DeletionPolicy=Delete, status.schemaCreated, a non-reserved schema,
// and no other live SchemaClaim still referencing the same schema on the same
// provider database. Peers with DeletionTimestamp set are ignored so concurrent
// deletes of shared-schema claims do not block each other. providerDefaultDB is
// applied when a claim omits spec.database, matching ResolveDatabase /
// cleanupAction.
func ShouldDropClaimSchema(
	claim *infraApi.SchemaClaim,
	providerDefaultDB string,
	others []infraApi.SchemaClaim,
) bool {
	if claim == nil {
		return false
	}
	if claim.Spec.DeletionPolicy != infraApi.DeletionPolicyDelete {
		return false
	}
	if !claim.Status.SchemaCreated {
		return false
	}
	schema := claim.Status.Schema
	if schema == "" {
		schema = SchemaNameFor(claim)
	}
	if schema == "" || IsReservedSchema(schema) {
		return false
	}
	database := schemaClaimDatabase(claim, providerDefaultDB)
	provider := claim.Status.Provider
	if provider == "" {
		provider = claim.Spec.Provider.Name
	}
	for i := range others {
		other := &others[i]
		if other.UID == claim.UID {
			continue
		}
		if !other.DeletionTimestamp.IsZero() {
			continue
		}
		otherProvider := other.Status.Provider
		if otherProvider == "" {
			otherProvider = other.Spec.Provider.Name
		}
		if otherProvider != provider {
			continue
		}
		otherSchema := other.Status.Schema
		if otherSchema == "" {
			otherSchema = SchemaNameFor(other)
		}
		if otherSchema != schema {
			continue
		}
		if schemaClaimDatabase(other, providerDefaultDB) == database {
			return false
		}
	}
	return true
}

// schemaClaimDatabase returns the effective database for share/drop checks:
// status.connection.database when set, otherwise Spec.Database resolved
// against the provider default (empty Spec means the default).
func schemaClaimDatabase(claim *infraApi.SchemaClaim, providerDefaultDB string) string {
	if claim.Status.Connection.Database != "" {
		return claim.Status.Connection.Database
	}
	return ResolveDatabase(claim.Spec.Database, postgres.Config{DBName: providerDefaultDB})
}

// ListSchemaClaims returns all SchemaClaims visible to cli.
func ListSchemaClaims(ctx context.Context, cli client.Client) ([]infraApi.SchemaClaim, error) {
	list := &infraApi.SchemaClaimList{}
	if err := cli.List(ctx, list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// RejectForeignCredentialsSecret returns SecretNotOwned when secret already
// exists and cannot be shown to belong to this claim. A missing Secret is
// fine. Ownership is established by the controller ownerRef, or — when the
// ownerRef was stripped after provisioning — by a matching credential stamp
// (the provisioner then preserves the Secret and provisionAction re-stamps
// the ownerRef). SchemaClaim "foo" and DatabaseClaim "sc-foo" both default to
// Secret "sc-foo"; rejecting early avoids Postgres role/password work on
// another claim's Secret.
func RejectForeignCredentialsSecret(secret *corev1.Secret, claimUID types.UID, role string) error {
	if secret == nil || secret.ResourceVersion == "" {
		return nil
	}
	owner := metav1.GetControllerOf(secret)
	if owner != nil && owner.UID == claimUID {
		return nil
	}
	if owner == nil && CredentialStampMatches(secret, claimUID, role) {
		return nil
	}
	return claimerrors.SecretNotOwned{Secret: secret.Name}
}
