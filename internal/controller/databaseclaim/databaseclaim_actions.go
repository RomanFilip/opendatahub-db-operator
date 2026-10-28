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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	odherrors "github.com/opendatahub-io/odh-platform-utilities/framework/controller/actions/errors"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	odhtypes "github.com/opendatahub-io/odh-platform-utilities/framework/controller/types"
	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/controller/claimerrors"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
)

// providerBindingRequeueAfter is a short pause after persisting status.provider
// so the next reconcile sees a durable binding before Ensure mutates Postgres.
const providerBindingRequeueAfter = time.Second

func (m *Controller) provisionAction(ctx context.Context, rr *odhtypes.ReconciliationRequest) error {
	obj, ok := rr.Instance.(*infraApi.DatabaseClaim)
	if !ok {
		return fmt.Errorf("instance is not a DatabaseClaim")
	}

	provider, err := dbcontroller.ResolveForCurrentOrDefault(ctx, rr.Client, obj.Spec.Provider, obj.Status.Provider)
	if err != nil {
		if notFound, ok := errors.AsType[dbcontroller.ErrNotFound](err); ok {
			rr.Conditions.Mark(ConditionProvisioned, metav1.ConditionFalse,
				conditions.WithError(notFound),
				conditions.WithReason("ProviderNotFound"))
			rr.Conditions.Mark(ConditionTLSConfiguration, metav1.ConditionUnknown,
				conditions.WithReason("ProviderNotFound"),
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
		rr.Conditions.Mark(ConditionProvisioned, metav1.ConditionFalse,
			conditions.WithReason("ProviderBound"),
			conditions.WithMessage(fmt.Sprintf("bound to DatabaseProvider %q; provisioning on next reconcile", provider.Name)))
		rr.Conditions.Mark(ConditionTLSConfiguration, metav1.ConditionUnknown,
			conditions.WithReason("ProviderBound"),
			conditions.WithMessage(fmt.Sprintf("waiting to provision against DatabaseProvider %q", provider.Name)))
		return odherrors.NewStopErrorW(fmt.Errorf("persisted provider binding %q", provider.Name)).
			WithRequeueAfter(providerBindingRequeueAfter)
	}

	if !dbcontroller.ProviderReachable(provider.GetConditions()) {
		rr.Conditions.Mark(ConditionProvisioned, metav1.ConditionFalse,
			conditions.WithReason("ProviderNotReady"),
			conditions.WithMessage(fmt.Sprintf("DatabaseProvider %q is not Reachable", provider.Name)))
		rr.Conditions.Mark(ConditionTLSConfiguration, metav1.ConditionUnknown,
			conditions.WithReason("ProviderNotReady"),
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
		rr.Conditions.Mark(ConditionProvisioned, metav1.ConditionFalse,
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

	provisioner := DatabaseProvisioner{
		Client:         rr.Client,
		Claim:          obj,
		Provider:       provider,
		Postgres:       pgClient,
		ProviderConfig: resolvedCfg,
	}
	secret, err := provisioner.Ensure(ctx)
	if notFound, ok := errors.AsType[claimerrors.DatabaseNotFound](err); ok {
		rr.Conditions.Mark(ConditionProvisioned, metav1.ConditionFalse,
			conditions.WithReason("DatabaseNotFound"),
			conditions.WithMessage(fmt.Sprintf("database %q does not exist on provider %q", notFound.Database, provider.Name)))
		return odherrors.NewStopErrorW(notFound).WithRequeueAfter(m.retryInterval())
	}
	if denied, ok := errors.AsType[claimerrors.DatabaseCreateNotAllowed](err); ok {
		rr.Conditions.Mark(ConditionProvisioned, metav1.ConditionFalse,
			conditions.WithReason("DatabaseCreateNotAllowed"),
			conditions.WithMessage(denied.Error()))
		// Terminal policy denial: do not polite-requeue; wait for a claim/provider change.
		return odherrors.NewStopErrorW(denied)
	}
	if err != nil {
		rr.Conditions.Mark(ConditionProvisioned, metav1.ConditionFalse,
			conditions.WithError(err))
		return err
	}

	if err := rr.AddResources(secret); err != nil {
		return fmt.Errorf("adding credentials Secret to resources: %w", err)
	}

	connection := provisioner.ConnectionStatus()
	obj.Status.Database = connection.Database
	obj.Status.Connection = connection
	rr.Conditions.Mark(ConditionProvisioned, metav1.ConditionTrue,
		conditions.WithReason("UserProvisioned"),
		conditions.WithMessage(fmt.Sprintf("User provisioned on database %q (provider %q)", connection.Database, provider.Name)))
	markTLS(rr, pgClient.Config())

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
	obj, ok := rr.Instance.(*infraApi.DatabaseClaim)
	if !ok {
		return fmt.Errorf("instance is not a DatabaseClaim")
	}

	role := dbcontroller.RoleNameFor(obj.Namespace, obj.Name)

	// Never drop the finalizer while cleanup fails. A timed grace release would
	// leave an enabled LOGIN role (and password) behind if PostgreSQL is down
	// or ownership cleanup keeps erroring. Keep returning errors so reconcile
	// retries until the role is gone. DropRole/DropDatabase terminate leftover
	// claim sessions (or FORCE the database drop) so open app connections do
	// not stick the finalizer.
	//
	// Cleanup must stay on the provider this claim bound to. Never re-resolve
	// via selector: a rebound provider could DropDatabase on the wrong backend
	// when DeletionPolicy=Delete.
	if obj.Status.Provider == "" {
		return nil
	}
	provider, err := dbcontroller.ResolveForCurrent(
		ctx,
		rr.Client,
		infraApi.ProviderRef{Name: obj.Status.Provider},
		"",
	)
	if err != nil {
		var notFound dbcontroller.ErrNotFound
		if errors.As(err, &notFound) {
			return nil
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

	database := obj.Status.Database
	if database == "" {
		database = dbcontroller.ResolveDatabase(obj.Spec.Database, providerCfg)
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
		if err := postgres.RevokeDatabasePrivileges(ctx, pgClient, database, role); err != nil {
			return fmt.Errorf("revoking privileges from role %q on database %q: %w", role, database, err)
		}
		if err := postgres.DropRole(ctx, pgClient, role); err != nil {
			return fmt.Errorf("dropping role %q: %w", role, err)
		}
	}

	others, err := dbcontroller.ListDatabaseClaims(ctx, rr.Client)
	if err != nil {
		return fmt.Errorf("listing DatabaseClaims before database drop: %w", err)
	}

	// Skip SchemaClaim listing when we would not drop anyway (e.g. Retain).
	if !dbcontroller.ShouldDropClaimDatabase(obj, providerCfg.DBName, others, nil) {
		return nil
	}
	schemaClaims, err := dbcontroller.ListSchemaClaims(ctx, rr.Client)
	if err != nil {
		if !schemaClaimAPIUnavailable(err) {
			return fmt.Errorf("listing SchemaClaims before database drop: %w", err)
		}
		// SchemaClaim CRD/API absent: nothing to protect.
	} else if !dbcontroller.ShouldDropClaimDatabase(obj, providerCfg.DBName, others, schemaClaims) {
		return nil
	}
	if err := postgres.DropDatabase(ctx, pgClient, database); err != nil {
		return fmt.Errorf("dropping database %q: %w", database, err)
	}
	return nil
}

func schemaClaimAPIUnavailable(err error) bool {
	return meta.IsNoMatchError(err) ||
		apierrors.IsNotFound(err) ||
		strings.Contains(err.Error(), "failed to get API group resources")
}
