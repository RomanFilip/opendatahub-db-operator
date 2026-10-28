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

// Package databaseservice registers the DatabaseService reconciler.
package databaseservice

import (
	"context"
	"time"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	fwapi "github.com/opendatahub-io/odh-platform-utilities/framework/api"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/conditions"
	"github.com/opendatahub-io/odh-platform-utilities/framework/controller/reconciler"
	infraApi "github.com/opendatahub-io/opendatahub-db-operator/api/infrastructure/v1alpha1"
	servicesv1alpha1 "github.com/opendatahub-io/opendatahub-db-operator/api/services/v1alpha1"
	moduleconfig "github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	dbcontroller "github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/resources/gvk"
)

// Module holds configuration shared by DatabaseService reconciliations.
type Module struct {
	cfg             *moduleconfig.Config
	platformRelease fwapi.Release
	apiReader       client.Reader
}

// NewModule captures cfg's platform release for reconciliation.
func NewModule(cfg *moduleconfig.Config) *Module {
	return &Module{
		cfg:             cfg,
		platformRelease: cfg.PlatformRelease(),
	}
}

// +kubebuilder:rbac:groups=services.platform.opendatahub.io,resources=databaseservices,verbs=get;list;watch;update
// +kubebuilder:rbac:groups=services.platform.opendatahub.io,resources=databaseservices/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=services.platform.opendatahub.io,resources=databaseservices/finalizers,verbs=update
// +kubebuilder:rbac:groups=infrastructure.opendatahub.io,resources=databaseproviders,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups=infrastructure.opendatahub.io,resources=schemaclaims;databaseclaims,verbs=list;watch
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims;secrets,verbs=get;patch

// Baseline RBAC required for all module operators. The CRD grant is
// deliberately narrower than a typical module operator's: this reconciler
// never creates, updates, or deletes CustomResourceDefinitions, so it only
// needs read access.
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;watch
// +kubebuilder:rbac:groups=authentication.k8s.io,resources=tokenreviews,verbs=create
// +kubebuilder:rbac:groups=authorization.k8s.io,resources=subjectaccessreviews,verbs=create
// +kubebuilder:rbac:urls=/metrics,verbs=get

// Framework events use events.k8s.io/v1, so the manager needs create and patch access for events.
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch

// NewReconciler registers the DatabaseService controller with mgr.
func NewReconciler(
	ctx context.Context,
	mgr ctrl.Manager,
	cfg *moduleconfig.Config,
) error {
	m := NewModule(cfg)
	m.apiReader = mgr.GetAPIReader()

	_, err := reconciler.ReconcilerFor(mgr, &servicesv1alpha1.DatabaseService{}).
		Watches(&infraApi.DatabaseProvider{},
			reconciler.Dynamic(reconciler.CrdExists(gvk.DatabaseProvider)),
			reconciler.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
			reconciler.WithEventMapper(dbcontroller.BroadcastListMapper(mgr.GetClient(), &servicesv1alpha1.DatabaseServiceList{}))).
		Watches(&infraApi.DatabaseClaim{},
			reconciler.Dynamic(reconciler.CrdExists(gvk.DatabaseClaim)),
			reconciler.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
			reconciler.WithEventMapper(dbcontroller.BroadcastListMapper(mgr.GetClient(), &servicesv1alpha1.DatabaseServiceList{}))).
		Watches(&infraApi.SchemaClaim{},
			reconciler.Dynamic(reconciler.CrdExists(gvk.SchemaClaim)),
			reconciler.WithPredicates(predicate.ResourceVersionChangedPredicate{}),
			reconciler.WithEventMapper(dbcontroller.BroadcastListMapper(mgr.GetClient(), &servicesv1alpha1.DatabaseServiceList{}))).
		WithReconcilerOpts(
			reconciler.WithRelease(m.platformRelease),
			reconciler.WithDefaultRequeueAfter(retryInterval(cfg)),
			reconciler.WithFinalizerName(defaultProviderCleanupFinalizer),
		).
		WithFinalizer(m.cleanupDefaultProvider).
		WithAction(m.reportStatus).
		WithAction(m.ensureDefaultProvider).
		WithConditions(conditions.Dependent(conditionDefaultProviderReady, conditions.HealthyWhenTrue)).
		Build(ctx)

	return err
}

// retryInterval returns the configured DatabaseService retry interval,
// falling back to the compiled default if it is unset (e.g. zero-value
// Config in tests).
func retryInterval(cfg *moduleconfig.Config) time.Duration {
	if cfg.DatabaseService.RetryInterval <= 0 {
		return moduleconfig.DefaultRetryInterval
	}
	return cfg.DatabaseService.RetryInterval
}
