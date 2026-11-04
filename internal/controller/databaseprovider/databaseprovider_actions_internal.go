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

package databaseprovider

import (
	"context"
	"errors"
	"fmt"
	"time"

	api "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	odherrors "github.com/opendatahub-io/odh-platform-utilities/framework/controller/actions/errors"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	odhtypes "github.com/opendatahub-io/odh-platform-utilities/framework/controller/types"
	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
	pginstance "github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres/instance"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (m *Controller) reconcileInternalAction(
	ctx context.Context,
	rr *odhtypes.ReconciliationRequest,
) error {
	obj, ok := rr.Instance.(*infraApi.DatabaseProvider)
	if !ok {
		return fmt.Errorf("instance is not a DatabaseProvider")
	}
	if obj.Spec.Type != infraApi.ProviderTypeInternal {
		return nil
	}
	rr.Conditions.Mark(infraApi.ConditionOperationalLimitations, metav1.ConditionTrue,
		conditions.WithSeverity(api.ConditionSeverityInfo),
		conditions.WithReason("SingleInstanceWithoutBackup"),
		conditions.WithMessage("The Internal provider is a single PostgreSQL instance without high availability or automated backup and restore. Use an appropriately managed External provider when those capabilities are required."))
	if internalTLSEnabled(obj) {
		available, err := dbcontroller.CertManagerAvailable(ctx, rr.Client)
		if err != nil {
			return fmt.Errorf("checking cert-manager availability: %w", err)
		}
		if !available {
			dbcontroller.MarkCertManagerMissing(rr)
			return nil
		}
	}

	tlsState, err := computeInternalAdminSecret(ctx, rr.Client, obj, m.cfg)
	obj.Status.TLS = internalTLSStatus(obj, m.cfg, tlsState.Ready)
	if err != nil {
		return m.reportInternalAdminSecretFailure(rr, obj, tlsState, err)
	}

	if err := rr.AddResources(tlsState.AdminSecret); err != nil {
		return fmt.Errorf("adding internal admin Secret to resources: %w", err)
	}

	// The deploy action has not persisted the Secret on the first reconcile.
	obj.Status.Connection = infraApi.ProviderConnectionStatus{
		Host:     string(tlsState.AdminSecret.Data[postgres.SecretKeyHost]),
		Port:     postgres.DefaultPort,
		Database: string(tlsState.AdminSecret.Data[postgres.SecretKeyDatabase]),
	}

	markInternalTLSConfiguration(rr, tlsState, nil)

	// Discover SCC once and share the result so Ensure and RoleBinding cannot
	// disagree within the same reconcile.
	sccAvailable, err := dbcontroller.SecurityContextConstraintsAvailable(rr.Client)
	if err != nil {
		return fmt.Errorf("checking SecurityContextConstraints API: %w", err)
	}
	// On OpenShift the install-time SCC must already exist before the
	// StatefulSet is admitted. The reconciler verifies; it does not create.
	if err := dbcontroller.EnsureInternalPostgresSCC(ctx, rr.Client, sccAvailable); err != nil {
		return fmt.Errorf("ensuring Internal Postgres SCC: %w", err)
	}

	return m.addInternalResources(ctx, rr, obj, tlsState, sccAvailable)
}

func (m *Controller) reportInternalAdminSecretFailure(
	rr *odhtypes.ReconciliationRequest,
	obj *infraApi.DatabaseProvider,
	tlsState TLSState,
	err error,
) error {
	obj.Status.Connection = infraApi.ProviderConnectionStatus{}
	markReachable(rr, obj, metav1.ConditionFalse,
		conditions.WithReason(reasonAdminSecretUnavailable),
		conditions.WithMessagef("%s", err.Error()))
	markInternalTLSConfiguration(rr, tlsState, err)

	if errors.Is(err, errInternalAdminCredentialsLost) {
		retryAfter := m.cfg.DatabaseProvider.RetryInterval
		if retryAfter <= 0 {
			retryAfter = moduleconfig.DefaultRetryInterval
		}
		return odherrors.NewStopErrorW(err).WithRequeueAfter(retryAfter)
	}
	return fmt.Errorf("ensuring internal admin Secret: %w", err)
}

func markInternalTLSConfiguration(rr *odhtypes.ReconciliationRequest, tlsState TLSState, err error) {
	if !tlsState.Enabled {
		rr.Conditions.Mark(ConditionTLSConfiguration, metav1.ConditionFalse,
			conditions.WithSeverity(api.ConditionSeverityInfo),
			conditions.WithReason(reasonTLSNotEnabled),
			conditions.WithMessage("TLS is not enabled for this internal provider"))
		return
	}
	if err != nil {
		rr.Conditions.Mark(ConditionTLSConfiguration, metav1.ConditionFalse,
			conditions.WithReason(reasonTLSProvisioning),
			conditions.WithMessagef("%s", err.Error()))
		return
	}
	if tlsState.Ready {
		rr.Conditions.Mark(ConditionTLSConfiguration, metav1.ConditionTrue,
			conditions.WithReason(reasonTLSConfigured),
			conditions.WithMessage("Internal provider TLS configuration resolved"))
		return
	}
	rr.Conditions.Mark(ConditionTLSConfiguration, metav1.ConditionFalse,
		conditions.WithReason(reasonTLSProvisioning),
		conditions.WithMessage("Internal provider TLS configuration is pending"))
}

func (m *Controller) addInternalResources(
	ctx context.Context,
	rr *odhtypes.ReconciliationRequest,
	obj *infraApi.DatabaseProvider,
	tlsState TLSState,
	sccAvailable bool,
) error {
	data, err := resolveInternalData(ctx, rr.Client, obj, m.cfg, tlsState)
	if err != nil {
		return fmt.Errorf("resolving internal resource data: %w", err)
	}

	pgres, err := pginstance.Resources(ctx, data)
	if err != nil {
		return fmt.Errorf("rendering internal resources: %w", err)
	}

	for i := range pgres {
		if tlsState.Enabled && !tlsState.Ready && pgres[i].GetKind() == "StatefulSet" {
			continue
		}
		if err := rr.AddResources(&pgres[i]); err != nil {
			return fmt.Errorf("adding internal resources: %w", err)
		}
	}

	if sccAvailable {
		binding := dbcontroller.DesiredInternalPostgresSCCRoleBinding(
			data.Namespace,
			data.ServiceAccountName,
			obj.Name,
		)
		if err := rr.AddResources(binding); err != nil {
			return fmt.Errorf("adding Internal Postgres SCC RoleBinding: %w", err)
		}
	}

	return nil
}

func (m *Controller) internalReadinessAction(
	ctx context.Context,
	rr *odhtypes.ReconciliationRequest,
) error {
	obj, ok := rr.Instance.(*infraApi.DatabaseProvider)
	if !ok {
		return fmt.Errorf("instance is not a DatabaseProvider")
	}
	if obj.Spec.Type != infraApi.ProviderTypeInternal {
		return nil
	}

	sts := &appsv1.StatefulSet{}
	sts.Namespace = dbcontroller.InternalNamespace(obj, m.cfg.OperatorNamespace)
	sts.Name = dbcontroller.InternalServiceName(obj.Name)

	if err := rr.Client.Get(ctx, client.ObjectKeyFromObject(sts), sts); err != nil {
		if apierrors.IsNotFound(err) {
			markReachable(rr, obj, metav1.ConditionFalse,
				conditions.WithReason(reasonProvisioning),
				conditions.WithMessage("Waiting for internal PostgreSQL resources"))
			return nil
		}
		return fmt.Errorf("reading internal StatefulSet: %w", err)
	}

	if sts.Status.ReadyReplicas != pginstance.InternalPostgresReplicaCount {
		markReachable(rr, obj, metav1.ConditionFalse,
			conditions.WithReason(reasonProvisioning),
			conditions.WithMessage("Waiting for internal PostgreSQL StatefulSet to become ready"))
		return nil
	}
	cfg, err := dbcontroller.LoadProviderConfig(ctx, rr.Client, obj, m.cfg.OperatorNamespace)
	if err != nil {
		markReachable(rr, obj, metav1.ConditionFalse,
			conditions.WithReason(reasonAdminSecretUnavailable),
			conditions.WithMessagef("Loading internal admin Secret: %s", err))
		return nil
	}
	cfg = m.cfg.WithPostgresTracing(cfg)
	if err := m.probeInternalAdminConnection(ctx, cfg); err != nil {
		markReachable(rr, obj, metav1.ConditionFalse,
			conditions.WithReason("AdminConnectionFailed"),
			conditions.WithMessagef(
				"Internal PostgreSQL admin connection failed: %s. Verify the admin Secret, TLS, and network access.",
				err,
			))
		return nil
	}

	markReachable(rr, obj, metav1.ConditionTrue,
		conditions.WithReason(reasonInstanceRunning),
		conditions.WithMessage("Internal PostgreSQL instance is ready"))

	return nil
}

func (m *Controller) probeInternalAdminConnection(ctx context.Context, cfg postgres.Config) error {
	factory := m.PostgresClientFactory
	if factory == nil {
		factory = postgres.DefaultClientFactory
	}
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pgClient, err := factory(probeCtx, cfg)
	if err != nil {
		return err
	}
	defer pgClient.Close()
	return pgClient.Ping(probeCtx)
}
