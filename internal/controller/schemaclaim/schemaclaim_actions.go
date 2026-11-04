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
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	api "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	odherrors "github.com/opendatahub-io/odh-platform-utilities/framework/controller/actions/errors"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	odhtypes "github.com/opendatahub-io/odh-platform-utilities/framework/controller/types"
	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/controller/claimerrors"
	dbmetrics "github.com/opendatahub-io/opendatahub-db-operator/pkg/metrics"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
)

// providerBindingRequeueAfter is a short pause after persisting status.provider
// so the next reconcile sees a durable binding before Ensure mutates Postgres.
const providerBindingRequeueAfter = time.Second

func (m *Controller) provisionAction(ctx context.Context, rr *odhtypes.ReconciliationRequest) error {
	obj, ok := rr.Instance.(*infraApi.SchemaClaim)
	if !ok {
		return fmt.Errorf("instance is not a SchemaClaim")
	}

	provider, err := dbcontroller.ResolveForCurrentOrDefault(ctx, rr.Client, obj.Spec.Provider, obj.Status.Provider)
	if err != nil {
		if notFound, ok := errors.AsType[dbcontroller.ErrNotFound](err); ok {
			dbcontroller.MarkClaimProvisioned(rr, metav1.ConditionFalse,
				conditions.WithError(notFound),
				conditions.WithReason(dbcontroller.ReasonProviderNotFound))
			rr.Conditions.Mark(ConditionTLSConfiguration, metav1.ConditionUnknown,
				conditions.WithReason(dbcontroller.ReasonProviderNotFound),
				conditions.WithMessage(notFound.Error()))
			return odherrors.NewStopErrorW(err).WithRequeueAfter(m.retryInterval())
		}
		return fmt.Errorf("resolving provider: %w", err)
	}

	// Persist status.provider before any Postgres mutation. The framework
	// applies status only after actions return; if Ensure created a role and
	// the process died before status landed, cleanup would see an empty
	// provider and drop the finalizer without DropRole.
	if obj.Status.Provider != provider.Name {
		obj.Status.Provider = provider.Name
		dbcontroller.MarkClaimProvisioned(rr, metav1.ConditionFalse,
			conditions.WithReason(dbcontroller.ReasonProviderBound),
			conditions.WithMessage(fmt.Sprintf("bound to DatabaseProvider %q; provisioning on next reconcile", provider.Name)))
		rr.Conditions.Mark(ConditionTLSConfiguration, metav1.ConditionUnknown,
			conditions.WithReason(dbcontroller.ReasonProviderBound),
			conditions.WithMessage(fmt.Sprintf("waiting to provision against DatabaseProvider %q", provider.Name)))
		return odherrors.NewStopErrorW(fmt.Errorf("persisted provider binding %q", provider.Name)).
			WithRequeueAfter(providerBindingRequeueAfter)
	}

	if err := dbcontroller.StopIfUnsupportedClaimExtensions(rr, obj.Spec.Extensions); err != nil {
		return err
	}

	if !dbcontroller.ProviderReachable(provider.GetConditions()) {
		dbcontroller.MarkClaimProvisioned(rr, metav1.ConditionFalse,
			conditions.WithReason(dbcontroller.ReasonProviderNotReady),
			conditions.WithMessage(fmt.Sprintf("DatabaseProvider %q is not Reachable", provider.Name)))
		rr.Conditions.Mark(ConditionTLSConfiguration, metav1.ConditionUnknown,
			conditions.WithReason(dbcontroller.ReasonProviderNotReady),
			conditions.WithMessage(fmt.Sprintf("waiting for DatabaseProvider %q to become Reachable", provider.Name)))
		return odherrors.NewStopErrorW(fmt.Errorf("provider %q not reachable", provider.Name)).
			WithRequeueAfter(m.retryInterval())
	}

	pgClient, resolvedCfg, err := dbcontroller.NewClient(
		ctx,
		rr.Client,
		provider,
		m.cfg,
		m.PostgresClientFactory,
	)
	if err != nil {
		dbcontroller.MarkClaimProvisioned(rr, metav1.ConditionFalse,
			conditions.WithError(err))
		rr.Conditions.Mark(ConditionTLSConfiguration, metav1.ConditionUnknown,
			conditions.WithReason(dbcontroller.ReasonTLSProvisioning),
			conditions.WithMessage(err.Error()))
		if retryErr := dbcontroller.StopWithQuickRetryIfConnectionRefused(err); retryErr != nil {
			return retryErr
		}
		return odherrors.NewStopErrorW(err).WithRequeueAfter(m.retryInterval())
	}
	defer pgClient.Close()

	markTLS(rr, pgClient.Config())

	provisioner := SchemaProvisioner{
		Client:         rr.Client,
		Claim:          obj,
		Provider:       provider,
		Postgres:       pgClient,
		ProviderConfig: resolvedCfg,
		ClientFactory:  m.PostgresClientFactory,
	}
	secret, err := provisioner.Ensure(ctx)
	if notFound, ok := errors.AsType[claimerrors.DatabaseNotFound](err); ok {
		dbcontroller.MarkClaimProvisioned(rr, metav1.ConditionFalse,
			conditions.WithReason(dbcontroller.ReasonDatabaseNotFound),
			conditions.WithMessage(fmt.Sprintf("database %q does not exist on provider %q", notFound.Database, provider.Name)))
		return odherrors.NewStopErrorW(notFound).WithRequeueAfter(m.retryInterval())
	}
	if denied, ok := errors.AsType[claimerrors.SchemaCreateNotAllowed](err); ok {
		dbcontroller.MarkClaimProvisioned(rr, metav1.ConditionFalse,
			conditions.WithReason(dbcontroller.ReasonSchemaCreateNotAllowed),
			conditions.WithMessage(denied.Error()))
		// Terminal policy denial: do not polite-requeue; wait for a claim/provider change.
		return odherrors.NewStopErrorW(denied)
	}
	if reserved, ok := errors.AsType[claimerrors.SchemaReserved](err); ok {
		dbcontroller.MarkClaimProvisioned(rr, metav1.ConditionFalse,
			conditions.WithReason(dbcontroller.ReasonSchemaReserved),
			conditions.WithMessage(reserved.Error()))
		return odherrors.NewStopErrorW(reserved)
	}
	if notOwned, ok := errors.AsType[claimerrors.SchemaNotOwned](err); ok {
		dbcontroller.MarkClaimProvisioned(rr, metav1.ConditionFalse,
			conditions.WithReason(dbcontroller.ReasonSchemaNotOwned),
			conditions.WithMessage(notOwned.Error()))
		return odherrors.NewStopErrorW(notOwned)
	}
	if secretNotOwned, ok := errors.AsType[claimerrors.SecretNotOwned](err); ok {
		dbcontroller.MarkClaimProvisioned(rr, metav1.ConditionFalse,
			conditions.WithReason(dbcontroller.ReasonSecretNotOwned),
			conditions.WithMessage(secretNotOwned.Error()))
		return odherrors.NewStopErrorW(secretNotOwned)
	}
	if missingExt, ok := errors.AsType[claimerrors.ExtensionNotAvailable](err); ok {
		dbcontroller.MarkClaimProvisioned(rr, metav1.ConditionFalse,
			conditions.WithReason(dbcontroller.ReasonExtensionNotAvailable),
			conditions.WithMessage(missingExt.Error()))
		// Out-of-band install in the target DB; requeue like DatabaseNotFound.
		return odherrors.NewStopErrorW(missingExt).WithRequeueAfter(m.retryInterval())
	}
	if err != nil {
		dbcontroller.MarkClaimProvisioned(rr, metav1.ConditionFalse,
			conditions.WithError(err))
		return err
	}

	// The controller ownerRef makes GC collect the Secret after claim
	// deletion and routes Owns(Secret) drift events to this claim.
	if err := controllerutil.SetControllerReference(obj, secret, rr.Client.Scheme()); err != nil {
		return fmt.Errorf("setting credentials Secret owner reference: %w", err)
	}
	if err := rr.AddResources(secret); err != nil {
		return fmt.Errorf("adding credentials Secret to resources: %w", err)
	}

	connection := provisioner.ConnectionStatus()
	obj.Status.Schema = connection.Schema
	obj.Status.Connection = connection
	dbcontroller.MarkClaimProvisioned(rr, metav1.ConditionTrue,
		conditions.WithReason(dbcontroller.ReasonSchemaProvisioned),
		conditions.WithMessage(fmt.Sprintf("Schema %q provisioned on database %q (provider %q)", connection.Schema, connection.Database, provider.Name)))

	return nil
}

func markTLS(rr *odhtypes.ReconciliationRequest, cfg postgres.Config) {
	switch {
	case !cfg.TLSEnabled():
		rr.Conditions.Mark(ConditionTLSConfiguration, metav1.ConditionFalse,
			conditions.WithSeverity(api.ConditionSeverityInfo),
			conditions.WithReason(dbcontroller.ReasonTLSNotEnabled),
			conditions.WithMessage("TLS is not enabled"))
	case cfg.TLSReady():
		rr.Conditions.Mark(ConditionTLSConfiguration, metav1.ConditionTrue,
			conditions.WithReason(dbcontroller.ReasonTLSConfigured),
			conditions.WithMessage("TLS configuration is ready"))
	default:
		rr.Conditions.Mark(ConditionTLSConfiguration, metav1.ConditionFalse,
			conditions.WithReason(dbcontroller.ReasonTLSProvisioning),
			conditions.WithMessage("TLS configuration is pending"))
	}
}

func (m *Controller) cleanupAction(ctx context.Context, rr *odhtypes.ReconciliationRequest) error {
	obj, ok := rr.Instance.(*infraApi.SchemaClaim)
	if !ok {
		return fmt.Errorf("instance is not a SchemaClaim")
	}

	// Drop the series as soon as cleanup starts so metrics follow claim
	// deletion even when DatabaseService is absent and cannot prune.
	dbmetrics.DeleteClaimProvisioned(infraApi.SchemaClaimKind, obj.Namespace, obj.Name)

	role := dbcontroller.RoleNameForSchemaClaim(obj.Namespace, obj.Name)

	// Never drop the finalizer while cleanup fails. Keep returning errors so
	// reconcile retries until the role (and optional schema) are gone.
	//
	// Cleanup must stay on the provider this claim bound to. Never re-resolve
	// via selector: a rebound provider could DropSchema on the wrong backend
	// when DeletionPolicy=Delete.
	//
	// Do not rely on GC alone for the credentials Secret: deletion must
	// finish before the finalizer is removed, so delete explicitly once
	// Postgres cleanup is done or skipped.
	if obj.Status.Provider == "" {
		return deleteClaimCredentialsSecret(ctx, rr.Client, obj)
	}
	provider, err := dbcontroller.ResolveForCurrent(
		ctx,
		rr.Client,
		infraApi.ProviderRef{Name: obj.Status.Provider},
		"",
	)
	if err != nil {
		if _, ok := errors.AsType[dbcontroller.ErrNotFound](err); ok {
			return deleteClaimCredentialsSecret(ctx, rr.Client, obj)
		}
		return err
	}

	pgClient, providerCfg, err := dbcontroller.NewClient(
		ctx,
		rr.Client,
		provider,
		m.cfg,
		m.PostgresClientFactory,
	)
	if err != nil {
		// Do not return StopError here: that would allow finalizer removal
		// before cleanup finishes.
		return fmt.Errorf("opening postgres client for provider %q: %w", provider.Name, err)
	}
	defer pgClient.Close()

	database := obj.Status.Connection.Database
	if database == "" {
		database = dbcontroller.ResolveDatabase(obj.Spec.Database, providerCfg)
	}
	schema := obj.Status.Schema
	if schema == "" {
		schema = dbcontroller.SchemaNameFor(obj)
	}

	// The named database may already be gone (e.g. a DatabaseClaim deleted it).
	// Treat that as cleaned up for schema-scoped steps so the finalizer can
	// still drop the role and credentials Secret.
	dbExists, err := postgres.DatabaseExists(ctx, pgClient, database)
	if err != nil {
		return fmt.Errorf("checking database %q before cleanup: %w", database, err)
	}

	var dbClient postgres.Client
	if dbExists {
		dbCfg := providerCfg
		dbCfg.DBName = database
		dbClient, err = dbcontroller.OpenClient(ctx, dbCfg, m.PostgresClientFactory)
		if err != nil {
			if postgres.IsInvalidCatalogName(err) {
				dbExists = false
			} else {
				return fmt.Errorf("opening postgres client for database %q: %w", database, err)
			}
		} else {
			defer dbClient.Close()
		}
	}

	roleExists, err := postgres.RoleExists(ctx, pgClient, role)
	if err != nil {
		return fmt.Errorf("checking role %q existence: %w", role, err)
	}
	if roleExists {
		if err := dbcontroller.ClearRoleOwnershipInDatabase(
			ctx,
			pgClient,
			providerCfg,
			m.PostgresClientFactory,
			database,
			role,
		); err != nil {
			return fmt.Errorf("clearing owned objects for role %q in database %q: %w", role, database, err)
		}
		if dbExists {
			// dbExists does not imply the schema is still present (out-of-band
			// DROP SCHEMA). Skip revoke when it is already gone so the finalizer
			// can still drop the role and credentials Secret.
			schemaExists, err := postgres.SchemaExists(ctx, dbClient, schema)
			if err != nil {
				return fmt.Errorf("checking schema %q before cleanup: %w", schema, err)
			}
			if schemaExists {
				if err := postgres.RevokeSchemaPrivileges(ctx, dbClient, schema, role); err != nil {
					return fmt.Errorf("revoking schema privileges from role %q on schema %q: %w", role, schema, err)
				}
			}
		}
		if err := postgres.RevokeDatabasePrivileges(ctx, pgClient, database, role); err != nil {
			return fmt.Errorf("revoking privileges from role %q on database %q: %w", role, database, err)
		}
		if err := postgres.DropRole(ctx, pgClient, role); err != nil {
			return fmt.Errorf("dropping role %q: %w", role, err)
		}
	}

	if dbExists {
		others, err := dbcontroller.ListSchemaClaims(ctx, rr.Client)
		if err != nil {
			return fmt.Errorf("listing SchemaClaims before schema drop: %w", err)
		}
		if dbcontroller.ShouldDropClaimSchema(obj, providerCfg.DBName, others) {
			// schemaCreated means this claim created a schema with this name
			// earlier; the object currently at that name may be a replacement.
			// Drop only when the PostgreSQL ownership marker still matches.
			schemaExists, err := postgres.SchemaExists(ctx, dbClient, schema)
			if err != nil {
				return fmt.Errorf("checking schema %q before drop: %w", schema, err)
			}
			if schemaExists {
				ownerUID, err := postgres.SchemaOwnerClaimUID(ctx, dbClient, schema)
				if err != nil {
					return fmt.Errorf("reading schema ownership marker before drop: %w", err)
				}
				if ownerUID == string(obj.UID) {
					if err := postgres.DropSchemaCascade(ctx, dbClient, schema); err != nil {
						return fmt.Errorf("dropping schema %q: %w", schema, err)
					}
				}
			}
		}
	}

	return deleteClaimCredentialsSecret(ctx, rr.Client, obj)
}

// deleteClaimCredentialsSecret deletes the claim's Secret when this claim is
// its controller owner. Secret reads bypass the informer cache via the
// manager client's Cache.DisableFor (RHOAIENG-96275), so the ownership gate
// sees the live Secret even when it carries no managed label.
func deleteClaimCredentialsSecret(ctx context.Context, cli client.Client, claim *infraApi.SchemaClaim) error {
	key := client.ObjectKey{
		Name:      dbcontroller.SecretNameForSchemaClaim(claim),
		Namespace: claim.Namespace,
	}
	secret := &corev1.Secret{}
	if err := cli.Get(ctx, key, secret); err != nil {
		return client.IgnoreNotFound(err)
	}

	// Name alone is not enough: SchemaClaim "foo" and DatabaseClaim "sc-foo"
	// both default to Secret "sc-foo". Delete only when this claim owns the
	// Secret, or — when the ownerRef was stripped — when the credential stamp
	// still proves the content is this claim's. RejectForeignCredentialsSecret
	// shares the ownership rule with provisioning, so a Secret owned by
	// another controller is never deleted here, forged stamp or not.
	role := dbcontroller.RoleNameForSchemaClaim(claim.Namespace, claim.Name)
	if dbcontroller.RejectForeignCredentialsSecret(secret, claim.UID, role) != nil {
		return nil
	}

	if err := cli.Delete(ctx, secret); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting credentials Secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	return nil
}
