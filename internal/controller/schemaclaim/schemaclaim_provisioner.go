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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/controller/claimerrors"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/resources/gvk"
	odhsecret "github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

// SchemaProvisioner provisions a dedicated schema and login role for a SchemaClaim.
type SchemaProvisioner struct {
	Client         client.Client
	Claim          *infraApi.SchemaClaim
	Provider       *infraApi.DatabaseProvider
	Postgres       postgres.Client
	ProviderConfig postgres.Config
	ClientFactory  postgres.ClientFactory
}

func (p SchemaProvisioner) Database() string {
	return dbcontroller.ResolveDatabase(p.Claim.Spec.Database, p.ProviderConfig)
}

func (p SchemaProvisioner) Schema() string {
	return dbcontroller.SchemaNameFor(p.Claim)
}

// ConnectionStatus returns the desired connection status for the claim.
func (p SchemaProvisioner) ConnectionStatus() infraApi.SchemaConnectionStatus {
	return infraApi.SchemaConnectionStatus{
		ConnectionStatus: infraApi.ConnectionStatus{
			SecretRef: corev1.LocalObjectReference{Name: dbcontroller.SecretNameForSchemaClaim(p.Claim)},
			Host:      p.ProviderConfig.Host,
			Port:      int32(p.ProviderConfig.Port),
		},
		Database: p.Database(),
		Schema:   p.Schema(),
	}
}

// Ensure validates the target database exists, creates the schema when
// permitted, provisions credentials, and returns the desired Secret.
// SchemaClaim never creates databases.
func (p SchemaProvisioner) Ensure(ctx context.Context) (*corev1.Secret, error) {
	database := p.Database()
	dbExists, err := postgres.DatabaseExists(ctx, p.Postgres, database)
	if err != nil {
		return nil, dbcontroller.WrapQuickRetry("checking database existence", err)
	}
	if !dbExists {
		return nil, claimerrors.DatabaseNotFound{Database: database}
	}

	// Reject a colliding credentials Secret before any schema/role work.
	// SchemaClaim "foo" and DatabaseClaim "sc-foo" both default to Secret "sc-foo".
	secret := &corev1.Secret{}
	secret.Name = dbcontroller.SecretNameForSchemaClaim(p.Claim)
	secret.Namespace = p.Claim.Namespace
	if err := p.Client.Get(ctx, client.ObjectKeyFromObject(secret), secret); client.IgnoreNotFound(err) != nil {
		return nil, fmt.Errorf("checking credentials Secret existence: %w", err)
	}
	role := dbcontroller.RoleNameForSchemaClaim(p.Claim.Namespace, p.Claim.Name)
	if err := dbcontroller.RejectForeignCredentialsSecret(secret, p.Claim.UID, role); err != nil {
		return nil, err
	}

	dbCfg := p.ProviderConfig
	dbCfg.DBName = database
	dbClient, err := dbcontroller.OpenClient(ctx, dbCfg, p.ClientFactory)
	if err != nil {
		return nil, dbcontroller.WrapQuickRetry("opening database client", err)
	}
	defer dbClient.Close()

	if err := dbcontroller.EnsureClaimExtensions(ctx, dbClient, p.Provider, p.Claim.Spec.Extensions); err != nil {
		return nil, dbcontroller.WrapQuickRetry("ensuring claim extensions", err)
	}

	schema := p.Schema()
	if err := p.ensureSchema(ctx, dbClient, schema); err != nil {
		return nil, err
	}

	roleExists, err := postgres.RoleExists(ctx, p.Postgres, role)
	if err != nil {
		return nil, dbcontroller.WrapQuickRetry("checking role existence", err)
	}

	secretExists := secret.ResourceVersion != ""
	access := p.Claim.Spec.Access
	if access == "" {
		access = infraApi.AccessModeReadWrite
	}

	switch {
	case !roleExists:
		pw, err := postgres.GeneratePassword(24)
		if err != nil {
			return nil, fmt.Errorf("generating password: %w", err)
		}
		if err := postgres.EnsureRole(ctx, p.Postgres, role, pw); err != nil {
			return nil, dbcontroller.WrapQuickRetry("creating role", err)
		}
		// EnsureRole is create-only: a concurrent create that loses on
		// DuplicateObject leaves the winner's password on the role. Force the
		// role to this reconcile's password so the Secret cannot diverge.
		if err := postgres.SetRolePassword(ctx, p.Postgres, role, pw); err != nil {
			return nil, dbcontroller.WrapQuickRetry("setting role password", err)
		}
		dbcontroller.ApplySchemaClaimCredentials(secret, p.ProviderConfig, role, pw, database, schema, p.Claim.UID)

	case !secretExists:
		pw, err := postgres.GeneratePassword(24)
		if err != nil {
			return nil, fmt.Errorf("generating password: %w", err)
		}
		if err := postgres.SetRolePassword(ctx, p.Postgres, role, pw); err != nil {
			return nil, dbcontroller.WrapQuickRetry("rotating role password", err)
		}
		dbcontroller.ApplySchemaClaimCredentials(secret, p.ProviderConfig, role, pw, database, schema, p.Claim.UID)

	default:
		// Keep a verifiable password; rotate anything else. The stamp rules
		// and the upgrade-migration trade-off are documented in
		// DatabaseProvisioner.Ensure.
		existingPassword := odhsecret.FromBytes(secret.Data[postgres.SecretKeyPassword])
		if existingPassword.IsZero() || (dbcontroller.HasCredentialStamp(secret) &&
			!dbcontroller.CredentialStampMatches(secret, p.Claim.UID, role)) {
			pw, genErr := postgres.GeneratePassword(24)
			if genErr != nil {
				return nil, fmt.Errorf("generating password: %w", genErr)
			}
			if err := postgres.SetRolePassword(ctx, p.Postgres, role, pw); err != nil {
				return nil, dbcontroller.WrapQuickRetry("rotating role password", err)
			}
			existingPassword = pw
		}
		dbcontroller.ApplySchemaClaimCredentials(secret, p.ProviderConfig, role, existingPassword, database, schema, p.Claim.UID)
	}

	// CONNECT on the database so the role can open a session; schema grants
	// bound access to the claim schema (never CREATE on the database).
	switch ok, err := postgres.HasDatabasePrivileges(ctx, p.Postgres, database, role, infraApi.AccessModeReadOnly); {
	case err != nil:
		return nil, dbcontroller.WrapQuickRetry("checking database privileges", err)
	case !ok:
		if err := postgres.GrantDatabasePrivileges(ctx, p.Postgres, database, role, infraApi.AccessModeReadOnly); err != nil {
			return nil, dbcontroller.WrapQuickRetry("granting database connect", err)
		}
	}

	// Always re-grant: GrantSchemaPrivileges is multi-statement and idempotent.
	// HasSchemaPrivileges only checks schema USAGE/CREATE, so a partial grant
	// (USAGE present, table/default privileges missing) would otherwise stick.
	if err := postgres.GrantSchemaPrivileges(ctx, dbClient, schema, role, access); err != nil {
		return nil, dbcontroller.WrapQuickRetry("granting schema privileges", err)
	}
	secret.SetGroupVersionKind(gvk.Secret)

	return secret, nil
}

// ensureSchema creates the claim schema when missing, or confirms ownership of
// an existing one. Reserved schemas and schemas this claim did not create are
// rejected. Ownership is proven by a PostgreSQL marker written with CREATE
// SCHEMA, so a failed status write or restart can recover only schemas this
// claim created — not admin-created or Retain leftovers.
func (p SchemaProvisioner) ensureSchema(ctx context.Context, dbClient postgres.Client, schema string) error {
	if dbcontroller.IsReservedSchema(schema) {
		return claimerrors.SchemaReserved{Schema: schema}
	}
	schemaExists, err := postgres.SchemaExists(ctx, dbClient, schema)
	if err != nil {
		return dbcontroller.WrapQuickRetry("checking schema existence", err)
	}
	database := p.Database()
	switch {
	case schemaExists && p.Claim.Status.SchemaCreated:
		// SchemaCreated is ownership of the recorded schema/database only.
		// A later Spec change that retargets SchemaNameFor / ResolveDatabase
		// must not inherit that flag (CEL also freezes Spec field presence).
		if recorded := p.Claim.Status.Schema; recorded != "" && recorded != schema {
			return claimerrors.SchemaNotOwned{Schema: schema}
		}
		if recordedDB := p.Claim.Status.Connection.Database; recordedDB != "" && recordedDB != database {
			return claimerrors.SchemaNotOwned{Schema: schema}
		}
		// Re-check the Postgres marker: the named schema may have been dropped
		// and replaced since SchemaCreated was set.
		ownerUID, err := postgres.SchemaOwnerClaimUID(ctx, dbClient, schema)
		if err != nil {
			return dbcontroller.WrapQuickRetry("reading schema ownership marker", err)
		}
		if ownerUID == "" || ownerUID != string(p.Claim.UID) {
			return claimerrors.SchemaNotOwned{Schema: schema}
		}
		return nil
	case schemaExists:
		return p.claimExistingSchema(ctx, dbClient, database, schema)
	default:
		if !dbcontroller.ExternalProviderAllows(p.Provider, infraApi.ExternalCapabilityCreateSchema) {
			return claimerrors.SchemaCreateNotAllowed{Schema: schema}
		}
		if err := postgres.CreateSchemaExclusiveWithOwner(
			ctx, dbClient, schema, string(p.Claim.UID),
		); err != nil {
			if postgres.IsDuplicateSchema(err) {
				return p.claimExistingSchema(ctx, dbClient, database, schema)
			}
			return dbcontroller.WrapQuickRetry("creating schema", err)
		}
		// Persist ownership before role/Secret work so a later failure cannot
		// leave the schema existing with SchemaCreated unset.
		return p.persistSchemaCreated(ctx, database, schema)
	}
}

// claimExistingSchema recovers ownership only when the PostgreSQL schema marker
// matches this claim's UID. Unmarked and foreign-owned schemas are rejected.
func (p SchemaProvisioner) claimExistingSchema(
	ctx context.Context,
	dbClient postgres.Client,
	database, schema string,
) error {
	ownerUID, err := postgres.SchemaOwnerClaimUID(ctx, dbClient, schema)
	if err != nil {
		return dbcontroller.WrapQuickRetry("reading schema ownership marker", err)
	}
	if ownerUID == "" || ownerUID != string(p.Claim.UID) {
		return claimerrors.SchemaNotOwned{Schema: schema}
	}
	return p.persistSchemaCreated(ctx, database, schema)
}

// persistSchemaCreated retries Get + Status().Update for conflicts and other
// retryable API errors. Each attempt re-fetches and checks database/schema
// still match the CreateSchema targets. SchemaNotOwned handling is unchanged.
func (p SchemaProvisioner) persistSchemaCreated(ctx context.Context, database, schema string) error {
	err := retry.OnError(retry.DefaultBackoff, func(err error) bool {
		return apierrors.IsConflict(err) ||
			apierrors.IsServerTimeout(err) ||
			apierrors.IsTimeout(err) ||
			apierrors.IsTooManyRequests(err) ||
			apierrors.IsServiceUnavailable(err) ||
			apierrors.IsInternalError(err)
	}, func() error {
		fresh := &infraApi.SchemaClaim{}
		if err := p.Client.Get(ctx, client.ObjectKeyFromObject(p.Claim), fresh); err != nil {
			return err
		}
		if dbcontroller.ResolveDatabase(fresh.Spec.Database, p.ProviderConfig) != database ||
			dbcontroller.SchemaNameFor(fresh) != schema {
			return fmt.Errorf("claim database/schema changed after creating schema %q", schema)
		}
		if fresh.Status.SchemaCreated &&
			fresh.Status.Schema == schema &&
			(fresh.Status.Connection.Database == "" || fresh.Status.Connection.Database == database) {
			*p.Claim = *fresh
			return nil
		}
		fresh.Status.SchemaCreated = true
		fresh.Status.Schema = schema
		fresh.Status.Connection.Database = database
		if err := p.Client.Status().Update(ctx, fresh); err != nil {
			return err
		}
		*p.Claim = *fresh
		return nil
	})
	if err != nil {
		return dbcontroller.WrapQuickRetry("persisting schemaCreated status", err)
	}
	return nil
}
