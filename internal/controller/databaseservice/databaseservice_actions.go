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

package databaseservice

import (
	"context"
	"fmt"
	"slices"
	"strings"

	common "github.com/opendatahub-io/odh-platform-utilities/api/common"
	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	odhtypes "github.com/opendatahub-io/odh-platform-utilities/framework/controller/types"

	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	servicesv1alpha1 "github.com/opendatahub-io/opendatahub-db-operator/api/services/v1alpha1"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	dbmetrics "github.com/opendatahub-io/opendatahub-db-operator/pkg/metrics"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	ConditionProviderHealth = "ProviderHealth"
	ConditionServerVersion  = "ServerVersion"
	ConditionClaimHealth    = "ClaimHealth"
)

func (m *Module) reportStatus(ctx context.Context, rr *odhtypes.ReconciliationRequest) error {
	obj, ok := rr.Instance.(*servicesv1alpha1.DatabaseService)
	if !ok {
		return fmt.Errorf("instance is not a DatabaseService")
	}
	obj.Status.Releases = []common.ComponentRelease{m.cfg.ComponentRelease()}
	if err := m.aggregateInputs(ctx, rr); err != nil {
		return err
	}
	return nil
}

func (m *Module) aggregateInputs(
	ctx context.Context,
	rr *odhtypes.ReconciliationRequest,
) error {
	providerHardFailures, err := m.aggregateProviders(ctx, rr)
	if err != nil {
		return err
	}
	claimHardFailures, err := m.aggregateClaims(ctx, rr)
	if err != nil {
		return err
	}

	markDegradedFromInputs(rr, providerHardFailures, claimHardFailures)
	return nil
}

// markDegradedFromInputs preserves pre-claim Degraded reasons when only
// providers contribute, and uses InputsUnhealthy only when claim failures are
// part of the rollup so existing consumers keep stable provider-only status.
func markDegradedFromInputs(rr *odhtypes.ReconciliationRequest, providerHardFailures, claimHardFailures []string) {
	if len(claimHardFailures) > 0 {
		message := strings.Join(append(append([]string{}, providerHardFailures...), claimHardFailures...), "; ")
		rr.Conditions.Mark(fwapi.ConditionTypeDegraded, metav1.ConditionTrue,
			conditions.WithReason("InputsUnhealthy"),
			conditions.WithMessage(message))
		return
	}
	if len(providerHardFailures) > 0 {
		message := strings.Join(providerHardFailures, "; ")
		rr.Conditions.Mark(fwapi.ConditionTypeDegraded, metav1.ConditionTrue,
			conditions.WithReason("ProviderUnreachable"),
			conditions.WithMessage(message))
		return
	}

	providerHealth := conditions.FindStatusCondition(rr.Instance, ConditionProviderHealth)
	if providerHealth != nil &&
		(providerHealth.Reason == "NoProvidersConfigured" || providerHealth.Reason == "DatabaseProviderCRDMissing") {
		rr.Conditions.Mark(fwapi.ConditionTypeDegraded, metav1.ConditionFalse,
			conditions.WithSeverity(fwapi.ConditionSeverityInfo),
			conditions.WithReason(providerHealth.Reason),
			conditions.WithMessage(providerHealth.Message))
		return
	}

	message := "All DatabaseProvider resources are reachable"
	if providerHealth != nil && providerHealth.Message != "" {
		message = providerHealth.Message
	}
	rr.Conditions.Mark(fwapi.ConditionTypeDegraded, metav1.ConditionFalse,
		conditions.WithReason("ProvidersReachable"),
		conditions.WithMessage(message))
}

func (m *Module) aggregateProviders(
	ctx context.Context,
	rr *odhtypes.ReconciliationRequest,
) ([]string, error) {
	providers := &infraApi.DatabaseProviderList{}
	if err := m.apiReader.List(ctx, providers); err != nil {
		if meta.IsNoMatchError(err) {
			message := "DatabaseProvider CRD is not installed; provider health is not available"
			rr.Conditions.Mark(ConditionProviderHealth, metav1.ConditionFalse,
				conditions.WithSeverity(fwapi.ConditionSeverityInfo),
				conditions.WithReason("DatabaseProviderCRDMissing"),
				conditions.WithMessage(message))
			rr.Conditions.Mark(ConditionServerVersion, metav1.ConditionUnknown,
				conditions.WithSeverity(fwapi.ConditionSeverityInfo),
				conditions.WithReason("DatabaseProviderCRDMissing"),
				conditions.WithMessage("DatabaseProvider CRD is not installed; server versions are not available"))
			dbmetrics.PruneProviderMetrics(nil)
			return nil, nil
		}
		return nil, fmt.Errorf("listing DatabaseProviders: %w", err)
	}
	slices.SortFunc(providers.Items, func(a, b infraApi.DatabaseProvider) int {
		return strings.Compare(a.Name, b.Name)
	})
	currentProviders := make([]string, 0, len(providers.Items))
	for _, provider := range providers.Items {
		currentProviders = append(currentProviders, provider.Name)
	}
	dbmetrics.PruneProviderMetrics(currentProviders)

	if len(providers.Items) == 0 {
		message := "No DatabaseProvider resources exist; create a provider to make this service operational"
		rr.Conditions.Mark(ConditionProviderHealth, metav1.ConditionFalse,
			conditions.WithSeverity(fwapi.ConditionSeverityInfo),
			conditions.WithReason("NoProvidersConfigured"),
			conditions.WithMessage(message))
		rr.Conditions.Mark(ConditionServerVersion, metav1.ConditionUnknown,
			conditions.WithSeverity(fwapi.ConditionSeverityInfo),
			conditions.WithReason("NoProvidersConfigured"),
			conditions.WithMessage("No DatabaseProvider resources exist; server versions are not available"))
		return nil, nil
	}

	unreachableMessages := make([]string, 0)
	outOfRangeMessages := make([]string, 0)
	for i := range providers.Items {
		provider := &providers.Items[i]
		reachable := conditions.FindStatusCondition(provider, "Reachable")
		if reachable == nil || reachable.Status != metav1.ConditionTrue {
			detail := "provider reachability is unknown"
			if reachable != nil && reachable.Message != "" {
				detail = reachable.Message
			}
			unreachableMessages = append(unreachableMessages, fmt.Sprintf(
				"DatabaseProvider %q is unreachable: %s. Check its connection settings.",
				provider.Name,
				detail,
			))
		}

		version := conditions.FindStatusCondition(provider, "ServerVersion")
		if version != nil && version.Status == metav1.ConditionFalse {
			detail := "server version is outside the tested range"
			if version.Message != "" {
				detail = version.Message
			}
			outOfRangeMessages = append(outOfRangeMessages, fmt.Sprintf("DatabaseProvider %q reports %s", provider.Name, detail))
		}
	}

	if len(unreachableMessages) > 0 {
		message := strings.Join(unreachableMessages, "; ")
		rr.Conditions.Mark(ConditionProviderHealth, metav1.ConditionFalse,
			conditions.WithReason("ProviderUnreachable"),
			conditions.WithMessage(message))
	} else {
		message := "All DatabaseProvider resources are reachable"
		rr.Conditions.Mark(ConditionProviderHealth, metav1.ConditionTrue,
			conditions.WithReason("ProvidersReachable"),
			conditions.WithMessage(message))
	}

	if len(outOfRangeMessages) > 0 {
		rr.Conditions.Mark(ConditionServerVersion, metav1.ConditionFalse,
			conditions.WithSeverity(fwapi.ConditionSeverityInfo),
			conditions.WithReason("ServerVersionOutsideTestedRange"),
			conditions.WithMessage(strings.Join(outOfRangeMessages, "; ")))
	} else {
		rr.Conditions.Mark(ConditionServerVersion, metav1.ConditionTrue,
			conditions.WithSeverity(fwapi.ConditionSeverityInfo),
			conditions.WithReason("ServerVersionsWithinTestedRange"),
			conditions.WithMessage("No DatabaseProvider reports a server version outside the tested range"))
	}

	return unreachableMessages, nil
}

func (m *Module) aggregateClaims(
	ctx context.Context,
	rr *odhtypes.ReconciliationRequest,
) ([]string, error) {
	databaseClaims := &infraApi.DatabaseClaimList{}
	schemaClaims := &infraApi.SchemaClaimList{}
	databaseCRDMissing := false
	schemaCRDMissing := false

	if err := m.apiReader.List(ctx, databaseClaims); err != nil {
		if !meta.IsNoMatchError(err) {
			return nil, fmt.Errorf("listing DatabaseClaims: %w", err)
		}
		databaseCRDMissing = true
	}
	if err := m.apiReader.List(ctx, schemaClaims); err != nil {
		if !meta.IsNoMatchError(err) {
			return nil, fmt.Errorf("listing SchemaClaims: %w", err)
		}
		schemaCRDMissing = true
	}

	if databaseCRDMissing && schemaCRDMissing {
		message := "DatabaseClaim and SchemaClaim CRDs are not installed; claim health is not available"
		rr.Conditions.Mark(ConditionClaimHealth, metav1.ConditionFalse,
			conditions.WithSeverity(fwapi.ConditionSeverityInfo),
			conditions.WithReason("ClaimCRDsMissing"),
			conditions.WithMessage(message))
		dbmetrics.PruneClaimMetrics(nil)
		return nil, nil
	}

	slices.SortFunc(databaseClaims.Items, func(a, b infraApi.DatabaseClaim) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})
	slices.SortFunc(schemaClaims.Items, func(a, b infraApi.SchemaClaim) int {
		return strings.Compare(a.Namespace+"/"+a.Name, b.Namespace+"/"+b.Name)
	})

	currentClaims := make([]dbmetrics.ClaimKey, 0, len(databaseClaims.Items)+len(schemaClaims.Items))
	failingMessages := make([]string, 0)
	pendingCount := 0
	for i := range databaseClaims.Items {
		claim := &databaseClaims.Items[i]
		currentClaims = append(currentClaims, dbmetrics.ClaimKey{
			Kind:      infraApi.DatabaseClaimKind,
			Namespace: claim.Namespace,
			Name:      claim.Name,
		})
		msg, failing, pending := classifyClaimProvisioned(
			infraApi.DatabaseClaimKind, claim.Namespace, claim.Name,
			conditions.FindStatusCondition(claim, dbcontroller.ConditionProvisioned),
		)
		if failing {
			failingMessages = append(failingMessages, msg)
		}
		if pending {
			pendingCount++
		}
	}
	for i := range schemaClaims.Items {
		claim := &schemaClaims.Items[i]
		currentClaims = append(currentClaims, dbmetrics.ClaimKey{
			Kind:      infraApi.SchemaClaimKind,
			Namespace: claim.Namespace,
			Name:      claim.Name,
		})
		msg, failing, pending := classifyClaimProvisioned(
			infraApi.SchemaClaimKind, claim.Namespace, claim.Name,
			conditions.FindStatusCondition(claim, dbcontroller.ConditionProvisioned),
		)
		if failing {
			failingMessages = append(failingMessages, msg)
		}
		if pending {
			pendingCount++
		}
	}
	dbmetrics.PruneClaimMetrics(currentClaims)

	if len(failingMessages) > 0 {
		message := strings.Join(failingMessages, "; ")
		rr.Conditions.Mark(ConditionClaimHealth, metav1.ConditionFalse,
			conditions.WithReason("ClaimsNotProvisioned"),
			conditions.WithMessage(message))
		return failingMessages, nil
	}

	if pendingCount > 0 {
		rr.Conditions.Mark(ConditionClaimHealth, metav1.ConditionUnknown,
			conditions.WithSeverity(fwapi.ConditionSeverityInfo),
			conditions.WithReason("ClaimsPending"),
			conditions.WithMessage(fmt.Sprintf(
				"Waiting for %d claim(s) to report Provisioned status", pendingCount)))
		return nil, nil
	}

	message := "All DatabaseClaim and SchemaClaim resources are provisioned"
	if databaseCRDMissing {
		message = "All SchemaClaim resources are provisioned; DatabaseClaim CRD is not installed"
	} else if schemaCRDMissing {
		message = "All DatabaseClaim resources are provisioned; SchemaClaim CRD is not installed"
	}
	rr.Conditions.Mark(ConditionClaimHealth, metav1.ConditionTrue,
		conditions.WithReason("ClaimsProvisioned"),
		conditions.WithMessage(message))
	return nil, nil
}

// classifyClaimProvisioned reports whether a claim is failing to bind, still
// pending first status, or neither. Missing Provisioned and ProviderBound are
// transient (not yet provisioned) and must not degrade DatabaseService.
func classifyClaimProvisioned(kind, namespace, name string, provisioned *fwapi.Condition) (string, bool, bool) {
	if provisioned == nil {
		return "", false, true
	}
	if provisioned.Status == metav1.ConditionTrue {
		return "", false, false
	}
	if provisioned.Reason == dbcontroller.ReasonProviderBound {
		return "", false, true
	}

	detail := provisioned.Message
	if detail == "" {
		detail = "claim is not provisioned"
	}
	return fmt.Sprintf(
		`%s "%s/%s" is not provisioned: %s. %s`,
		kind,
		namespace,
		name,
		detail,
		correctiveActionForClaim(provisioned),
	), true, false
}

func correctiveActionForClaim(provisioned *fwapi.Condition) string {
	switch provisioned.Reason {
	case dbcontroller.ReasonExtensionNotAvailable:
		return "Install the required extension in the target database or remove it from the claim spec."
	case dbcontroller.ReasonExtensionNotSupported:
		return "Remove unsupported extensions from the claim spec."
	case dbcontroller.ReasonProviderNotFound:
		return "Create a matching DatabaseProvider or fix the claim provider reference."
	case dbcontroller.ReasonProviderNotReady:
		return "Restore DatabaseProvider reachability, then wait for the claim to reprovision."
	case dbcontroller.ReasonDatabaseNotFound:
		return "Create the database on the provider or update the claim to an existing database."
	case dbcontroller.ReasonDatabaseCreateNotAllowed, dbcontroller.ReasonSchemaCreateNotAllowed:
		return "Enable the required create capability on the provider or change the claim request."
	case dbcontroller.ReasonSchemaReserved:
		return "Choose a non-reserved schema name on the claim."
	case dbcontroller.ReasonSchemaNotOwned, dbcontroller.ReasonSecretNotOwned:
		return "Resolve the ownership conflict for the existing schema or credentials Secret."
	default:
		return "Inspect the claim status and resolve the reported provisioning error."
	}
}
