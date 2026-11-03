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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/controller/claimerrors"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/resources/gvk"
	odhsecret "github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

// DatabaseProvisioner provisions database-scoped credentials for a DatabaseClaim.
type DatabaseProvisioner struct {
	Client         client.Client
	Claim          *infraApi.DatabaseClaim
	Provider       *infraApi.DatabaseProvider
	Postgres       postgres.Client
	ProviderConfig postgres.Config
	ClientFactory  postgres.ClientFactory
}

func (p DatabaseProvisioner) Database() string {
	return dbcontroller.ResolveDatabase(p.Claim.Spec.Database, p.ProviderConfig)
}

// ConnectionStatus returns the desired connection status for the claim.
func (p DatabaseProvisioner) ConnectionStatus() infraApi.DatabaseConnectionStatus {
	return infraApi.DatabaseConnectionStatus{
		SecretRef: corev1.LocalObjectReference{Name: dbcontroller.SecretNameForDatabaseClaim(p.Claim)},
		Host:      p.ProviderConfig.Host,
		Port:      int32(p.ProviderConfig.Port),
		Database:  p.Database(),
	}
}

// Ensure validates the target database, provisions credentials if needed, and
// returns the desired Secret.
func (p DatabaseProvisioner) Ensure(ctx context.Context) (*corev1.Secret, error) {
	database := p.Database()
	dbExists, err := postgres.DatabaseExists(ctx, p.Postgres, database)
	if err != nil {
		return nil, dbcontroller.WrapQuickRetry("checking database existence", err)
	}
	if !dbExists {
		if p.Claim.Spec.Database == "" {
			return nil, claimerrors.DatabaseNotFound{Database: database}
		}
		if !dbcontroller.ExternalProviderAllows(p.Provider, infraApi.ExternalCapabilityCreateDatabase) {
			return nil, claimerrors.DatabaseCreateNotAllowed{Database: database}
		}
		if err := postgres.CreateDatabase(ctx, p.Postgres, database); err != nil {
			return nil, dbcontroller.WrapQuickRetry("creating database", err)
		}
		// Record creation only on this path so DeletionPolicy=Delete cannot
		// drop a pre-existing database the claim merely bound to.
		p.Claim.Status.DatabaseCreated = true
	}

	// Reject a foreign credentials Secret before role/extension work.
	// SchemaClaim "foo" and DatabaseClaim "sc-foo" both default to Secret "sc-foo".
	secret := &corev1.Secret{}
	secret.Name = dbcontroller.SecretNameForDatabaseClaim(p.Claim)
	secret.Namespace = p.Claim.Namespace
	if err := p.Client.Get(ctx, client.ObjectKeyFromObject(secret), secret); client.IgnoreNotFound(err) != nil {
		return nil, fmt.Errorf("checking credentials Secret existence: %w", err)
	}
	role := dbcontroller.RoleNameFor(p.Claim.Namespace, p.Claim.Name)
	if err := dbcontroller.RejectForeignCredentialsSecret(secret, p.Claim.UID, role); err != nil {
		return nil, err
	}

	// Extensions are per-database; only dial the target DB when required so
	// the common empty-extensions path keeps using the admin connection only.
	if len(p.Claim.Spec.Extensions) > 0 {
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
		dbcontroller.ApplyClaimCredentials(secret, p.ProviderConfig, role, pw, database, p.Claim.UID)

	case !secretExists:
		// Secret lost while role remains: rotate so Secret and role stay in sync.
		pw, err := postgres.GeneratePassword(24)
		if err != nil {
			return nil, fmt.Errorf("generating password: %w", err)
		}
		if err := postgres.SetRolePassword(ctx, p.Postgres, role, pw); err != nil {
			return nil, dbcontroller.WrapQuickRetry("rotating role password", err)
		}
		dbcontroller.ApplyClaimCredentials(secret, p.ProviderConfig, role, pw, database, p.Claim.UID)

	default:
		// Keep a verifiable password; rotate anything else: an empty
		// password cannot be recovered, and a stamp mismatch means the
		// stored content is no longer what this claim provisioned. An owned
		// Secret with no stamp is the upgrade-migration case, deliberately
		// preserved and stamped rather than rotated so upgrades do not churn
		// every claim's credentials; the accepted trade-off is that a Secret
		// recreated without annotations (e.g. a GitOps restore of a
		// pre-rotation version) also takes this path and keeps a stale
		// password — deleting the Secret heals that by rotation. The stamp
		// checks Secret-content consistency only; it cannot detect the
		// role's password changing out-of-band in PostgreSQL.
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
		dbcontroller.ApplyClaimCredentials(secret, p.ProviderConfig, role, existingPassword, database, p.Claim.UID)
	}

	switch ok, err := postgres.HasDatabasePrivileges(ctx, p.Postgres, database, role, access); {
	case err != nil:
		return nil, dbcontroller.WrapQuickRetry("checking database privileges", err)
	case !ok:
		if err := postgres.GrantDatabasePrivileges(ctx, p.Postgres, database, role, access); err != nil {
			return nil, dbcontroller.WrapQuickRetry("granting database privileges", err)
		}
	}
	secret.SetGroupVersionKind(gvk.Secret)

	return secret, nil
}
